#!/usr/bin/env bash
# install.sh — bootstrap de rigby en Linux. Idempotente.
#
# Uso 1 (desde consola física, tras primer boot sin SSH):
#   sudo bash install.sh
# Uso 2 (desde la Mac via SSH, con preseed ya aplicado):
#   scp -r bundle rigby:/tmp/ && ssh rigby 'sudo bash /tmp/bundle/install.sh'
#
# Qué hace:
#  1. Autoriza la pubkey ed25519 "rigby-access" para el usuario ops.
#  2. Crea el usuario de servicio nodemesh.
#  3. Copia el binario a /usr/local/bin/.
#  4. Copia el config a /etc/nodemesh/.
#  5. Instala y habilita nodemesh.service.
#  6. Verifica status.
#
# NO despliega socorro-watchdog: la pata rigby del tailnet-watchdog fue
# retirada 2026-09-23 (redundante con testigoSuffix del dead-man).
# NO toca: WiFi, firewall, DNS. Eso lo decide Debian al instalar.

set -euo pipefail

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  echo "corré con sudo" >&2
  exit 1
fi

HERE="$(cd "$(dirname "$0")" && pwd)"
PUB='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICuFWPLciYu5Soyz8i1Jb49A0Pg4aQv9s6YZYT5/od1w rigby-access'

echo "[1/5] autorizando la pubkey rigby-access para ops..."
install -d -o ops -g ops -m 0700 /home/ops/.ssh
touch /home/ops/.ssh/authorized_keys
chown ops:ops /home/ops/.ssh/authorized_keys
chmod 0600 /home/ops/.ssh/authorized_keys
grep -qxF "$PUB" /home/ops/.ssh/authorized_keys || echo "$PUB" >> /home/ops/.ssh/authorized_keys

echo "[2/5] creando usuario de servicio nodemesh..."
getent group  nodemesh >/dev/null || groupadd --system nodemesh
getent passwd nodemesh >/dev/null || useradd  --system --gid nodemesh --home-dir /var/lib/nodemesh --shell /usr/sbin/nologin nodemesh

echo "[3/5] instalando binario..."
install -m 0755 "$HERE/usr/local/bin/nodemesh" /usr/local/bin/nodemesh

echo "[4/5] colocando config..."
install -d -m 0755 /etc/nodemesh /var/lib/nodemesh
chown nodemesh:nodemesh /var/lib/nodemesh

if [[ ! -f /etc/nodemesh/nodemesh.json ]]; then
  install -m 0644 "$HERE/etc/nodemesh/nodemesh.json" /etc/nodemesh/nodemesh.json
  echo "  nodemesh.json instalado"
else
  echo "  nodemesh.json ya existe, respetado"
fi

echo "[5/5] instalando unidad systemd..."
install -m 0644 "$HERE/etc/systemd/system/nodemesh.service" /etc/systemd/system/nodemesh.service
systemctl daemon-reload
systemctl enable --now nodemesh.service

echo
echo "estado:"
systemctl --no-pager --lines=0 status nodemesh.service || true

echo
echo "listo. verificá el latido desde el tailnet:"
echo "  ssh entry 'curl -fsS http://entry.ts.stevensierra.xyz:7777/api/nodes' | jq .rigby"
