// heartbeat-collector — corre en gcp-east detrás de Tailscale Funnel.
// Recibe latidos POST de los nodos por internet plano (independiente del tailnet)
// y es un dead-man switch: si un nodo deja de reportar más de DeadAfter, alerta
// por la API directa de Telegram. Vive FUERA de entry (donde está todo el resto
// del alertado) para no ser el mismo SPOF.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Listen        string `json:"listen"`    // ej "127.0.0.1:9099" (Funnel apunta aquí)
	Token         string `json:"token"`     // compartido con los nodos
	StatePath     string `json:"statePath"` // ej "/var/lib/heartbeat/state.json"
	TelegramToken string `json:"telegramToken"`
	TelegramChat  string `json:"telegramChat"`
	DeadAfterSecs int    `json:"deadAfterSecs"` // silencio máximo antes de alertar
	RealertHours  int    `json:"realertHours"`
	// Notify: segunda via de aviso, a los aparatos (el R1 y los M5). El
	// servicio notify corre en ESTA MISMA maquina, asi que este camino no
	// pasa por entry ni por el cluster: sigue vivo justo cuando entry —lo
	// que mas hay que vigilar— esta muerta. Telegram es la via principal;
	// esta es la que suena en el bolsillo. Las dos son independientes: si
	// una falla, la otra sigue.
	NotifyURL     string   `json:"notifyURL"`     // ej "http://127.0.0.1:8090/v1/messages"
	NotifyToken   string   `json:"notifyToken"`   // token del cliente "heartbeat"
	NotifyTargets []string `json:"notifyTargets"` // ej ["r1","m5"]
	// Cada cuanto se revisa que los canales de aviso SIGAN pudiendo avisar
	// (ver canales.go). 0 = 300 s.
	CanalesCadaSegs int `json:"canalesCadaSegs,omitempty"`
	// Sondeos malos SEGUIDOS antes de dar un canal por roto cuando el fallo no
	// es firme (ver Salud en canales.go). 0 = 3.
	CanalesFallosSeguidos int      `json:"canalesFallosSeguidos,omitempty"`
	Expect                []string `json:"expect"` // nodos que SE esperan (alertar si nunca llegan)
	// Nodos MOVILES: se siguen (su last_seen sirve para diagnosticar) pero NUNCA
	// alertan al irse ni al volver. Un portatil que se cierra, el R1 en el
	// bolsillo o una tablet que se guarda no son incidentes: son lo normal.
	// Steven, 2026-09-11: "los nodos moviles son efimeros, son opcionales, que
	// no me alerte cuando un nodo movil se va".
	//
	// OJO: infra-watchdog (en entry) mantiene la MISMA lista bajo el nombre
	// "nodos_intermitentes". Son dos vigilantes distintos a proposito —este vive
	// fuera de entry para no compartir su SPOF— pero si las listas se separan,
	// uno callara y el otro no. Al tocar una, tocar la otra.
	Moviles   []string          `json:"moviles,omitempty"`
	Locations map[string]string `json:"locations,omitempty"`
}

// esMovil dice si un nodo esta exento de alertar por ausencia.
func esMovil(node string) bool {
	for _, m := range cfg.Moviles {
		if m == node {
			return true
		}
	}
	return false
}

