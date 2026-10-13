#!/usr/bin/env bash
# Despliega el binario Phase A a rigby cuando Bitvise vuelva a aceptar
# conexiones no-admin (license aplicada o reemplazo del SSH server).
#
# Uso: bash deploy-rigby.sh
#
# Preconds:
#   - estar en /Users/steven/repos/nodemesh con dist/nodemesh-windows-386.exe
#     construido por build.sh (ya está en el commit 9ed1a40 de main)
#   - `ssh rigby echo` responde sin banner de evaluation-expired
set -euo pipefail

BIN="dist/nodemesh-windows-386.exe"
if [[ ! -f "$BIN" ]]; then
  echo "falta $BIN — corre ./build.sh primero" >&2
  exit 1
fi

echo "probando ssh rigby…"
probe=$(ssh -o ConnectTimeout=10 -o BatchMode=yes rigby 'echo READY' 2>&1 || true)
if [[ "$probe" != *READY* ]]; then
  echo "SSH aún bloqueado. Respuesta:" >&2
  echo "$probe" >&2
  exit 2
fi

echo "subiendo binario a ~ops/nodemesh.new.exe…"
scp "$BIN" rigby:nodemesh.new.exe

echo "parando scheduled task y reemplazando binario…"
ssh rigby 'schtasks /end /tn nodemesh' || true
sleep 3
ssh rigby 'cmd /c "move /y nodemesh.new.exe C:\ProgramData\nodemesh\nodemesh.exe"'
ssh rigby 'schtasks /run /tn nodemesh'

echo "esperando 20 s para que el latido llegue a gcp-east…"
sleep 20

echo "verificando campos nuevos en state.json:"
ssh gcp-east 'curl -s http://127.0.0.1:7778/state.json 2>/dev/null || curl -s http://127.0.0.1:8080/state.json' \
  | python3 -c '
import json, sys
d = json.load(sys.stdin)
r = d.get("nodes", {}).get("rigby", {})
print(f"  tailscale_state={r.get(\"tailscale_state\", \"MISSING\")}")
print(f"  ts_ip={r.get(\"ts_ip\", \"MISSING\")}")
print(f"  mac={r.get(\"mac\", \"MISSING\")}")
print(f"  lima_vms={r.get(\"lima_vms\", \"n/a (rigby no corre Lima)\")}")
'
echo "listo. Phase A debería estar 11/11."
