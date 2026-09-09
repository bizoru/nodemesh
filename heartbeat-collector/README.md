# heartbeat-collector

Dead-man switch para la flota. Corre en **gcp-east** detrás de **Tailscale Funnel**
(`https://gcp-east.tail0ebe9.ts.net/hb`), así los nodos empujan "sigo vivo" por
**internet plano, independiente del tailnet** — la señal que falta cuando el
tailnet cae (todo lo demás, gossip y watchdogs, va por el tailnet y muere con él).

Vive FUERA de entry a propósito: entry concentra todo el alertado y era el SPOF.

## Cómo funciona
- Cada nodemesh (los 5+) hace POST `/hb node=X token=... uptime=...` cada ~2.5 min
  (campos `heartbeatURL`/`heartbeatToken`/`heartbeatSecs` en su nodemesh.json).
- El colector guarda last_seen por nodo (`/var/lib/heartbeat/state.json`).
- Bucle dead-man: si un nodo de `expect` calla > `deadAfterSecs` (10 min), alerta
  por la API DIRECTA de Telegram (chat 66513789). Re-alerta cada `realertHours`.
  ✅ al volver.
- `/status` (JSON) para ver last_seen_ago_s por nodo.

## Por qué Funnel y no una IP pública
gcp-east NO tiene IPv4 pública (mantenerla costaría, rompe el $0 de GCP) y la IPv6
de los nodos caseros es inconsistente. Funnel da una URL HTTPS pública con IPv4+IPv6
gratis, servida por la infra de Tailscale, alcanzable aunque el emisor NO esté en
el tailnet. Auto-cura: con tailnet arriba el POST va por MagicDNS (tailnet), con
tailnet caído el DNS revierte al público y el POST sale por internet — llega igual.

## Deploy (gcp-east)
```
GOOS=linux GOARCH=amd64 go build -o hbcollector .
# /opt/heartbeat/hbcollector + /etc/heartbeat/config.json (600, con token+telegram)
# systemd: heartbeat-collector.service
sudo tailscale funnel --bg --set-path=/ 9099
```
El token de Telegram y el token compartido NO van en git.