type NodeState struct {
	LastSeen  int64 `json:"last_seen"`
	Uptime    int64 `json:"uptime"`
	Down      bool  `json:"down"`
	LastAlert int64 `json:"last_alert"`
	// MCL-199: ULTIMA memoria conocida del nodo, del ultimo /hb que trajo el
	// dato (clientes viejos que aun no manden estos campos no lo pisan con
	// cero: ver el guard en handleHB). Es lo que permite que la alerta del
	// dead-man diga "asi estaba de memoria justo antes de callarse" en vez de
	// solo "no contesta".
	MemUsedMB  int64 `json:"mem_used_mb,omitempty"`
	MemTotalMB int64 `json:"mem_total_mb,omitempty"`
	// Red del nodo tal y como ÉL la ve: SSID (o el nombre del sitio deducido),
	// IP de LAN y si va por wifi o cable. La IP pública de abajo dice por qué
	// salida se fue el paquete; esto dice a qué red está enganchado y con qué
	// dirección — que es lo que hace falta cuando en un mismo sitio hay dos
	// proveedores (Socorro) o cuando el nodo no está en el tailnet y su estado
	// no llega por gossip a nadie (rigby).
	// LanPeer/LanPeerOK: a quien vigila este nodo en su LAN y si lo ve. Lo
	// manda QUIEN MIRA, no el mirado, asi que sobrevive a que el mirado este
	// muerto — que es justo cuando hace falta.
	LanPeer      string `json:"lan_peer,omitempty"`
	LanPeerOK    bool   `json:"lan_peer_ok,omitempty"`
	LanPeerVisto int64  `json:"lan_peer_visto,omitempty"`
	SSID         string `json:"ssid,omitempty"`
	LocalIP      string `json:"local_ip,omitempty"`
	NetType      string `json:"net_type,omitempty"`
	// PublicIP: desde dónde salió el último latido. No lo manda el nodo —lo
	// ve el colector—, así que es el único dato de aquí que el emisor no
	// puede falsear sin cambiar de red de verdad. Sirve para ubicar al nodo
	// por prefijo cuando su sistema le esconde el SSID.
	PublicIP string `json:"public_ip,omitempty"`
	// TSIP: dirección del nodo en el tailnet (CGNAT 100.x). Lo manda el nodo
	// porque un host puede estar vivo con Tailscale backend Stopped y aquí
	// se queda "" — señal clara de "el tailnet se cayó aunque el host vive".
	TSIP string `json:"ts_ip,omitempty"`
	// TailscaleState: BackendState devuelto por `tailscale status --json`
	// (Running / Stopped / NeedsLogin / NoState / Starting). Vacío si el nodo
	// no tiene Tailscale instalado. Permite distinguir "no reporta porque la
	// máquina se cayó" de "no reporta porque Tailscale backend está Stopped
	// pero la máquina está viva en la LAN". MCL postmortem mini 9/10-oct-2026.
	TailscaleState string `json:"tailscale_state,omitempty"`
	// LimaVMs: "nombre:estado[,nombre:estado…]" de todas las VMs Lima del
	// host (hoy mini y lia). Alerta cuando una VM queda Broken aunque el
	// host siga pulsando — gap que dejó al descubierto el incidente mini
	// del 9-oct-2026.
	LimaVMs string `json:"lima_vms,omitempty"`
	// MAC: MAC de la interfaz default. Puede cambiar entre reboots (Private
	// WiFi Address, randomización); el latido la refresca para que el rescate
	// por LAN no empiece con un dato obsoleto de una nota desactualizada.
	MAC string `json:"mac,omitempty"`
	// Supervisión activa (Phase 2, 2026-10-13): cuando el nodo SÍ reporta pero
	// supervisionDegradada(ns) devuelve no vacío, arrancamos un reloj y alertamos
	// pasados 180 s si sigue degradado. Permite capturar "la máquina vive pero
	// el servicio adentro está roto" que el dead-man clásico nunca ve.
	DegradedSince        int64 `json:"degraded_since,omitempty"`
	Degraded             bool  `json:"degraded,omitempty"`
	LastSupervisionAlert int64 `json:"last_supervision_alert,omitempty"`
}
// Pulso: un latido registrado, con vía por la que llegó. Se guarda poco (solo
// las ultimas 4 h) para poder dibujar "cuantos pulsos por nodo en el rato
// reciente" sin pedirle a nadie que mire logs.
type Pulso struct {
	Ts  int64  `json:"ts"`
	Via string `json:"via"` // "tailnet" o "funnel"
}

const pulsosVentanaSegs = 4 * 3600

