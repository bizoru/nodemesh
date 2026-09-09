package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

// heartbeatLoop empuja "sigo vivo" a un endpoint público (gcp-east vía Tailscale
// Funnel) por internet plano, INDEPENDIENTE del tailnet. Es el dead-man switch:
// el colector alerta si un nodo deja de reportar. Complementa el gossip (que va
// por el tailnet y falla justo cuando el tailnet falla).
//
// Solo corre si cfg.HeartbeatURL está puesto. El POST lleva el nombre del nodo,
// su uptime y un token compartido (el endpoint es público; el token evita que
// cualquiera falsifique latidos). Cadencia propia (HeartbeatSecs), típicamente
// más lenta que el collector.
func heartbeatLoop(cfg *Config) {
	if cfg.HeartbeatURL == "" {
		return
	}
	interval := cfg.HeartbeatSecs
	if interval == 0 {
		interval = 150 // 2.5 min por defecto
	}
	client := &http.Client{Timeout: 15 * time.Second}
	for {
		sendHeartbeat(cfg, client)
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func sendHeartbeat(cfg *Config, client *http.Client) {
	form := url.Values{}
	form.Set("node", cfg.Node)
	form.Set("token", cfg.HeartbeatToken)
	form.Set("uptime", fmt.Sprint(uptimeSeconds()))
	resp, err := client.PostForm(cfg.HeartbeatURL, form)
	if err != nil {
		// silencioso salvo debug: un latido perdido no es un error del nodo,
		// es justo lo que el colector detecta.
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("heartbeat: %s respondió %d", cfg.HeartbeatURL, resp.StatusCode)
	}
}
