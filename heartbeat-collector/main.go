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
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

type Config struct {
	Listen        string            `json:"listen"`         // ej "127.0.0.1:9099" (Funnel apunta aquí)
	Token         string            `json:"token"`          // compartido con los nodos
	StatePath     string            `json:"statePath"`      // ej "/var/lib/heartbeat/state.json"
	TelegramToken string            `json:"telegramToken"`
	TelegramChat  string            `json:"telegramChat"`
	DeadAfterSecs int               `json:"deadAfterSecs"`  // silencio máximo antes de alertar
	RealertHours  int               `json:"realertHours"`
	Expect        []string          `json:"expect"`         // nodos que SE esperan (alertar si nunca llegan)
	Locations     map[string]string `json:"locations,omitempty"`
}

type NodeState struct {
	LastSeen int64 `json:"last_seen"`
	Uptime   int64 `json:"uptime"`
	Down     bool  `json:"down"`
	LastAlert int64 `json:"last_alert"`
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
	now := time.Now().Unix()
	state.mu.Lock()
	ns := state.Nodes[node]
	if ns == nil {
		ns = &NodeState{}
		state.Nodes[node] = ns
	}
	wasDown := ns.Down
	ns.LastSeen, ns.Uptime, ns.Down = now, up, false
	state.mu.Unlock()
	if wasDown {
		telegram(fmt.Sprintf("✅ heartbeat: %s volvió a reportar (por internet).", node))
	}
	fmt.Fprintln(w, "ok")
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now().Unix()
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{}
	for n, ns := range state.Nodes {
		out[n] = map[string]any{"last_seen_ago_s": now - ns.LastSeen, "down": ns.Down, "uptime_s": ns.Uptime}
	}
	json.NewEncoder(w).Encode(out)
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
			silent := now - ns.LastSeen
			if silent > int64(cfg.DeadAfterSecs) {
				if !ns.Down || now-ns.LastAlert >= realert {
					mins := silent / 60
					telegram(fmt.Sprintf("🔴 heartbeat: %s no reporta hace %d min (posible caída o sin internet).", node, mins))
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
