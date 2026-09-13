# heartbeat-collector

Dead-man switch para la flota. Corre en **gcp-east** detrás de **Tailscale Funnel**
(`https://gcp-east.tail0ebe9.ts.net/hb`), así los nodos empujan "sigo vivo" por
**internet plano, independiente del tailnet** — la señal que falta cuando el
tailnet cae (todo lo demás, gossip y watchdogs, va por el tailnet y muere con él).

Vive FUERA de entry a propósito: entry concentra todo el alertado y era el SPOF.

## Cómo funciona
- Cada nodemesh (los 5+) hace POST `/hb node=X token=... uptime=... mem_used_mb=...
  mem_total_mb=...` cada ~2.5 min (campos `heartbeatURL`/`heartbeatToken`/
  `heartbeatSecs` en su nodemesh.json). La memoria sale de `localSpecs()`, la
  misma que ya viaja por gossip — cero coste extra, cacheada 30 s.
- El colector guarda last_seen y la ÚLTIMA memoria conocida por nodo
  (`/var/lib/heartbeat/state.json`). Un binario viejo que no mande `mem_*` no
  borra el último dato bueno (ver el guard en `handleHB`).
- Bucle dead-man: si un nodo de `expect` calla > `deadAfterSecs` (10 min), alerta
  por la API DIRECTA de Telegram (chat 66513789), con la última memoria
  conocida si la hay (MCL-199, postmortem del OOM de entry 2026-09-12: antes el
  mensaje era solo "no reporta"; ahora, cuando hay dato, dice algo como
  "Última memoria conocida: 3502/4096 MB (85%)" — la diferencia entre "se
  cayó" y "se está quedando sin memoria"). Re-alerta cada `realertHours`. ✅ al
  volver.
- `/status` (JSON) para ver last_seen_ago_s y mem_used_mb/mem_total_mb por nodo.

### Pendiente, no implementado aquí (MCL-199, ver el postmortem)
Memoria usada/total ya dice mucho ("iba al 92%" no es lo mismo que "llevaba
2 min sin cortes de luz"), pero no dice QUIÉN se comió la RAM. Para eso
faltaría un campo más, p. ej. `top_rss` ("proceso:MB"), leyendo `/proc/*/status`
igual que `meminfoKB` lee `/proc/meminfo` en `specs_linux.go` — Linux-only
a propósito (entry es la máquina que de verdad importa aquí), sin exec, mismo
criterio que el resto de `specs_linux.go`. Se deja sin construir: es más
código en un binario expuesto a internet (aunque sea detrás del token) y no
hacía falta para MCL-199 tal como se pidió. Si se necesita, va en
`heartbeat.go` (juntando el nuevo campo al `form.Set` de memoria) y en
`main.go` (`NodeState` + `memInfoSuffix`).

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
