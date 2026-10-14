# rigby → Debian 13 — guía de reinstalación

Carpeta: `/Users/steven/rigby-linux-prep/`.

## Qué hay aquí

| archivo | para qué sirve |
| --- | --- |
| `debian-13.7.0-amd64-netinst.iso` | ISO oficial de Debian 13 Trixie. Incluye firmware non-free. SHA256 verificado. |
| `debian-SHA256SUMS` | checksum para re-verificar la ISO en Bucaramanga. |
| `ventoy-1.1.18-linux.tar.gz` | Ventoy para flashear el pendrive desde Linux. |
| `ventoy-1.1.18-windows.zip` | Ventoy para flashear el pendrive desde Windows (en tu Mac usá UTM/Parallels si lo preferís). |
| `preseed.cfg` | respuestas semi-automáticas para el instalador. Opcional. |
| `bundle/` | binarios + configs + systemd units + install.sh que vas a correr post-primer-boot. |
| `rigby-linux-bundle.tar.gz` | la misma `bundle/` empaquetada, lista para `scp` a la tablet. |

## Hardware que vamos a enfrentar

- HP Stream 7 (Bay Trail Z3735G, 4×1.33 GHz, **0.94 GB RAM**, eMMC 24 GB).
- **UEFI 32-bit sobre CPU 64-bit** — hay que arrancar con `bootia32.efi`. Ventoy ≥ 1.0.80 ya lo lleva.
- WiFi **RTL8723BS** — kernel ≥ 5.15 lo soporta; el `firmware-realtek` del preseed trae el blob.
- Sin Ethernet; WiFi mandatorio desde el instalador.
- Pantalla táctil, autorotación, audio — IRRELEVANTES para el rol (headless). Si fallan, no importa.

## Preparación del pendrive (en Bucaramanga)

Pendrive mínimo: 2 GB. Recomendado 8 GB o más. **TODO lo que tenga se borra.**

### Mac → pendrive con Ventoy (ruta más sencilla)

Ventoy no tiene instalador oficial para macOS, así que lo flasheamos corriendo Ventoy en una VM Linux rápida, o usamos **Rufus** si tenés un Windows a mano. Alternativas por si no querés VM:

- **Opción A — balenaEtcher + mano**: Etcher la ISO directo al pendrive (pierde la ventaja de Ventoy, pero bootea igual en 32-bit UEFI de Bay Trail porque la ISO de Debian lleva bootloader IA32 integrado desde 11.x).
- **Opción B — Ventoy via Docker en la Mac**:
  ```
  docker run --rm --privileged \
      -v /Users/steven/rigby-linux-prep:/work -v /dev:/dev \
      debian:13 bash -c 'apt-get update -qq && apt-get install -y -qq curl util-linux && \
      cd /tmp && tar xf /work/ventoy-1.1.18-linux.tar.gz && cd ventoy-1.1.18 && \
      ./Ventoy2Disk.sh -i /dev/diskX   # <-- reemplazar diskX por el pendrive REAL'
  ```
  (ojo con `diskX`: antes correr `diskutil list` y confirmar que es el pendrive y no tu disco interno).

Una vez Ventoy esté en el pendrive, aparecen dos particiones. Montá la grande y copiale:

```
debian-13.7.0-amd64-netinst.iso
preseed.cfg            # opcional, ver abajo
rigby-linux-bundle.tar.gz
```

## Boot en la tablet (HP Stream 7)

1. **Apagá la tablet** completamente (hold power + "Shut down").
2. Enchufá el **adaptador USB OTG** con el pendrive + un **teclado USB** (lo vas a necesitar para el instalador).
3. **Mantené Vol−** y tocá power. Entra al **menú UEFI** (si no abre, repetí con F2 o Vol+ según firmware).
4. En **"Boot Manager"** elegí el pendrive. Si no aparece: **"Boot Device Options" → UEFI: <nombre del USB>**.
5. Si el pendrive aparece como "Legacy" y no "UEFI", desactivá CSM/Legacy Boot en Setup y guardá.
6. **Secure Boot**: dejarlo en Enabled debería funcionar (Debian 13 firma los boot loaders). Si falla, deshabilitarlo.
7. Ventoy muestra la ISO → "**Boot in normal mode**".

