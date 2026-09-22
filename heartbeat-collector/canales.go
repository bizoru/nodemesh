package main

// Vigilancia de los CANALES de aviso, no de los nodos.
//
// El 2026-09-18 entry estuvo 8 minutos apagada y no salio una sola alerta. La
// causa de fondo no fue el umbral: era que `telegramToken` llevaba vacio desde
// el 11-sep por lo menos, y el envio se rendia en silencio. El colector se
// marcaba `last_alert`, se quedaba tan tranquilo, y nadie —ni el— sabia que
// llevaba una semana disparando al vacio.
//
// La leccion no es "revisar el token": es que un vigilante tiene que vigilar
// TAMBIEN su propia capacidad de avisar, y que el fallo de un canal se anuncia
// por el OTRO. Aqui: si Telegram no responde se avisa por los aparatos, y si
// notify esta degradado se avisa por Telegram. Si fallan los dos, queda en el
// journal y `/status` lo expone, que es lo unico que se puede hacer sin
// inventarse un tercer canal.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// EstadoCanal es lo que se sabe de un canal de salida y desde cuando.
type EstadoCanal struct {
	OK          bool   `json:"ok"`
	Motivo      string `json:"motivo,omitempty"`
	Desde       int64  `json:"desde,omitempty"`        // epoch del ultimo cambio de estado
	UltimoAviso int64  `json:"ultimo_aviso,omitempty"` // para no repetir antes de realert
	Fallos      int    `json:"fallos,omitempty"`       // sondeos malos seguidos, ver Salud.Firme
}

// Salud es el resultado de UNA comprobacion de un canal.
//
// Firme separa las dos maneras de estar roto, que no se parecen en nada:
//
//   - Firme: el token esta vacio o revocado, el chat no existe, notify contesta
//     "degraded". Eso no se arregla solo, asi que se denuncia en el primer
//     sondeo — que es justo para lo que se puso esta vigilancia.
//   - Pasajero: la API no contesto a tiempo. gcp-east sale SOLO por IPv6 y a
//     api.telegram.org le da hipo: el 2026-09-18 hubo cuatro baches de un unico
//     sondeo, y cada uno solto su "🔴 no puede avisar" y, cinco minutos despues,
//     su "✅ vuelve a funcionar". Ocho mensajes que no decian nada de nada, y en
//     la pantalla del R1 veinte minutos de cola que retrasaron seis el aviso de
//     que Abby habia salido de clase. Un vigilante que cuenta baches de red no
//     esta vigilando: esta haciendo ruido. Para estos hacen falta
//     [fallosSeguidosParaAvisar] sondeos malos SEGUIDOS.
type Salud struct {
	OK     bool
	Motivo string
	Firme  bool
}

// sinToken quita el token de cualquier texto antes de registrarlo. Go mete la
// URL COMPLETA en los errores de http (*url.Error), y la URL de la API de
// Telegram lleva el token dentro: registrar el error tal cual lo publica en el
// journal. Paso el 2026-09-18, en la primera prueba de este mismo fichero.
func sinToken(s string) string {
	if cfg.TelegramToken == "" {
		return s
	}
	return strings.ReplaceAll(s, cfg.TelegramToken, "<token>")
}

