#!/usr/bin/env bash

# Copyright (c) 2021-2026 community-scripts ORG
# Author: TuroYT
# License: MIT | https://github.com/community-scripts/ProxmoxVE/raw/main/LICENSE
# Source: https://github.com/TuroYT/pitlane

source /dev/stdin <<<"$FUNCTIONS_FILE_PATH"
color
verb_ip6
catch_errors
setting_up_container
network_check
update_os

setup_docker

fetch_and_deploy_gh_release "pitlane" "TuroYT/pitlane" "singlefile" "latest" "/usr/local/bin" "pitlane_linux_$(arch_resolve)"

msg_info "Configuring Pitlane"
mkdir -p /etc/pitlane /var/lib/pitlane
chmod 700 /var/lib/pitlane
LOCAL_IP=$(hostname -I | awk '{print $1}')
cat <<EOF >/etc/pitlane/pitlane.env
# Address you open in your browser (used for the GitHub App redirects).
PITLANE_BASE_URL=http://${LOCAL_IP}:8080
PITLANE_LISTEN=:8080
PITLANE_DATA=/var/lib/pitlane
EOF
msg_ok "Configured Pitlane"

msg_info "Creating Service"
cat <<EOF >/etc/systemd/system/pitlane.service
[Unit]
Description=Pitlane - autoscaled self-hosted GitHub Actions runners
Documentation=https://github.com/TuroYT/pitlane
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
EnvironmentFile=-/etc/pitlane/pitlane.env
ExecStart=/usr/local/bin/pitlane
Restart=always
RestartSec=5
KillMode=process
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
EOF
systemctl enable -q --now pitlane
msg_ok "Created Service"

msg_info "Waiting for the setup token"
TOKEN=""
for _ in {1..30}; do
  TOKEN=$(/usr/local/bin/pitlane setup-token /var/lib/pitlane 2>/dev/null || true)
  [[ -n "$TOKEN" ]] && break
  sleep 2
done
{
  echo "Pitlane"
  echo "URL: http://${LOCAL_IP}:8080"
  echo "Setup token: ${TOKEN:-run 'pitlane setup-token /var/lib/pitlane'}"
} >~/pitlane.creds
msg_ok "Saved the setup token to ~/pitlane.creds"

motd_ssh
customize
cleanup_lxc
