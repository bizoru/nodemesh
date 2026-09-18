// heartbeat-collector — corre en gcp-east detrás de Tailscale Funnel.
// Recibe latidos POST de los nodos por internet plano (independiente del tailnet)
// y es un dead-man switch: si un nodo deja de reportar más de DeadAfter, alerta
// por la API directa de Telegram. Vive FUERA de entry (donde está todo el resto
// del alertado) para no ser el mismo SPOF.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
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
	Listen        string   `json:"listen"`    // ej "127.0.0.1:9099" (Funnel apunta aquí)
	Token         string   `json:"token"`     // compartido con los nodos
	StatePath     string   `json:"statePath"` // ej "/var/lib/heartbeat/state.json"
	TelegramToken string   `json:"telegramToken"`
	TelegramChat  string   `json:"telegramChat"`
	DeadAfterSecs int      `json:"deadAfterSecs"` // silencio máximo antes de alertar
	RealertHours  int      `json:"realertHours"`
	Expect        []string `json:"expect"` // nodos que SE esperan (alertar si nunca llegan)
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
	// PublicIP: desde dónde salió el último latido. No lo manda el nodo —lo
	// ve el colector—, así que es el único dato de aquí que el emisor no
	// puede falsear sin cambiar de red de verdad. Sirve para ubicar al nodo
	// por prefijo cuando su sistema le esconde el SSID.
	PublicIP string `json:"public_ip,omitempty"`
}
type State struct {
	mu    sync.Mutex
	Nodes map[string]*NodeState `json:"nodes"`
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

func telegram(text string) {
	if cfg.TelegramToken == "" {
		return
	}
	form := url.Values{}
	form.Set("chat_id", cfg.TelegramChat)
	form.Set("text", text)
	cl := &http.Client{Timeout: 15 * time.Second}
	if r, err := cl.PostForm("https://api.telegram.org/bot"+cfg.TelegramToken+"/sendMessage", form); err == nil {
		r.Body.Close()
	}
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
	ns.LastSeen, ns.Uptime, ns.Down = now, up, false
	ns.PublicIP = ipDeOrigen(r)
	// Solo se pisa si el latido trae memoria de verdad: un binario viejo sin
	// estos campos no debe borrar el ultimo dato bueno que ya se tenia.
	if memTotal > 0 {
		ns.MemUsedMB, ns.MemTotalMB = memUsed, memTotal
	}
	state.mu.Unlock()
	if wasDown {
		telegram(fmt.Sprintf("✅ heartbeat: %s volvió a reportar (por internet).", node))
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

func handleStatus(w http.ResponseWriter, r *http.Request) {
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now().Unix()
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{}
	for n, ns := range state.Nodes {
		out[n] = map[string]any{"last_seen_ago_s": now - ns.LastSeen, "down": ns.Down, "uptime_s": ns.Uptime, "movil": esMovil(n), "mem_used_mb": ns.MemUsedMB, "mem_total_mb": ns.MemTotalMB}
	}
	json.NewEncoder(w).Encode(out)
}

// memInfoSuffix arma el trozo diagnostico del mensaje de dead-man a partir de
// la ULTIMA memoria conocida del nodo (no hay otra: si el nodo no contesta al
// latido, tampoco va a contestar a un /metrics en vivo). Vacio si nunca llego
// memoria (cliente viejo, o nodo movil sin este campo) — MCL-199.
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
			silent := now - ns.LastSeen
			if silent > int64(cfg.DeadAfterSecs) {
				if !ns.Down || now-ns.LastAlert >= realert {
					mins := silent / 60
					// MCL-199: la memoria de justo antes de callarse, para que
					// la alerta diagnostique en vez de solo constatar.
					telegram(fmt.Sprintf("🔴 heartbeat: %s no reporta hace %d min (posible caída o sin internet).%s", node, mins, memInfoSuffix(ns)))
					ns.Down = true
					ns.LastAlert = now
				}
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
		cfg.DeadAfterSecs = 600 // 10 min sin latido = alerta
	}
	if cfg.RealertHours == 0 {
		cfg.RealertHours = 8
	}
	loadState()
	go deadManLoop()
	http.HandleFunc("/hb", handleHB)
	http.HandleFunc("/status", handleStatus)
	log.Printf("heartbeat-collector escuchando en %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, nil))
}
