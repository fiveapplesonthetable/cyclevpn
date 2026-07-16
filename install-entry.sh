#!/usr/bin/env bash
# install-entry.sh — turnkey ENTRY node, runs INSIDE Russia (e.g. your RuVDS box).
# Your phone connects here with a STANDARD VLESS app (Shadowrocket / v2rayNG).
# That hop is domestic (Russia->Russia) so it is NOT throttled; this box then
# tunnels out to the exit node using the cycling transport that beats the throttle.
#
#   sudo ./install-entry.sh <exit-domain> [phone-port]
set -euo pipefail

EXIT_DOMAIN="${1:?usage: install-entry.sh <exit-domain> [phone-port]}"
PORT="${2:-2053}"
ARCH=$(dpkg --print-architecture 2>/dev/null || echo amd64)
BIN_SRC="$(dirname "$0")/cyclevpn-linux-${ARCH}"
[ -f "$BIN_SRC" ] || BIN_SRC="$(dirname "$0")/cyclevpn"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl unzip ufw qrencode >/dev/null || apt-get install -y -qq curl unzip ufw >/dev/null

echo ">> installing cyclevpn client binary"
install -m0755 "$BIN_SRC" /usr/local/bin/cyclevpn

echo ">> installing xray (standard VLESS server for the phone)"
if [ ! -x /usr/local/bin/xray ]; then
  curl -4 -sSL -o /tmp/xray.zip "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip"
  mkdir -p /tmp/xr && (cd /tmp/xr && unzip -o /tmp/xray.zip >/dev/null && install -m0755 xray /usr/local/bin/xray)
fi

UUID=$(cat /proc/sys/kernel/random/uuid)
IP=$(curl -4 -s --max-time 8 https://api.ipify.org || hostname -I | awk '{print $1}')

echo ">> cyclevpn client service (-> ${EXIT_DOMAIN})"
cat > /etc/systemd/system/cyccli.service <<UNIT
[Unit]
Description=cyclevpn cycling client
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/cyclevpn client -url https://${EXIT_DOMAIN} -listen 127.0.0.1:10900 -workers 32 -maxconns 128 -pool 96 -fetch-timeout 1500ms
Restart=always
RestartSec=2
LimitNOFILE=1048576
[Install]
WantedBy=multi-user.target
UNIT

echo ">> xray VLESS entry (port ${PORT}) -> cycling client"
mkdir -p /usr/local/etc/xray
cat > /usr/local/etc/xray/entry.json <<JSON
{
  "log": {"loglevel": "warning"},
  "inbounds": [{
    "listen": "0.0.0.0", "port": ${PORT}, "protocol": "vless",
    "settings": {"clients": [{"id": "${UUID}"}], "decryption": "none"},
    "streamSettings": {"network": "tcp", "security": "none"}
  }],
  "outbounds": [{
    "protocol": "socks",
    "settings": {"servers": [{"address": "127.0.0.1", "port": 10900}]}
  }]
}
JSON
cat > /etc/systemd/system/entry.service <<'UNIT'
[Unit]
Description=cyclevpn VLESS entry
After=network-online.target cyccli.service
[Service]
ExecStart=/usr/local/bin/xray run -c /usr/local/etc/xray/entry.json
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT

ufw allow 22/tcp >/dev/null 2>&1 || true
ufw allow "${PORT}/tcp" >/dev/null 2>&1 || true
yes | ufw enable >/dev/null 2>&1 || true

systemctl daemon-reload
systemctl enable --now cyccli entry >/dev/null
sleep 3

LINK="vless://${UUID}@${IP}:${PORT}?encryption=none&type=tcp&security=none#RU-cyclevpn"
echo
echo "=================================================================="
echo " ENTRY node ready. Phone config (Shadowrocket / v2rayNG / NekoBox):"
echo
echo "  ${LINK}"
echo
command -v qrencode >/dev/null && qrencode -t ANSIUTF8 "${LINK}" || echo " (install 'qrencode' to print a scannable QR here)"
echo "=================================================================="
echo "$LINK" > /root/cyclevpn-phone-link.txt
echo "saved link to /root/cyclevpn-phone-link.txt"
