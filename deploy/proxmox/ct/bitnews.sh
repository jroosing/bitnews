#!/usr/bin/env bash
_cs_boot="${COMMUNITY_SCRIPTS_CORE_DIR:-$(dirname "${BASH_SOURCE[0]}")/../../core}/core/build.func"
source "$_cs_boot" 2>/dev/null || source <(curl -fsSL "${COMMUNITY_SCRIPTS_CORE_URL:-https://raw.githubusercontent.com/community-scripts/core/main}/core/build.func")
# Copyright (c) 2021-2026 community-scripts ORG
# Author: jroosing
# License: MIT | https://github.com/community-scripts/ProxmoxVE/raw/main/LICENSE
# Source: https://github.com/jroosing/bitnews

APP="bitnews"
var_tags="${var_tags:-arr;usenet;torrent}"
var_cpu="${var_cpu:-2}"
var_ram="${var_ram:-2048}"
var_disk="${var_disk:-8}"
var_os="${var_os:-debian}"
var_version="${var_version:-13}"
var_unprivileged="${var_unprivileged:-1}"

header_info "$APP"
variables
color
catch_errors

function update_script() {
  header_info
  check_container_storage
  check_container_resources
  if [[ ! -x /opt/bitnews/bitnews ]]; then
    msg_error "No ${APP} Installation Found!"
    exit
  fi

  if check_for_gh_release "bitnews" "jroosing/bitnews"; then
    msg_info "Stopping Service"
    systemctl stop bitnews
    msg_ok "Stopped Service"

    # The release holds bitnews and the pinned 7zz and par2, so one download
    # updates all three. The database and master key live in /var/lib/bitnews.
    fetch_and_deploy_gh_release "bitnews" "jroosing/bitnews" "prebuild" "latest" "/opt/bitnews" "bitnews_*_linux_amd64.tar.gz"
    ln -sf /opt/bitnews/7zz /usr/local/bin/7zz
    ln -sf /opt/bitnews/par2 /usr/local/bin/par2

    msg_info "Starting Service"
    systemctl start bitnews
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
echo -e "${INFO}${YW}Access it using the following URL:${CL}"
echo -e "${GATEWAY}${BGN}http://${IP}:9494${CL}"
echo -e "${INFO}${YW}The admin login is in ${BGN}/root/bitnews.creds${YW} inside the container.${CL}"
echo -e "${INFO}${YW}Torrent peers connect to port 51413 (TCP and UDP): forward it on your router.${CL}"
