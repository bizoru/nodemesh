package main

import (
	"sync"
)

// ---------- alcanzabilidad cruzada ----------
//
// El estado de un nodo se derivaba SOLO de la edad de su cadena: online bajo
// 16 min, stale bajo 45, offline más allá. Eso hace invisible cualquier caída
// más corta que el umbral — el 2026-09-07 steven-mini estuvo 6 min apagado por
// un corte de luz (uptime de 13 días a 31 s en la cadena) y nodemesh lo reportó
// "online" todo el tiempo, porque el hueco cabía entre dos heartbeats.
//
// Aquí las observaciones directas (ping de LAN, baliza BLE) dejan de ser
// decorado del dashboard y pasan a corregir ese estado en las dos direcciones:
// adelantan el veredicto de caída cuando el hermano de LAN confirma que no
// responde, y lo IMPIDEN cuando alguien tiene prueba de vida. Un nodo solo se
// declara caído cuando el tiempo y una observación directa dicen lo mismo.
//
// Nada de esto entra en la cadena. Son observaciones del observador, no hechos
// sobre el sujeto: van como campo vivo con su timestamp, igual que los Specs.

const (
	staleAfterSecs   = 16 * 60 // heartbeat cada 10 min + hasta ~4 min de gossip
	offlineAfterSecs = 45 * 60
)

// Estados. "isolated" es el que añade el cross-check: la máquina está
// encendida (la radio la ve) pero no la alcanza ninguna red. Antes ese caso
// se reportaba como "offline", que es justo lo contrario de lo que pasa.
const (
	stateOnline   = "online"
	stateStale    = "stale"
	stateOffline  = "offline"
	stateIsolated = "isolated"
)

// OverlayCheck: ¿contestó el nodo la última vez que le hablamos por el overlay?
//
// Es la señal más fuerte que hay y estaba tirándose a la basura: gossipOnce
// hacía `continue` en silencio cuando un peer no respondía. Un GET a
// /api/nodes que devuelve datos es prueba de vida inmediata, mientras que la
// cadena puede llevar diez minutos quieta en un nodo perfectamente sano.
// Since dice desde cuándo dura el resultado actual, por el mismo motivo que en
// LANCheck: lo que importa no es el último intento, sino cuánto lleva así.
type OverlayCheck struct {
	OK    bool   `json:"ok"`
	At    int64  `json:"at"`
	Since int64  `json:"since,omitempty"`
	By    string `json:"by,omitempty"`
}

// reachability agrupa lo que se sabe de un sujeto por vías ajenas a su propia
// cadena. Cualquiera puede faltar: la mayoría de los nodos no tiene hermano de
// LAN ni baliza, y de un nodo que nadie tiene como peer no hay contacto.
type reachability struct {
	Overlay *OverlayCheck
	LAN     *LANCheck
	BLE     *BLECheck
}

// stateByAge es la regla de siempre, intacta: cuánto hace que el sujeto no
// extiende su cadena.
func stateByAge(age int64) string {
	switch {
	case age > offlineAfterSecs:
		return stateOffline
	case age > staleAfterSecs:
		return stateStale
	}
	return stateOnline
}

// crossCheckedState corrige stateByAge con las observaciones directas.
//
// La caída NO se juzga por la edad de la cadena. La cadena solo crece en
// cambios de estado y en el heartbeat de 10 min, así que un nodo sano puede
// pasar minutos enteros sin escribir: "hace rato que no reporta" no distingue
// un nodo apagado de uno callado, y usarlo como condición marcaba offline
// nodos vivos. Lo que sí es una señal rápida y limpia es el ping, que corre
// cada CollectSecs: se exige que lleve fallando al menos grace (varios ciclos
// seguidos, no un blip) y que la cadena no haya avanzado desde que empezó a
// fallar — si el sujeto escribió DESPUÉS, está vivo y el que se equivoca es
// el ping.
//
// observerOnline responde si el nodo que hizo la observación está él mismo
// online. Es la salvaguarda contra el falso positivo obvio: si al observador se
// le cae el WiFi, su ping al vecino falla aunque el vecino esté perfecto, y sin
// este filtro el observador arrastraría al vecino a "offline" al reportar su
// propia avería como si fuera ajena. Una observación de un observador caído no vale nada.
//
// obsTTL descarta la observación rancia — un negativo de hace horas no puede
// seguir declarando caído a un nodo indefinidamente.
func crossCheckedState(byAge string, headTS int64, r reachability, now, grace, obsTTL int64,
	observerOnline func(string) bool) string {

	fresh := func(ts int64, by string) bool {
		if ts == 0 || now-ts > obsTTL {
			return false
		}
		// by == "" es una observación de primera mano: el observador es quien
		// está respondiendo, así que por definición está vivo.
		return by == "" || observerOnline(by)
	}

	overlayOK := r.Overlay != nil && fresh(r.Overlay.At, r.Overlay.By) && r.Overlay.OK
	overlayDown := r.Overlay != nil && fresh(r.Overlay.At, r.Overlay.By) &&
		!r.Overlay.OK && now-r.Overlay.Since >= grace

	lanSeen := r.LAN != nil && fresh(r.LAN.CheckedAt, r.LAN.By)
	lanUp := lanSeen && r.LAN.Reachable
	lanDown := lanSeen && !r.LAN.Reachable && now-r.LAN.Since >= grace
	// La baliza prueba VIDA, no red: LastSeen reciente significa que la radio
	// del sujeto sigue anunciándose. PeerStatus (ok|sin_internet) es la visión
	// del vecino sobre SU propio internet y no dice nada sobre si el sujeto
	// está encendido, así que aquí no se mira.
	bleAlive := r.BLE != nil && fresh(r.BLE.LastSeen, r.BLE.By)

	// Le acabamos de hablar y contestó. No hay nada que discutir: que su cadena
	// lleve rato quieta es cosa suya, no una caída.
	if overlayOK {
		return stateOnline
	}
	// Responde en la LAN: vivo, solo que fuera del overlay. Es la promesa
	// original del cross-check — "off the overlay, but answering on the LAN".
	if lanUp {
		if byAge == stateOffline {
			return stateStale
		}
		return byAge
	}

	// Ninguna vía de red llega, sostenidamente y por dos caminos independientes.
	// Aquí sí se puede declarar la caída sin esperar los 45 min.
	if overlayDown && lanDown {
		if bleAlive {
			return stateIsolated // encendido, pero incomunicado
		}
		return stateOffline
	}

	// Una sola vía negativa no basta: puede ser la ruta, el mDNS o el permiso
	// de Red Local del observador, no el sujeto. Se cae al veredicto por
	// tiempo, que es el comportamiento conservador de siempre.
	if bleAlive && byAge == stateOffline {
		return stateIsolated
	}
	return byAge
}