// saludTelegram comprueba que se PUEDE enviar, sin enviar nada: getMe valida el
// token contra la API real y no le escribe a nadie. Es la comprobacion que le
// faltaba al colector — con ella, un token vacio o revocado se nota en 5
// minutos en vez de en una semana.
func saludTelegram() Salud {
	if cfg.TelegramToken == "" {
		return Salud{Motivo: "no hay token configurado (telegramToken vacio)", Firme: true}
	}
	if cfg.TelegramChat == "" {
		return Salud{Motivo: "no hay chat configurado (telegramChat vacio)", Firme: true}
	}
	cl := &http.Client{Timeout: 15 * time.Second}
	r, err := cl.Get("https://api.telegram.org/bot" + cfg.TelegramToken + "/getMe")
	if err != nil {
		// Un timeout o un DNS que no resuelve es un bache hasta que se repita.
		return Salud{Motivo: sinToken(fmt.Sprintf("no se alcanza la API: %v", err))}
	}
	defer r.Body.Close()
	cuerpo, _ := io.ReadAll(io.LimitReader(r.Body, 500))
	if r.StatusCode != http.StatusOK {
		// 401 = token malo o revocado, que es justo lo que hay que cazar.
		var d struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(cuerpo, &d)
		motivo := strings.TrimSpace(d.Description)
		if motivo == "" {
			motivo = fmt.Sprintf("respondio %d", r.StatusCode)
		}
		// Que la API conteste y diga que no, es firme: el token o el chat
		// estan mal y manana seguiran estandolo. Un 429 o un 5xx son suyos,
		// se le pasan solos, y entran por la puerta de los baches.
		firme := r.StatusCode < 500 && r.StatusCode != http.StatusTooManyRequests
		return Salud{Motivo: motivo, Firme: firme}
	}
	return Salud{OK: true}
}

// saludNotify pregunta al servicio notify por su propia salud. "degraded"
// cuenta como fallo: el 2026-09-12 el transporte MQTT quedo sin poder
// autenticarse y los M5 dejaron de recibir sin que nadie se enterara — el
// servicio respondia, pero a medias.
func saludNotify() Salud {
	url := urlSaludNotify()
	if url == "" {
		return Salud{Motivo: "no hay notifyURL configurada", Firme: true}
	}
	cl := &http.Client{Timeout: 15 * time.Second}
	r, err := cl.Get(url)
	if err != nil {
		// Puede ser un redespliegue de notify en marcha; se confirma antes de
		// contarlo.
		return Salud{Motivo: fmt.Sprintf("no responde: %v", err)}
	}
	defer r.Body.Close()
	cuerpo, _ := io.ReadAll(io.LimitReader(r.Body, 500))
	var d map[string]string
	_ = json.Unmarshal(cuerpo, &d)
	estado := d["status"]
	if r.StatusCode == http.StatusOK && estado == "ok" {
		return Salud{OK: true}
	}
	// Se nombra la pieza rota (mqtt/nats), que es lo que hace falta para
	// arreglarlo sin ponerse a investigar desde cero.
	var rotas []string
	for k, v := range d {
		if k != "status" && v != "" && v != "connected" {
			rotas = append(rotas, fmt.Sprintf("%s=%s", k, v))
		}
	}
	motivo := estado
	if motivo == "" {
		motivo = fmt.Sprintf("respondio %d", r.StatusCode)
	}
	if len(rotas) > 0 {
		motivo += " (" + strings.Join(rotas, ", ") + ")"
	}
	// Contesto y dijo que esta mal: eso es una averia, no un bache.
	return Salud{Motivo: motivo, Firme: true}
}

// urlSaludNotify deriva /v1/health de la URL de envio, para no tener que
// configurar dos veces lo mismo (y no poder equivocarse en una).
func urlSaludNotify() string {
	if cfg.NotifyURL == "" {
		return ""
	}
	return strings.TrimSuffix(cfg.NotifyURL, "/v1/messages") + "/v1/health"
}

