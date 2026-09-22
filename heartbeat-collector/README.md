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
- Bucle dead-man: si un nodo de `expect` calla > `deadAfterSecs` (3 min: tres
  latidos perdidos), alerta por **dos vías independientes** — la API DIRECTA de
  Telegram (chat 66513789) y **notify** (`notifyTargets`, hoy el R1 y los M5),
  que corre en esta misma máquina. Con la última memoria
  conocida si la hay (MCL-199, postmortem del OOM de entry 2026-09-12: antes el
  mensaje era solo "no reporta"; ahora, cuando hay dato, dice algo como
  "Última memoria conocida: 3502/4096 MB (85%)" — la diferencia entre "se
  cayó" y "se está quedando sin memoria"). Re-alerta cada `realertHours`. ✅ al
  volver.
- `/status` (JSON) para ver last_seen_ago_s y mem_used_mb/mem_total_mb por nodo.
- `POST /mantenimiento node=entry mins=30 token=...` anuncia una parada: mientras
  dure, el silencio de ese nodo NO alerta, pero al volver SÍ avisa. `mins=0` la
  cancela; el tope son 12 h. Es la alternativa a apagar el vigilante y olvidarse
  de encenderlo.

## Por qué 3 minutos y dos vías (2026-09-18)
El 18-sep entry estuvo apagada 8 minutos para un resize y **no salió una sola
alerta**. Tres fallos a la vez, y los tres silenciosos:

1. `deadAfterSecs` eran 600, así que una caída de 8 min no llegaba a mirarse.
2. `telegramToken` estaba **vacío** desde el 11-sep por lo menos: el colector
   marcaba `last_alert` y no enviaba nada. El `telegram()` de entonces se
   tragaba el error, el código HTTP y el token vacío sin registrar una línea.
3. El aviso sólo iba a Telegram, nunca a los aparatos.

De ahí: umbral de 3 min (con los nodos latiendo cada 60 s), envío también por
notify, y **todo fallo de envío se registra** — un vigilante que cree haber
avisado y no avisó es peor que no tener vigilante.

## Vigilancia de los CANALES (canales.go)
La lección de fondo del 18-sep no fue el token: fue que **nadie vigilaba la
capacidad de avisar**. Cada 5 min (`canalesCadaSegs`) el colector comprueba que
sus dos vías siguen vivas, y **denuncia cada una por la otra**:

- **telegram**: `getMe` contra la API real — valida el token **sin escribirle a
  nadie**. Un token vacío, revocado o una API inalcanzable se ve en 5 minutos,
  no en una semana. Si falla, se avisa **por los aparatos**.
- **notify**: su `/v1/health` (derivado de `notifyURL`, para no configurar dos
  veces lo mismo). `degraded` cuenta como roto y el mensaje nombra la pieza
  (`mqtt=disconnected`), porque eso es lo que hace falta para arreglarlo. Si
  falla, se avisa **por Telegram**.

Mismo criterio de silencio que el resto de la flota: se avisa al romperse, no se
repite antes de `realertHours`, y la recuperación **siempre** se avisa diciendo
cuánto duró.

### Un bache de red no es una avería (2026-09-18, tarde)
El mismo día que se montó esto, la vigilancia de canales soltó **ocho mensajes
por nada**: cuatro veces `getMe` tardó más de 15 s, y cada una generó su "🔴 no
puede avisar" y, cinco minutos después, su "✅ vuelve a funcionar". gcp-east sale
**solo por IPv6** y a `api.telegram.org` le da hipo; no había ninguna avería que
contar. Y no fue gratis: esos ocho avisos llenaron la cola de la pantalla del R1
y retrasaron seis minutos el aviso de que Abby había salido de clase.

Ahora cada comprobación devuelve además si el fallo es **firme**:

- **Firme** (token vacío o revocado, chat que no existe, un 4xx de la API, notify
  contestando `degraded`): se denuncia en el **primer** sondeo. Eso no se
  arregla solo, y cazarlo rápido es justo para lo que se puso esta vigilancia.
- **Pasajero** (timeout, DNS, 5xx, 429): hacen falta **3 sondeos malos
  seguidos** (`canalesFallosSeguidos`, 15 min con el sondeo por defecto) para
  darlo por roto. Mientras tanto queda en el journal y en `/status` — no se
  pierde, simplemente no se despierta a nadie por un bache. Y como no llega a
  marcarse roto, tampoco hay después un "vuelve a funcionar" que contar: un
  bache **no genera ni un mensaje**.

Un vigilante que cuenta baches de red no está vigilando: está haciendo ruido, y
el ruido tapa justo lo que sí importaba. El estado vive en `state.json` para que un reinicio no borre que
algo llevaba roto. `/status` lo expone bajo `_canales` (los nodos siguen
colgando de la raíz: no se rompe ningún script que ya lo lea).

**OJO al registrar errores de Telegram**: Go mete la URL entera en sus
`*url.Error`, y esa URL lleva el token. Todo lo que se registre pasa por
`sinToken()` — se aprendió publicando el token en el journal en la primera
prueba de este fichero.

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