type State struct {
	mu    sync.Mutex
	Nodes map[string]*NodeState `json:"nodes"`
	// Canales: estado de cada via de aviso (ver canales.go). Se persiste con el
	// resto del estado para que un reinicio del colector no borre que un canal
	// llevaba roto: si se olvidara, la recuperacion pasaria muda y un canal
	// roto volveria a anunciarse como nuevo en cada arranque.
	Canales map[string]*EstadoCanal `json:"canales,omitempty"`
	// Mantenimiento: nodo -> epoch hasta el que su silencio esta ANUNCIADO.
	// Se guarda en el estado (no en la config) para que sobreviva a un
	// reinicio del colector: una parada anunciada no puede volverse alerta
	// porque el vigilante se reinicio en medio.
	Mantenimiento map[string]int64 `json:"mantenimiento,omitempty"`
	// Pulsos: ventana deslizante de los ultimos [pulsosVentanaSegs] s por nodo.
	// Sobrevive a reinicios porque esta en el estado persistido. 11 nodos a
	// 60 s = ~2640 entradas, trivial.
	Pulsos map[string][]Pulso `json:"pulsos,omitempty"`
}

// clasificarVia dice por donde llego un POST segun la IP origen. Las IPs CGNAT
// de Tailscale son 100.64.0.0/10 (y fd7a:115c:a1e0::/48 en IPv6). Todo lo
// demas vino por el Funnel desde internet.
func clasificarVia(ip string) string {
	if strings.HasPrefix(ip, "100.") {
		if parsed := net.ParseIP(ip); parsed != nil {
			if b := parsed.To4(); b != nil && b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
				return "tailnet"
			}
		}
	}
	if strings.HasPrefix(ip, "fd7a:115c:a1e0") {
		return "tailnet"
	}
	return "funnel"
}

// enMantenimiento dice si el silencio del nodo esta anunciado. El que llama
// DEBE tener state.mu tomado.
func enMantenimiento(node string) bool {
	hasta, ok := state.Mantenimiento[node]
	return ok && time.Now().Unix() < hasta
}

var (
	cfg   Config
	state = &State{Nodes: map[string]*NodeState{}}
)

func loadState() {
	b, err := os.ReadFile(cfg.StatePath)
	if err == nil {
		json.Unmarshal(b, state)
	}
	if state.Nodes == nil {
		state.Nodes = map[string]*NodeState{}
	}
}
func saveState() {
	state.mu.Lock()
	defer state.mu.Unlock()
	b, _ := json.MarshalIndent(state, "", "  ")
	os.WriteFile(cfg.StatePath, b, 0600)
}

// avisar manda el MISMO texto por todas las vias configuradas. Ninguna depende
// de la otra a proposito: Telegram necesita internet hacia api.telegram.org,
// notify necesita el broker local; que una se caiga no puede dejar mudo al
// unico vigilante que sobrevive a la caida de entry.
func avisar(text string) {
	telegram(text)
	for _, destino := range cfg.NotifyTargets {
		notificar(destino, text)
	}
}

