#!/usr/bin/env bash
_cs_boot="${COMMUNITY_SCRIPTS_CORE_DIR:-$(dirname "${BASH_SOURCE[0]}")/../../core}/core/build.func"
source "$_cs_boot" 2>/dev/null || source <(curl -fsSL "${COMMUNITY_SCRIPTS_CORE_URL:-https://raw.githubusercontent.com/community-scripts/core/main}/core/build.func")
# Copyright (c) 2021-2026 community-scripts ORG
# Author: TuroYT
# License: MIT | https://github.com/community-scripts/ProxmoxVE/raw/main/LICENSE
# Source: https://github.com/TuroYT/pitlane

APP="Pitlane"
var_tags="${var_tags:-ci;github;docker}"
var_cpu="${var_cpu:-8}"
var_ram="${var_ram:-16384}"
var_disk="${var_disk:-150}"
var_os="${var_os:-debian}"
var_version="${var_version:-13}"
var_arm64="${var_arm64:-yes}"
var_unprivileged="${var_unprivileged:-1}"
var_nesting="${var_nesting:-1}"
var_keyctl="${var_keyctl:-1}"

header_info "$APP"
variables
color
catch_errors

function update_script() {
  header_info
  check_container_storage
  check_container_resources
  if [[ ! -f /usr/local/bin/pitlane ]]; then
    msg_error "No ${APP} Installation Found!"
    exit
  fi

  if check_for_gh_release "pitlane" "TuroYT/pitlane"; then
    msg_info "Stopping Service"
    systemctl stop pitlane
    msg_ok "Stopped Service"

    # Busy runners are Docker containers: they keep running and Pitlane adopts them again on start.
    fetch_and_deploy_gh_release "pitlane" "TuroYT/pitlane" "singlefile" "latest" "/usr/local/bin" "pitlane_linux_$(arch_resolve)"

    msg_info "Starting Service"
    systemctl start pitlane
    msg_ok "Started Service"
    msg_ok "Updated successfully!"
  fi
  exit
}

start
build_container
description

msg_ok "Completed successfully!\n"
echo -e "${CREATING}${GN}${APP} setup has been successfully initialized!${CL}"
echo -e "${INFO}${YW} Access it using the following URL:${CL}"
echo -e "${TAB}${GATEWAY}${BGN}http://${IP}:8080${CL}"
echo -e "${INFO}${YW} The first-run setup token is in the container:${CL}"
echo -e "${TAB}${BGN}cat ~/pitlane.creds${CL}"