// revisarCanal aplica el mismo criterio que el resto de la flota: avisa al
// romperse, NO repite antes de realertHours, y SIEMPRE avisa al recuperarse
// diciendo cuanto duro. `avisarPor` es el canal por el que se cuenta — nunca el
// que esta roto.
//
// Con una salvedad, desde el 2026-09-18: un fallo PASAJERO (ver [Salud]) no
// rompe el canal hasta repetirse [fallosSeguidosParaAvisar] sondeos seguidos.
// Mientras tanto no se marca roto, y por eso tampoco habra despues un "vuelve
// a funcionar" que contar: un bache de red no genera ni un mensaje.
func revisarCanal(nombre string, s Salud, ahora int64, avisarPor func(string)) {
	if state.Canales == nil {
		state.Canales = map[string]*EstadoCanal{}
	}
	est := state.Canales[nombre]
	if est == nil {
		est = &EstadoCanal{OK: true, Desde: ahora}
		state.Canales[nombre] = est
	}
	realert := int64(cfg.RealertHours) * 3600
	if s.OK {
		est.Fallos = 0
		if !est.OK {
			mins := (ahora - est.Desde) / 60
			avisarPor(fmt.Sprintf("✅ canal %s: vuelve a funcionar (estuvo %d min sin poder avisar).", nombre, mins))
			est.OK, est.Desde, est.Motivo, est.UltimoAviso = true, ahora, "", 0
		}
		return
	}
	est.Fallos++
	if est.OK {
		if !s.Firme && est.Fallos < fallosSeguidosParaAvisar() {
			// Queda en el journal y en /status: no se pierde, simplemente no
			// se le despierta a nadie por un bache.
			log.Printf("canal %s: sondeo malo %d/%d (%s), todavia no lo denuncio",
				nombre, est.Fallos, fallosSeguidosParaAvisar(), s.Motivo)
			est.Motivo = s.Motivo
			return
		}
		est.OK, est.Desde, est.Motivo = false, ahora, s.Motivo
		est.UltimoAviso = ahora
		aguante := ""
		if !s.Firme {
			aguante = fmt.Sprintf(" (%d sondeos seguidos)", est.Fallos)
		}
		avisarPor(fmt.Sprintf("🔴 canal %s NO puede avisar%s: %s. Las alertas que salgan por ahi se estan perdiendo.", nombre, aguante, s.Motivo))
		return
	}
	est.Motivo = s.Motivo
	if ahora-est.UltimoAviso >= realert {
		mins := (ahora - est.Desde) / 60
		est.UltimoAviso = ahora
		avisarPor(fmt.Sprintf("🔴 canal %s sigue sin poder avisar (%d min): %s.", nombre, mins, s.Motivo))
	}
}

// vigilarCanales corre en paralelo al dead-man. Ojo con el orden: cada canal se
// denuncia por el otro, nunca por si mismo.
func vigilarCanales() {
	for {
		ahora := time.Now().Unix()

		saludTG := saludTelegram()
		saludNot := saludNotify()

		state.mu.Lock()
		// Telegram roto -> se cuenta por los aparatos.
		revisarCanal("telegram", saludTG, ahora, func(texto string) {
			if !saludNot.OK {
				log.Printf("canal telegram roto y notify tambien: %s", texto)
				return
			}
			for _, destino := range cfg.NotifyTargets {
				notificar(destino, texto)
			}
		})
		// notify roto -> se cuenta por Telegram.
		revisarCanal("notify", saludNot, ahora, func(texto string) {
			if !saludTG.OK {
				log.Printf("canal notify roto y telegram tambien: %s", texto)
				return
			}
			telegram(texto)
		})
		state.mu.Unlock()
		saveState()

		time.Sleep(time.Duration(canalesCadaSegs()) * time.Second)
	}
}

// fallosSeguidosParaAvisar: cuantos sondeos malos seguidos hacen falta para
// dar un canal por roto cuando el fallo no es firme. Con el sondeo de 5 min
// por defecto, tres son un cuarto de hora sin poder avisar — lo bastante corto
// para enterarse de una averia de verdad y lo bastante largo para que los
// baches de IPv6 de gcp-east no se oigan.
func fallosSeguidosParaAvisar() int {
	if cfg.CanalesFallosSeguidos > 0 {
		return cfg.CanalesFallosSeguidos
	}
	return 3
}

func canalesCadaSegs() int {
	if cfg.CanalesCadaSegs > 0 {
		return cfg.CanalesCadaSegs
	}
	return 300 // 5 min
}
