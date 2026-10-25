#!/usr/bin/env bash

# Copyright (c) 2021-2026 community-scripts ORG
# Author: jroosing
# License: MIT | https://github.com/community-scripts/ProxmoxVE/raw/main/LICENSE
# Source: https://github.com/jroosing/bitnews

source /dev/stdin <<<"$FUNCTIONS_FILE_PATH"
color
verb_ip6
catch_errors
setting_up_container
network_check
update_os

msg_info "Installing Dependencies"
$STD apt install -y ca-certificates tzdata
msg_ok "Installed Dependencies"

fetch_and_deploy_gh_release "bitnews" "jroosing/bitnews" "prebuild" "latest" "/opt/bitnews" "bitnews_*_linux_amd64.tar.gz"

msg_info "Configuring bitnews"
# bitnews runs as its own user (fixed ids 1000:1000, so the ids on the Proxmox
# host are predictable: 101000 in an unprivileged container) and looks for 7zz
# and par2 on PATH.
groupadd --gid 1000 bitnews
useradd --uid 1000 --gid 1000 --home-dir /var/lib/bitnews --create-home --shell /usr/sbin/nologin bitnews
ln -sf /opt/bitnews/7zz /usr/local/bin/7zz
ln -sf /opt/bitnews/par2 /usr/local/bin/par2

# The default folders of Settings > Paths, in the TRaSH layout. Mount your
# media storage over /data (Proxmox: Resources > Add > Mount Point).
mkdir -p /data/usenet/incomplete /data/usenet/complete /data/torrents
chown -R bitnews:bitnews /data

# The admin account is created from this file on the first start, so there is
# no setup screen. The file is removed again once bitnews is up.
ADMIN_PASS="$(openssl rand -base64 24 | tr -d '/+=')"
install -d -m 0750 -o root -g bitnews /etc/bitnews
(umask 077 && echo "$ADMIN_PASS" >/etc/bitnews/admin-password)
chown bitnews:bitnews /etc/bitnews/admin-password
cat <<EOT >/etc/bitnews/bitnews.env
BITNEWS_CONFIG_DIR=/var/lib/bitnews
BITNEWS_LISTEN=0.0.0.0:9494
UMASK=002
BITNEWS_ADMIN_PASSWORD_FILE=/etc/bitnews/admin-password
EOT
{
  echo "bitnews Credentials"
  echo "Username: admin"
  echo "Password: $ADMIN_PASS"
} >~/bitnews.creds
chmod 600 ~/bitnews.creds
msg_ok "Configured bitnews"

msg_info "Creating Service"
cat <<EOT >/etc/systemd/system/bitnews.service
[Unit]
Description=bitnews download daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=bitnews
Group=bitnews
EnvironmentFile=/etc/bitnews/bitnews.env
ExecStart=/opt/bitnews/bitnews
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOT
systemctl enable -q --now bitnews
for _ in $(seq 1 30); do
  if curl -fs -o /dev/null http://127.0.0.1:9494/api/v1/health; then
    # bitnews creates the admin before it serves, so the password file is not needed any more.
    sed -i '/^BITNEWS_ADMIN_PASSWORD_FILE=/d' /etc/bitnews/bitnews.env
    rm -f /etc/bitnews/admin-password
    break
  fi
  sleep 1
done
msg_ok "Created Service"

motd_ssh
customize
cleanup_lxc