// notificar publica el aviso en notify (el R1 y los M5), que corre en esta
// misma maquina. El R1 es "live only": si esta apagado el mensaje se pierde,
// por eso los M5 —que si encolan— valen la pena como segundo destino.
func notificar(destino, text string) {
	if cfg.NotifyURL == "" || cfg.NotifyToken == "" {
		return
	}
	// notify rechaza con 413 cualquier texto de mas de 280 caracteres. Se
	// recorta aqui para que un mensaje largo llegue cortado en vez de no
	// llegar.
	if r := []rune(text); len(r) > 280 {
		text = string(r[:277]) + "..."
	}
	cuerpo, _ := json.Marshal(map[string]string{"target": destino, "text": text, "source": "heartbeat"})
	req, err := http.NewRequest("POST", cfg.NotifyURL, bytes.NewReader(cuerpo))
	if err != nil {
		log.Printf("notify %s: no se pudo armar la peticion: %v", destino, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.NotifyToken)
	cl := &http.Client{Timeout: 15 * time.Second}
	r, err := cl.Do(req)
	if err != nil {
		log.Printf("notify %s: fallo el envio: %v", destino, err)
		return
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		resp, _ := io.ReadAll(io.LimitReader(r.Body, 300))
		log.Printf("notify %s: respondio %d: %s", destino, r.StatusCode, strings.TrimSpace(string(resp)))
		return
	}
	log.Printf("notify %s: enviado", destino)
}

func telegram(text string) {
	if cfg.TelegramToken == "" {
		log.Printf("Telegram: sin token configurado, aviso NO enviado: %s", text)
		return
	}
	form := url.Values{}
	form.Set("chat_id", cfg.TelegramChat)
	form.Set("text", text)
	cl := clienteTelegram()
	r, err := cl.PostForm("https://api.telegram.org/bot"+cfg.TelegramToken+"/sendMessage", form)
	if err != nil {
		// sinToken y no "%v" a secas: el error de http trae la URL, y la URL
		// trae el token (ver canales.go).
		log.Printf("Telegram: fallo el envio: %s", sinToken(err.Error()))
		return
	}
	defer r.Body.Close()
	// El motivo real viene en el CUERPO ("chat not found", "bot was blocked"),
	// no en el codigo. Tirarlo, como se hacia antes, deja al ultimo vigilante
	// que queda fallando en silencio: se cree que aviso y nadie recibio nada.
	if r.StatusCode != http.StatusOK {
		resp, _ := io.ReadAll(io.LimitReader(r.Body, 300))
		log.Printf("Telegram: respondio %d: %s", r.StatusCode, strings.TrimSpace(string(resp)))
		return
	}
	log.Printf("Telegram: enviado")
}

// POST /hb  node=X token=... uptime=...
func handleHB(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if r.FormValue("token") != cfg.Token {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	node := r.FormValue("node")
	if node == "" {
		http.Error(w, "no node", http.StatusBadRequest)
		return
	}
	var up int64
	fmt.Sscan(r.FormValue("uptime"), &up)
	// MCL-199: memoria del emisor, si la manda (clientes viejos no la
	// mandan; se guarda 0 y el guard de abajo la deja tal cual).
	var memUsed, memTotal int64
	fmt.Sscan(r.FormValue("mem_used_mb"), &memUsed)
	fmt.Sscan(r.FormValue("mem_total_mb"), &memTotal)
	now := time.Now().Unix()
	state.mu.Lock()
	ns := state.Nodes[node]
	if ns == nil {
		ns = &NodeState{}
		state.Nodes[node] = ns
	}
	wasDown := ns.Down
	// Si volvio dentro de su ventana de mantenimiento, la ventana se cierra
	// aqui: ya reporta, no hay nada que silenciar. Y se avisa —una parada
	// anunciada que nadie confirma que termino es igual de ciega que una
	// caida sin alerta.
	volvioDeMantenimiento := false
	if state.Mantenimiento != nil && enMantenimiento(node) {
		volvioDeMantenimiento = true
		delete(state.Mantenimiento, node)
	}
	ns.LastSeen, ns.Uptime, ns.Down = now, up, false
	ns.PublicIP = ipDeOrigen(r)
	// Solo se pisa si el latido trae memoria de verdad: un binario viejo sin
	// estos campos no debe borrar el ultimo dato bueno que ya se tenia.
	if memTotal > 0 {
		ns.MemUsedMB, ns.MemTotalMB = memUsed, memTotal
	}
	// Misma regla para la red: vacio NO pisa. Asi conviven binarios viejos y
	// nuevos, y un nodo que momentaneamente no sepa su SSID (wifi reasociando)
	// no borra el ultimo que si se supo.
	if v := r.FormValue("lan_peer"); v != "" {
		ns.LanPeer = v
		ns.LanPeerOK = r.FormValue("lan_peer_ok") == "1"
		ns.LanPeerVisto = now
	}
	if v := r.FormValue("ssid"); v != "" {
		ns.SSID = v
	}
	if v := r.FormValue("local_ip"); v != "" {
		ns.LocalIP = v
	}
	if v := r.FormValue("net_type"); v != "" {
		ns.NetType = v
	}
	// Campos de supervisión añadidos tras el postmortem mini 9/10-oct-2026.
	// Misma regla: vacío NO pisa, así los binarios viejos y nuevos conviven.
	if v := r.FormValue("ts_ip"); v != "" {
		ns.TSIP = v
	}
	if v := r.FormValue("tailscale_state"); v != "" {
		ns.TailscaleState = v
	}
	if v := r.FormValue("lima_vms"); v != "" {
		ns.LimaVMs = v
	}
	if v := r.FormValue("mac"); v != "" {
		ns.MAC = v
	}
	// Pulsos: anotar y recortar los que caigan fuera de la ventana. Hecho
	// bajo el mismo lock que la actualizacion del nodo para que /pulsos nunca
	// vea una mitad coherente y la otra no.
	if state.Pulsos == nil {
		state.Pulsos = map[string][]Pulso{}
	}
	via := clasificarVia(ns.PublicIP)
	ps := append(state.Pulsos[node], Pulso{Ts: now, Via: via})
	cutoff := now - pulsosVentanaSegs
	i := 0
	for i < len(ps) && ps[i].Ts < cutoff {
		i++
	}
	state.Pulsos[node] = ps[i:]
	state.mu.Unlock()
	switch {
	case volvioDeMantenimiento:
		saveState()
		avisar(fmt.Sprintf("✅ heartbeat: %s volvió del mantenimiento (uptime %d min).", node, up/60))
	case wasDown:
		avisar(fmt.Sprintf("✅ heartbeat: %s volvió a reportar (por internet).", node))
	}
	// Se responde con la IP desde la que llegó el latido. El nodo no tiene
	// otra forma de saber su IP pública sin preguntarle a un tercero, y le
	// hace falta para ubicarse cuando el sistema le esconde el SSID (macOS
	// sin Localización) o la tabla ARP (Android). Aquí sale gratis: el
	// paquete ya está entrando por internet plano.
	fmt.Fprintln(w, "ok", ipDeOrigen(r))
}

// ipDeOrigen saca la IP real del emisor. Detrás del Funnel de Tailscale la
// conexión llega de localhost, así que la buena es la de X-Forwarded-For; se
// coge la PRIMERA de la lista, que es el cliente, y solo se acepta si es una
// IP válida.
func ipDeOrigen(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		primera := strings.TrimSpace(strings.Split(xff, ",")[0])
		if net.ParseIP(primera) != nil {
			return primera
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if net.ParseIP(host) != nil {
		return host
	}
	return ""
}

// handleMantenimiento anuncia una parada: POST /mantenimiento node=entry mins=30
// token=... . Mientras dure, el silencio de ese nodo NO alerta; al volver, si
// avisa. `mins=0` la cancela. Existe para que apagar un nodo a proposito no
// obligue a elegir entre recibir ruido o apagar el vigilante y olvidarse de
// volver a encenderlo.
func handleMantenimiento(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if r.FormValue("token") != cfg.Token {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	node := r.FormValue("node")
	if node == "" {
		http.Error(w, "no node", http.StatusBadRequest)
		return
	}
	var mins int64
	fmt.Sscan(r.FormValue("mins"), &mins)
	// Tope de 12 h: un mantenimiento "para siempre" es un vigilante apagado
	// con otro nombre, y eso es exactamente lo que no se quiere.
	if mins > 720 {
		mins = 720
	}
	state.mu.Lock()
	if state.Mantenimiento == nil {
		state.Mantenimiento = map[string]int64{}
	}
	if mins <= 0 {
		delete(state.Mantenimiento, node)
	} else {
		state.Mantenimiento[node] = time.Now().Unix() + mins*60
	}
	state.mu.Unlock()
	saveState()
	if mins <= 0 {
		avisar(fmt.Sprintf("🔧 heartbeat: %s sale de mantenimiento, vuelve a vigilarse.", node))
		fmt.Fprintln(w, "ok, vigilando", node)
		return
	}
	avisar(fmt.Sprintf("🔧 heartbeat: %s en mantenimiento %d min — no alerto por su silencio, pero aviso cuando vuelva.", node, mins))
	fmt.Fprintf(w, "ok, %s en mantenimiento %d min\n", node, mins)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now().Unix()
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{}
	for n, ns := range state.Nodes {
		fila := map[string]any{"last_seen_ago_s": now - ns.LastSeen, "down": ns.Down, "uptime_s": ns.Uptime, "movil": esMovil(n), "mem_used_mb": ns.MemUsedMB, "mem_total_mb": ns.MemTotalMB}
		// Campos opcionales: solo salen si el nodo los ha mandado alguna vez,
		// para que un cliente viejo no aparezca con cadenas vacias.
		for k, v := range map[string]string{"ssid": ns.SSID, "local_ip": ns.LocalIP, "net_type": ns.NetType, "public_ip": ns.PublicIP} {
			if v != "" {
				fila[k] = v
			}
		}
		if ns.LanPeer != "" {
			fila["vigila_en_lan"] = ns.LanPeer
			fila["vigila_en_lan_ok"] = ns.LanPeerOK
		}
		if hasta, ok := state.Mantenimiento[n]; ok && now < hasta {
			fila["mantenimiento_min_restantes"] = (hasta - now + 59) / 60
		}
		out[n] = fila
	}
	// Los nodos siguen colgando de la raiz, como siempre: cualquier script
	// suelto que lea `.entry.last_seen_ago_s` no se entera del cambio. Lo
	// nuevo va bajo una clave con guion bajo, que ningun nodo puede llamarse.
	out["_canales"] = state.Canales
	json.NewEncoder(w).Encode(out)
}

// handlePulsos devuelve las ultimas 4 h de latidos por nodo. Devuelve crudos
// (ts epoch + via) para que el cliente los agrupe como le convenga. Un query
// opcional `?desde=<epoch>` recorta a lo posterior.
func handlePulsos(w http.ResponseWriter, r *http.Request) {
	desde := int64(0)
	fmt.Sscan(r.URL.Query().Get("desde"), &desde)
	state.mu.Lock()
	defer state.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{
		"ventana_segs": pulsosVentanaSegs,
		"ahora":        time.Now().Unix(),
		"nodos":        map[string][]Pulso{},
	}
	por_nodo := out["nodos"].(map[string][]Pulso)
	for n, ps := range state.Pulsos {
		if desde == 0 {
			por_nodo[n] = ps
			continue
		}
		i := 0
		for i < len(ps) && ps[i].Ts < desde {
			i++
		}
		por_nodo[n] = ps[i:]
	}
	json.NewEncoder(w).Encode(out)
}

// memInfoSuffix arma el trozo diagnostico del mensaje de dead-man a partir de
// la ULTIMA memoria conocida del nodo (no hay otra: si el nodo no contesta al
// latido, tampoco va a contestar a un /metrics en vivo). Vacio si nunca llego
// memoria (cliente viejo, o nodo movil sin este campo) — MCL-199.
// redInfoSuffix dice en que red estaba el nodo la ultima vez que hablo. En un
// dead-man eso es la mitad del diagnostico: "se cayo" y "se fue a la otra red"
// se parecen desde fuera, y en Socorro —dos proveedores en la misma casa— es
// la diferencia entre ir a encender una maquina o no moverse del sitio.
// testigoSuffix busca si ALGUIEN mas dice ver a este nodo en su LAN. Es la
// mitad del diagnostico que el dead-man nunca tuvo: "no reporta" se parece
// demasiado a "esta apagado", y no son lo mismo.
//
// El que llama DEBE tener state.mu tomado (recorre state.Nodes).
func testigoSuffix(node string) string {
	for otro, ns := range state.Nodes {
		if otro == node || ns.LanPeer != node {
			continue
		}
		// Un testigo que lleva callado tanto como el vigilado no prueba nada.
		if time.Now().Unix()-ns.LastSeen > 600 {
			continue
		}
		if ns.LanPeerOK {
			return fmt.Sprintf(" PERO %s SI lo ve en su LAN: encendido y sin internet, no hace falta ir.", otro)
		}
		return fmt.Sprintf(" Y %s TAMPOCO lo ve en su LAN (ni por ARP): apagado de verdad.", otro)
	}
	return ""
}

func redInfoSuffix(ns *NodeState) string {
	partes := []string{}
	if ns.SSID != "" {
		partes = append(partes, ns.SSID)
	}
	if ns.LocalIP != "" {
		partes = append(partes, ns.LocalIP)
	}
	if ns.MAC != "" {
		// MAC junto a la IP local: eso es lo que necesita quien busca al
		// nodo por LAN cuando pierde tailnet. Antes había que ir a la nota
		// del nodo y rezar que estuviera al día; ahora el latido la refresca.
		partes = append(partes, "mac "+ns.MAC)
	}
	if ns.PublicIP != "" {
		partes = append(partes, "salida "+ns.PublicIP)
	}
	if ns.TSIP != "" {
		partes = append(partes, "tailnet "+ns.TSIP)
	}
	if len(partes) == 0 {
		return ""
	}
	return fmt.Sprintf(" Última red conocida: %s.", strings.Join(partes, " · "))
}

// supervisionDegradada arma "tailscale=X · lima=k3s:Broken,…" si alguna pieza
// supervisada no está Running. Vacío = todo en orden (o binario viejo que no
// manda esos campos). Lo usan supervisionSuffix (para pegarlo al dead-man) y
// el loop de supervisión activa (para decidir si abrir incidente de "vivo pero
// roto adentro").
func supervisionDegradada(ns *NodeState) string {
	partes := []string{}
	if ns.TailscaleState != "" && ns.TailscaleState != "Running" {
		partes = append(partes, "tailscale="+ns.TailscaleState)
	}
	if ns.LimaVMs != "" {
		anomalas := []string{}
		for _, vm := range strings.Split(ns.LimaVMs, ",") {
			if _, estado, ok := strings.Cut(vm, ":"); ok && estado != "Running" {
				anomalas = append(anomalas, vm)
			}
		}
		if len(anomalas) > 0 {
			partes = append(partes, "lima="+strings.Join(anomalas, ","))
		}
	}
	return strings.Join(partes, " · ")
}

// supervisionSuffix dice en qué estado estaban las piezas que un host-vivo
// con backend-caído deja invisibles al dead-man clásico: Tailscale backend y
// VMs Lima. Vacío si no hay dato (binarios viejos). Se concatena a la alerta
// de dead-man y a la de recuperación — la causa posible cuenta más que el
// hecho de que no reporte.
func supervisionSuffix(ns *NodeState) string {
	d := supervisionDegradada(ns)
	if d == "" {
		return ""
	}
	return fmt.Sprintf(" Supervisión al callarse: %s.", d)
}

func memInfoSuffix(ns *NodeState) string {
	if ns.MemTotalMB <= 0 {
		return ""
	}
	pct := 100 * ns.MemUsedMB / ns.MemTotalMB
	return fmt.Sprintf(" Última memoria conocida: %d/%d MB (%d%%).", ns.MemUsedMB, ns.MemTotalMB, pct)
}

func deadManLoop() {
	for {
		time.Sleep(60 * time.Second)
		now := time.Now().Unix()
		realert := int64(cfg.RealertHours) * 3600
		state.mu.Lock()
		for _, node := range cfg.Expect {
			ns := state.Nodes[node]
			if ns == nil {
				// nunca reportó desde que arrancó el colector: aún no alertamos
				continue
			}
			if esMovil(node) {
				// Se sigue registrando su last_seen, pero irse no es un incidente.
				// Ademas se limpia Down para que al volver no dispare el "✅ volvió
				// a reportar" de handleHB por un estado anterior a esta exencion.
				ns.Down = false
				continue
			}
			if enMantenimiento(node) {
				// Parada anunciada por Steven: no se alerta por su silencio.
				// Tampoco se marca Down, para que al volver el aviso sea el
				// de "volvió del mantenimiento" y no un falso "volvió a
				// reportar" de algo que nunca se reporto como caido.
				continue
			}
			silent := now - ns.LastSeen
			if silent > int64(cfg.DeadAfterSecs) {
				if !ns.Down || now-ns.LastAlert >= realert {
					mins := silent / 60
					// MCL-199: la memoria de justo antes de callarse, para que
					// la alerta diagnostique en vez de solo constatar.
					avisar(fmt.Sprintf("🔴 heartbeat: %s no reporta hace %d min (posible caída o sin internet).%s%s%s%s", node, mins, testigoSuffix(node), redInfoSuffix(ns), supervisionSuffix(ns), memInfoSuffix(ns)))
					ns.Down = true
					ns.LastAlert = now
				}
				// Nodo callado: no evaluamos supervisión activa — el dead-man ya
				// está cantando y el supervisionSuffix de arriba lleva la causa.
				// Mantenemos DegradedSince para que al volver, si ya era un
				// degraded abierto, no se pierda la continuidad.
				continue
			}
			// Phase 2 (2026-10-13): supervisión activa. El host SÍ reporta, pero
			// algo adentro puede estar caído (tailscale Stopped, Lima VM Broken).
			// Esperamos 180 s con el reloj para no ladrar en reinicios normales.
			if deg := supervisionDegradada(ns); deg != "" {
				if ns.DegradedSince == 0 {
					ns.DegradedSince = now
				}
				if now-ns.DegradedSince >= 180 && (!ns.Degraded || now-ns.LastSupervisionAlert >= realert) {
					mins := (now - ns.DegradedSince) / 60
					avisar(fmt.Sprintf("🟠 heartbeat: %s vivo pero supervisión degradada hace %d min: %s.%s", node, mins, deg, redInfoSuffix(ns)))
					ns.Degraded = true
					ns.LastSupervisionAlert = now
				}
			} else if ns.Degraded {
				// Ya volvió todo a Running: cerramos el incidente.
				avisar(fmt.Sprintf("✅ heartbeat: %s recuperó supervisión (tailscale/lima en Running).", node))
				ns.Degraded = false
				ns.DegradedSince = 0
				ns.LastSupervisionAlert = 0
			} else if ns.DegradedSince != 0 {
				// Degradado se vio pero nunca llegó a alertar (duró < 180 s).
				// Reset silencioso del reloj.
				ns.DegradedSince = 0
			}
		}
		state.mu.Unlock()
		saveState()
	}
}

func main() {
	cfgPath := flag.String("config", "/etc/heartbeat/config.json", "config")
	flag.Parse()
	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.DeadAfterSecs == 0 {
		// 3 min. Antes eran 10, y por eso una caida de entry de 8 minutos
		// (resize del 2026-09-18) no genero UNA sola alerta: el unico
		// vigilante que sobrevive a entry no llegaba a mirar a tiempo.
		cfg.DeadAfterSecs = 180
	}
	if cfg.RealertHours == 0 {
		cfg.RealertHours = 8
	}
	loadState()
	// Un aviso al arrancar de lo que YA esta roto: si el colector se levanta
	// sin poder avisar, eso es lo primero que hay que saber.
	if s := saludTelegram(); !s.OK {
		log.Printf("ARRANQUE: el canal telegram no puede avisar: %s", s.Motivo)
	}
	if s := saludNotify(); !s.OK {
		log.Printf("ARRANQUE: el canal notify no puede avisar: %s", s.Motivo)
	}
	go deadManLoop()
	go vigilarCanales()
	http.HandleFunc("/hb", handleHB)
	// /ip devuelve la IP desde la que se ve al que pregunta, y nada más. Es
	// para que un nodo sepa su IP pública sin depender de un servicio ajeno:
	// esto ya es infraestructura propia y está publicada por Funnel. Sin
	// token a propósito — no cuenta nada que el que pregunta no sepa ya.
	http.HandleFunc("/ip", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, ipDeOrigen(r))
	})
	http.HandleFunc("/status", handleStatus)
	http.HandleFunc("/pulsos", handlePulsos)
	http.HandleFunc("/mantenimiento", handleMantenimiento)
	log.Printf("heartbeat-collector escuchando en %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, nil))
}
