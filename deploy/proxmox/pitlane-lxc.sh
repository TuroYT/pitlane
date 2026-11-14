#!/usr/bin/env bash
# Create a Proxmox VE container ready for Pitlane and install it inside.
# Run on the Proxmox host as root:
#
#   bash -c "$(curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/pitlane-lxc.sh)"
#
# Overrides: CTID, HOSTNAME, CORES, MEMORY (MB), DISK (GB), STORAGE, BRIDGE,
# PITLANE_VERSION (default: latest, or e.g. 0.2.0).
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
PITLANE_VERSION="${PITLANE_VERSION:-latest}"
REPO="TuroYT/pitlane"

command -v pct >/dev/null || { echo "Run this on a Proxmox VE host." >&2; exit 1; }

# Everything comes from one release: resolve "latest" to its tag, and check
# install.sh against that release's checksums.txt before running it.
if [ "$PITLANE_VERSION" = latest ]; then
  LATEST="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")"
  TAG="${LATEST##*/}"
else
  TAG="v${PITLANE_VERSION#v}"
fi
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo "Invalid Pitlane version '$TAG' (is there a published release?)." >&2; exit 1; }
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "==> Downloading install.sh of Pitlane $TAG"
curl -fsSL "https://github.com/$REPO/releases/download/$TAG/install.sh" -o "$TMP/install.sh"
curl -fsSL "https://github.com/$REPO/releases/download/$TAG/checksums.txt" -o "$TMP/checksums.txt"
WANT="$(awk '$2 == "install.sh" || $2 == "*install.sh" {print $1}' "$TMP/checksums.txt")"
GOT="$(sha256sum "$TMP/install.sh" | awk '{print $1}')"
[ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || { echo "install.sh does not match checksums.txt of $TAG." >&2; exit 1; }

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
pct push "$CTID" "$TMP/install.sh" /root/pitlane-install.sh --perms 0755
echo "==> Installing Pitlane $TAG"
pct exec "$CTID" -- env PITLANE_VERSION="$TAG" sh /root/pitlane-install.sh
