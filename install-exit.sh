#!/usr/bin/env bash
# install-exit.sh — turnkey EXIT node (the box that reaches the open internet).
# Run on a fresh Debian/Ubuntu VPS OUTSIDE Russia (e.g. Contabo Germany).
#
#   sudo ./install-exit.sh <exit-domain>
#
# <exit-domain> must already have a DNS A record pointing at THIS box's public IP
# (DNS-only / grey-cloud if it's on Cloudflare). Caddy auto-provisions HTTPS.
set -euo pipefail

DOMAIN="${1:?usage: install-exit.sh <exit-domain>}"
ARCH=$(dpkg --print-architecture 2>/dev/null || echo amd64)
BIN_SRC="$(dirname "$0")/cyclevpn-linux-${ARCH}"
[ -f "$BIN_SRC" ] || BIN_SRC="$(dirname "$0")/cyclevpn"   # fallback to a prebuilt ./cyclevpn

echo ">> installing dependencies (caddy)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl ufw >/dev/null
if ! command -v caddy >/dev/null; then
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y -qq caddy >/dev/null
fi

echo ">> installing cyclevpn relay binary"
install -m0755 "$BIN_SRC" /usr/local/bin/cyclevpn

cat > /etc/systemd/system/cycrelay.service <<'UNIT'
[Unit]
Description=cyclevpn relay
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/cyclevpn relay -listen 127.0.0.1:8791
Restart=always
RestartSec=2
LimitNOFILE=1048576
NoNewPrivileges=true
[Install]
WantedBy=multi-user.target
UNIT

echo ">> configuring Caddy (TLS + reverse proxy) for ${DOMAIN}"
cat > /etc/caddy/Caddyfile <<CADDY
${DOMAIN} {
	handle /t/* {
		reverse_proxy 127.0.0.1:8791
	}
	handle {
		respond "ok" 200
	}
}
CADDY

echo ">> firewall (22, 80, 443)"
ufw allow 22/tcp  >/dev/null 2>&1 || true
ufw allow 80/tcp  >/dev/null 2>&1 || true
ufw allow 443/tcp >/dev/null 2>&1 || true
yes | ufw enable  >/dev/null 2>&1 || true

systemctl daemon-reload
systemctl enable --now cycrelay >/dev/null
systemctl restart caddy

sleep 3
echo ">> verifying"
curl -s --max-time 5 http://127.0.0.1:8791/t/ping && echo "  relay ok (local)"
for i in $(seq 1 15); do
  if [ "$(curl -s --max-time 6 -o /dev/null -w '%{http_code}' "https://${DOMAIN}/t/ping")" = 200 ]; then
    echo "  HTTPS ok: https://${DOMAIN}/t/ping"; break
  fi; sleep 4
done
echo ">> EXIT node ready:  https://${DOMAIN}"
echo "   point the entry client at it with:  cyclevpn client -url https://${DOMAIN} ..."