### Boot con preseed (opcional, deja la instalación casi desatendida)

En el menú de GRUB de la ISO, parate sobre "Install" y apretá **`e`** para editar la línea de kernel. Agregá al final:

```
auto=true priority=critical file=/hd-media/preseed.cfg
```

y apretá `F10`. El instalador va a cargar el preseed del pendrive, saltar la mayoría de los prompts, y solo te va a pedir:

- **SSID + password del WiFi** (STEVEN.SIERRA — es la red que ve rigby hoy).
- **password para el usuario `ops`** (elegí algo que recordés; podés rotarla luego con `passwd`).

Zona horaria, locale, teclado, nombre de host, particionado, paquetes, y `authorized_keys` para la pubkey `rigby-access` quedan todos preconfigurados.

### Boot sin preseed

Es más clicky pero igual de válido. Durante los prompts del instalador contestá:

- **Idioma**: Spanish (Colombia) o English — da igual.
- **Teclado**: lo que uses físicamente.
- **Hostname**: `rigby`.
- **Domain**: `stevensierra.xyz` (o vacío).
- **Root password**: dejar vacío (sudo).
- **Full name**: `ops` · **Username**: `ops` · password que recuerdes.
- **Zona horaria**: America/Bogota.
- **Particionado**: Guided — use entire disk → eMMC → All files in one partition.
- **Mirror**: Colombia o cualquiera.
- **Software selection**: `standard system utilities` + `SSH server`. **DESMARCÁ `GNOME`** y los demás escritorios — rigby es headless.

Reinicia solo. Primer boot → login en consola física. Correr:

```
sudo install -d -o ops -g ops -m 0700 /home/ops/.ssh
echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICuFWPLciYu5Soyz8i1Jb49A0Pg4aQv9s6YZYT5/od1w rigby-access' \
  | sudo tee /home/ops/.ssh/authorized_keys > /dev/null
sudo chown ops:ops /home/ops/.ssh/authorized_keys
sudo chmod 0600 /home/ops/.ssh/authorized_keys
```

y ya podés salir por SSH desde tu Mac.

## Despliegue del bundle (desde la Mac, por SSH)

```
scp /Users/steven/rigby-linux-prep/rigby-linux-bundle.tar.gz rigby:/tmp/
ssh rigby 'cd /tmp && tar xzf rigby-linux-bundle.tar.gz && sudo bash bundle/install.sh'
```

Al terminar, `nodemesh.service` arranca y empieza a reportar al censo + al dead-man.

Nota: **no desplegamos `socorro-watchdog`**. La pata rigby del `tailnet-watchdog` fue retirada 2026-09-23 (ver `tailnet-watchdog/socorro/README.md`): el testigo LAN de athena ya lo cubre el `testigoSuffix` del dead-man en gcp-east.

## Verificación

Desde cualquier nodo del tailnet:

```
ssh entry 'curl -fsS http://entry.ts.stevensierra.xyz:7777/api/nodes' | jq '."rigby"'
```

Deberías ver un seq nuevo, netType `wifi`, SSID `STEVEN.SIERRA`, localIP `192.168.101.88`. El dead-man de gcp-east también recibirá el latido cada 150 s (rigby queda en la lista `moviles`, así que no dispara aviso al callarse — eso es por diseño).

## Qué queda pendiente para después

- **Retirar el pod `w8-streaming`** (o reutilizarlo): la tablet en Linux ya tiene SSH openssh, no hace falta tunelar por noVNC. El chart se puede borrar del repo o reutilizar si querés consola gráfica remota con otro backend.
- **Sacar rigby de la lista `moviles`** del collector en gcp-east: ahora que corre Debian y queda fija en Socorro con energía estable, su silencio SÍ debería disparar aviso. Editá `/etc/heartbeat/config.json` y restart del service. (No lo hice yo porque es un cambio en gcp-east y requiere permiso explícito.)
- **Reserva DHCP** en el router de Socorro: `bindIP` del nodemesh sigue siendo IP fija (`192.168.101.88`); si el router la rota, se rompe silencioso.
- **Memoria `rigby.md`**: tras la migración, actualizar la nota (OS, acceso ssh openssh en vez de Bitvise, gotchas de Win 8.1 obsoletos, estado "fijo" en vez de "móvil").
