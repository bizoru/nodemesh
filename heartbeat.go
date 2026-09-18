package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
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
		// 60 s. Antes 150, y con el umbral del colector en 3 min eso dejaba
		// margen para UN solo latido perdido: un latido que se retrasa se
		// volvia una alerta falsa. A 60 s hacen falta tres latidos perdidos
		// seguidos para alertar, que ya es una caida de verdad.
		interval = 60
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
	// MCL-199 (postmortem OOM de entry, 2026-09-12): si el dead-man dispara,
	// que el mensaje diga algo mas que "no contesta". localSpecs() es la
	// MISMA Specs que ya viaja por gossip (cacheada 30s, cero coste extra);
	// aqui va tambien por el latido porque el latido es el canal que
	// sobrevive cuando el tailnet —y con el, el gossip— se cae. El colector
	// guarda el ULTIMO valor recibido, asi que si el nodo se queda mudo, la
	// alerta cuenta la memoria de justo antes de callarse, no un dato en
	// vivo que por definicion no se puede pedir a un nodo que no contesta.
	specs := localSpecs()
	form.Set("mem_used_mb", fmt.Sprint(specs.MemUsedMB))
	form.Set("mem_total_mb", fmt.Sprint(specs.MemTotalMB))
	resp, err := client.PostForm(cfg.HeartbeatURL, form)
	if err != nil {
		// silencioso salvo debug: un latido perdido no es un error del nodo,
		// es justo lo que el colector detecta.
		return
	}
	// El colector contesta con la IP desde la que nos vio salir. Es la única
	// vía para conocer la IP pública propia SIN preguntarle a un servicio de
	// fuera, y es lo que ubica a los nodos donde el sistema no deja leer el
	// SSID (macOS sin Localización) ni la tabla ARP (Android). El latido ya
	// cruzaba internet plano; esto no añade ni una petición.
	//
	// Un colector viejo responde "ok" a secas: fijaIPPublica lo descarta por
	// no ser una IP, así que se puede desplegar en cualquier orden.
	cuerpo, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("heartbeat: %s respondió %d", cfg.HeartbeatURL, resp.StatusCode)
		return
	}
	for _, campo := range strings.Fields(string(cuerpo)) {
		fijaIPPublica(campo)
	}
}