// ---------- relevo de observaciones por gossip ----------
//
// El ping de LAN lo hace un solo nodo —el vecino de LAN del sujeto—, así que
// hasta ahora solo él podía contar esa historia: quien preguntara a entry o
// athena seguía viendo el veredicto por tiempo. Se relevan de segunda mano por la misma razón que los
// Specs y con el mismo mecanismo: llevan timestamp propio (CheckedAt/LastSeen),
// así que el receptor siempre puede quedarse con la copia más nueva y tirar la
// rancia. Sin ese timestamp el relevo acabaría sirviendo un negativo viejo
// para siempre — que es justo por lo que las versiones NO se relevan.

var peerChecks = struct {
	mu      sync.RWMutex
	overlay map[string]OverlayCheck
	lan     map[string]LANCheck
	ble     map[string]BLECheck
}{overlay: map[string]OverlayCheck{}, lan: map[string]LANCheck{}, ble: map[string]BLECheck{}}

// setOverlayCheck registra el resultado de hablarle a un nodo por el overlay.
// Solo de primera mano: el contacto de OTRO nodo con un tercero no se relaya,
// porque "yo no lo alcanzo" y "nadie lo alcanza" son cosas distintas y mezclarlas
// convertiría un problema de ruta ajeno en una caída del sujeto.
func setOverlayCheck(node, observer string, ok bool, now int64) {
	peerChecks.mu.Lock()
	defer peerChecks.mu.Unlock()
	c := OverlayCheck{OK: ok, At: now, Since: now, By: observer}
	if cur, exists := peerChecks.overlay[node]; exists && cur.OK == ok && cur.Since > 0 {
		c.Since = cur.Since // mismo resultado: se conserva desde cuándo dura
	}
	peerChecks.overlay[node] = c
}

// setPeerChecks guarda lo que un peer observó sobre un tercero. observer es el
// nodo que respondió el gossip: se graba como autor para que crossCheckedState
// pueda exigir después que ESE nodo estuviera online.
func setPeerChecks(node, observer string, lan *LANCheck, ble *BLECheck) {
	peerChecks.mu.Lock()
	defer peerChecks.mu.Unlock()
	if lan != nil && lan.CheckedAt > 0 {
		if cur, ok := peerChecks.lan[node]; !ok || lan.CheckedAt > cur.CheckedAt {
			c := *lan
			if c.By == "" {
				c.By = observer
			}
			peerChecks.lan[node] = c
		}
	}
	if ble != nil && ble.LastSeen > 0 {
		if cur, ok := peerChecks.ble[node]; !ok || ble.LastSeen > cur.LastSeen {
			c := *ble
			if c.By == "" {
				c.By = observer
			}
			peerChecks.ble[node] = c
		}
	}
}

func getPeerChecks(node string) reachability {
	peerChecks.mu.RLock()
	defer peerChecks.mu.RUnlock()
	var r reachability
	if c, ok := peerChecks.overlay[node]; ok {
		r.Overlay = &c
	}
	if c, ok := peerChecks.lan[node]; ok {
		r.LAN = &c
	}
	if c, ok := peerChecks.ble[node]; ok {
		r.BLE = &c
	}
	return r
}

// ---------- nombre del peer por IP ----------
//
// El contacto se registra por NOMBRE de nodo, pero cuando el peer no responde
// no hay respuesta de la que sacarlo. Se cachea el nombre visto la última vez
// que sí contestó: sin esto, la señal más útil (el fallo) sería justo la que
// no se puede atribuir a nadie.

var peerNames = struct {
	mu sync.RWMutex
	m  map[string]string
}{m: map[string]string{}}

func rememberPeerName(ip, node string) {
	if ip == "" || node == "" {
		return
	}
	peerNames.mu.Lock()
	peerNames.m[ip] = node
	peerNames.mu.Unlock()
}

func peerNameForIP(ip string) string {
	peerNames.mu.RLock()
	defer peerNames.mu.RUnlock()
	return peerNames.m[ip]
}
