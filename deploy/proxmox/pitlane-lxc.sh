#!/usr/bin/env bash
# Create a Proxmox VE container ready for Pitlane and install it inside.
# Run on the Proxmox host as root:
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/TuroYT/pitlane/main/deploy/proxmox/pitlane-lxc.sh)"
#
# Overrides: CTID, HOSTNAME, CORES, MEMORY (MB), DISK (GB), STORAGE, BRIDGE.
# The container is unprivileged with nesting + keyctl, which is what Docker
# (and Pitlane's Docker-in-Docker slots) need.
set -euo pipefail

CTID="${CTID:-$(pvesh get /cluster/nextid)}"
HOSTNAME="${HOSTNAME:-pitlane}"
CORES="${CORES:-8}"
MEMORY="${MEMORY:-16384}"
DISK="${DISK:-150}"
STORAGE="${STORAGE:-local-lvm}"
BRIDGE="${BRIDGE:-vmbr0}"
TEMPLATE_STORAGE="${TEMPLATE_STORAGE:-local}"

command -v pct >/dev/null || { echo "Run this on a Proxmox VE host." >&2; exit 1; }

echo "==> Finding the latest Debian template"
pveam update >/dev/null
TEMPLATE="$(pveam available --section system | awk '/debian-13-standard/ {print $2}' | sort -V | tail -1)"
[ -n "$TEMPLATE" ] || TEMPLATE="$(pveam available --section system | awk '/debian-12-standard/ {print $2}' | sort -V | tail -1)"
pveam list "$TEMPLATE_STORAGE" | grep -q "$TEMPLATE" || pveam download "$TEMPLATE_STORAGE" "$TEMPLATE"

echo "==> Creating CT $CTID ($HOSTNAME): ${CORES} cores, ${MEMORY} MB RAM, ${DISK} GB disk on $STORAGE"
pct create "$CTID" "$TEMPLATE_STORAGE:vztmpl/$TEMPLATE" \
  --hostname "$HOSTNAME" --cores "$CORES" --memory "$MEMORY" --swap 512 \
  --rootfs "$STORAGE:$DISK" --net0 "name=eth0,bridge=$BRIDGE,ip=dhcp" \
  --unprivileged 1 --features nesting=1,keyctl=1 --onboot 1 --start 1

echo "==> Waiting for network"
for _ in $(seq 1 30); do
  pct exec "$CTID" -- sh -c 'getent hosts github.com >/dev/null' && break
  sleep 2
done

pct exec "$CTID" -- sh -c 'apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null'
pct exec "$CTID" -- sh -c 'curl -fsSL https://raw.githubusercontent.com/TuroYT/pitlane/main/deploy/install.sh | sh'
