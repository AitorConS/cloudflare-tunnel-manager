#!/usr/bin/env bash
# Installs or updates Cloudflare Tunnel Manager on a Debian/Ubuntu system with
# systemd, for example inside a Proxmox LXC container. Safe to run again: it
# upgrades the binary and cloudflared and keeps the data.
#
#   curl -fsSL https://raw.githubusercontent.com/AitorConS/cloudflare-tunnel-manager/main/proxmox/install.sh | bash
#
# Optional environment: CTM_VERSION (release tag, default latest), CTM_PORT (default 8088).
set -euo pipefail

REPO="${CTM_REPO:-AitorConS/cloudflare-tunnel-manager}"
VERSION="${CTM_VERSION:-latest}"
PORT="${CTM_PORT:-8088}"
INSTALL_DIR=/opt/tunnel-manager
DATA_DIR=/var/lib/tunnel-manager
SVC_USER=tunnelmanager
UNIT=/etc/systemd/system/tunnel-manager.service

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this script as root"
command -v systemctl >/dev/null || die "systemd is required"
command -v apt-get >/dev/null || die "this script supports Debian and Ubuntu only"

case "$(dpkg --print-architecture)" in
  amd64) ARCH=amd64 ;;
  arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(dpkg --print-architecture)" ;;
esac

export DEBIAN_FRONTEND=noninteractive
say "Installing prerequisites"
apt-get update -qq
apt-get install -y -qq curl ca-certificates >/dev/null

say "Installing cloudflared"
install_cloudflared_apt() {
  install -d -m 0755 /usr/share/keyrings
  curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
  echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' \
    > /etc/apt/sources.list.d/cloudflared.list
  apt-get update -qq
  apt-get install -y -qq cloudflared >/dev/null
}
install_cloudflared_deb() {
  local deb
  deb="$(mktemp --suffix=.deb)"
  curl -fsSL -o "$deb" "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${ARCH}.deb"
  dpkg -i "$deb" >/dev/null
  rm -f "$deb"
}
if ! install_cloudflared_apt; then
  say "Cloudflare's apt repository is unavailable; using the release package instead"
  rm -f /etc/apt/sources.list.d/cloudflared.list
  install_cloudflared_deb
fi
cloudflared --version

say "Downloading Tunnel Manager ($VERSION)"
if [ "$VERSION" = latest ]; then
  BASE="https://github.com/${REPO}/releases/latest/download"
else
  BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/ctm-linux-${ARCH}" "$BASE/ctm-linux-${ARCH}"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt"
(cd "$TMP" && grep " ctm-linux-${ARCH}\$" checksums.txt | sha256sum -c --quiet -) \
  || die "the download does not match its checksum"

say "Installing the service"
id "$SVC_USER" >/dev/null 2>&1 || useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SVC_USER"
# The service user owns the install directory so the panel can update itself.
install -d -o "$SVC_USER" -g "$SVC_USER" -m 0755 "$INSTALL_DIR"
install -d -o "$SVC_USER" -g "$SVC_USER" -m 0700 "$DATA_DIR"
install -o "$SVC_USER" -g "$SVC_USER" -m 0755 "$TMP/ctm-linux-${ARCH}" "$INSTALL_DIR/ctm.new"
mv -f "$INSTALL_DIR/ctm.new" "$INSTALL_DIR/ctm"

cat > "$UNIT" <<UNIT_EOF
[Unit]
Description=Cloudflare Tunnel Manager
After=network-online.target
Wants=network-online.target

[Service]
User=$SVC_USER
Group=$SVC_USER
Environment=CTM_DATA_DIR=$DATA_DIR
Environment=CTM_LISTEN=:$PORT
Environment=CTM_SELF_UPDATE=1
EnvironmentFile=-/etc/default/tunnel-manager
ExecStart=$INSTALL_DIR/ctm
Restart=always
RestartSec=2
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT_EOF

cat > /usr/local/bin/tunnel-manager-update <<UPDATE_EOF
#!/bin/sh
# Updates Tunnel Manager and cloudflared to their latest releases.
set -e
curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/proxmox/install.sh" | CTM_PORT="$PORT" bash
UPDATE_EOF
chmod 0755 /usr/local/bin/tunnel-manager-update

systemctl daemon-reload
systemctl enable --quiet tunnel-manager
systemctl restart tunnel-manager

for _ in $(seq 1 20); do
  if curl -fs -o /dev/null "http://127.0.0.1:${PORT}/api/status"; then
    IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
    say "Tunnel Manager $("$INSTALL_DIR/ctm" --version | awk '{print $2}') is running"
    echo "    Open http://${IP:-<this-machine>}:${PORT} and create the admin account."
    echo "    Update later with: tunnel-manager-update  (or from Settings in the panel)"
    exit 0
  fi
  sleep 1
done
die "the service did not start; see: journalctl -u tunnel-manager -n 50"
