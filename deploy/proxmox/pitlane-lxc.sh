#!/usr/bin/env bash
# Create a Proxmox VE container for Pitlane and install it inside.
# Run on the Proxmox host as root:
#
#   bash -c "$(curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/pitlane-lxc.sh)"
#
# This runs the community-scripts installer (ct/pitlane.sh,
# install/pitlane-install.sh) from the fork it is prepared in,
# TuroYT/ProxmoxVE. deploy/proxmox/{ct,install,json} mirror those files.
# You get the same menu (default or advanced settings) and the same overrides,
# for example:
#
#   var_cpu=4 var_ram=8192 var_disk=60 bash -c "$(curl -fsSL .../pitlane-lxc.sh)"
#
# Inside the container, `update` updates Pitlane, like any community script.
set -euo pipefail

command -v pct >/dev/null || { echo "Run this on a Proxmox VE host." >&2; exit 1; }

# The container's `update` command fetches ct/pitlane.sh from this base too.
export COMMUNITY_SCRIPTS_URL="${COMMUNITY_SCRIPTS_URL:-https://raw.githubusercontent.com/TuroYT/ProxmoxVE/feat/pitlane}"
exec bash -c "$(curl -fsSL "$COMMUNITY_SCRIPTS_URL/ct/pitlane.sh")"
