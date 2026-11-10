#!/usr/bin/env bash
# Creates a Debian LXC container on a Proxmox VE host and installs Cloudflare
# Tunnel Manager in it. Run it in the Proxmox host shell:
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/AitorConS/cloudflare-tunnel-manager/main/proxmox/ct.sh)"
#
# Every setting has a default and can be overridden with an environment variable:
#   CTID  CT_HOSTNAME  CORES  MEMORY  SWAP  DISK  BRIDGE  IP  GATEWAY
#   STORAGE  TEMPLATE_STORAGE  CTM_PORT  CTM_VERSION  CTM_YES=1 (no questions)
set -euo pipefail

REPO="${CTM_REPO:-AitorConS/cloudflare-tunnel-manager}"
RAW="https://raw.githubusercontent.com/${REPO}/main"

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this script as root on the Proxmox host"
for cmd in pct pveam pvesm pvesh; do
  command -v "$cmd" >/dev/null || die "'$cmd' not found: this script must run on a Proxmox VE host"
done

INTERACTIVE=0
if [ -t 0 ] && [ "${CTM_YES:-0}" != 1 ]; then INTERACTIVE=1; fi

# pick_storage <content-type> <label>: prints the storage to use for that content.
pick_storage() {
  local content="$1" label="$2" choice
  local -a stores
  mapfile -t stores < <(pvesm status -content "$content" 2>/dev/null | awk 'NR>1 && $3=="active" {print $1}')
  [ "${#stores[@]}" -gt 0 ] || die "no active storage accepts $label"
  if [ "${#stores[@]}" -eq 1 ] || [ "$INTERACTIVE" -eq 0 ]; then
    echo "${stores[0]}"
    return
  fi
  echo "Storage for $label:" >&2
  select choice in "${stores[@]}"; do
    [ -n "$choice" ] && break
  done
  echo "$choice"
}

CTID="${CTID:-$(pvesh get /cluster/nextid)}"
CT_HOSTNAME="${CT_HOSTNAME:-tunnel-manager}"
CORES="${CORES:-1}"
MEMORY="${MEMORY:-512}"
SWAP="${SWAP:-512}"
DISK="${DISK:-2}"
BRIDGE="${BRIDGE:-vmbr0}"
IP="${IP:-dhcp}"
GATEWAY="${GATEWAY:-}"
PORT="${CTM_PORT:-8088}"
STORAGE="${STORAGE:-$(pick_storage rootdir "the container disk")}"
TEMPLATE_STORAGE="${TEMPLATE_STORAGE:-$(pick_storage vztmpl "container templates")}"

pct status "$CTID" >/dev/null 2>&1 && die "container $CTID already exists; set CTID to a free id"

NET="name=eth0,bridge=${BRIDGE},ip=${IP}"
if [ "$IP" != dhcp ] && [ -n "$GATEWAY" ]; then NET="${NET},gw=${GATEWAY}"; fi

cat <<SUMMARY

  Cloudflare Tunnel Manager, new LXC container

    ID / hostname   $CTID / $CT_HOSTNAME
    Resources       $CORES core(s), ${MEMORY} MB RAM, ${DISK} GB disk on $STORAGE
    Network         $BRIDGE, $IP
    Type            unprivileged Debian, starts on boot
    Panel port      $PORT

SUMMARY
if [ "$INTERACTIVE" -eq 1 ]; then
  read -r -p "Create it? [Y/n] " answer
  case "$answer" in
    "" | y | Y | yes) ;;
    *) echo "Cancelled. Change settings with environment variables, e.g. CTID=120 MEMORY=1024 bash ct.sh"; exit 0 ;;
  esac
fi

say "Finding a Debian template"
pveam update >/dev/null 2>&1 || true
TEMPLATE="$(pveam available --section system | awk '{print $2}' | grep -E '^debian-1[23]-standard_.*_amd64\.tar\.' | sort -V | tail -n 1)"
[ -n "$TEMPLATE" ] || die "no Debian 12/13 template is available from pveam"
if ! pveam list "$TEMPLATE_STORAGE" 2>/dev/null | grep -q "$TEMPLATE"; then
  say "Downloading $TEMPLATE"
  pveam download "$TEMPLATE_STORAGE" "$TEMPLATE" >/dev/null
fi

say "Creating container $CTID"
pct create "$CTID" "${TEMPLATE_STORAGE}:vztmpl/${TEMPLATE}" \
  --hostname "$CT_HOSTNAME" \
  --cores "$CORES" --memory "$MEMORY" --swap "$SWAP" \
  --rootfs "${STORAGE}:${DISK}" \
  --net0 "$NET" \
  --unprivileged 1 --features nesting=1 --onboot 1 --ostype debian \
  --tags tunnel-manager \
  --description "Cloudflare Tunnel Manager, https://github.com/${REPO}" >/dev/null

on_error() {
  echo
  echo "The installation did not finish. The container was left in place for inspection." >&2
  echo "  Look inside:  pct enter $CTID" >&2
  echo "  Remove it:    pct stop $CTID; pct destroy $CTID" >&2
}
trap on_error ERR

pct start "$CTID"
say "Waiting for the container's network"
for i in $(seq 1 30); do
  if pct exec "$CTID" -- getent hosts github.com >/dev/null 2>&1; then break; fi
  [ "$i" -lt 30 ] || die "the container has no network or DNS (bridge $BRIDGE, ip $IP)"
  sleep 2
done

say "Installing Tunnel Manager inside the container"
pct exec "$CTID" -- bash -c "
  set -e
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq curl ca-certificates >/dev/null
  curl -fsSL '${RAW}/proxmox/install.sh' | CTM_REPO='${REPO}' CTM_PORT='${PORT}' CTM_VERSION='${CTM_VERSION:-latest}' bash
"

CT_IP="$(pct exec "$CTID" -- hostname -I 2>/dev/null | awk '{print $1}')"
cat <<DONE

  Done. Container $CTID ($CT_HOSTNAME) is running.

    Panel     http://${CT_IP:-<container-ip>}:${PORT}
    Update    pct exec $CTID -- tunnel-manager-update   (or Settings in the panel)
    Logs      pct exec $CTID -- journalctl -u tunnel-manager -f

DONE
