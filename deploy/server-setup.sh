#!/usr/bin/env bash
set -euo pipefail

APP_DIR="${APP_DIR:-/opt/spring-code-1}"
APP_USER="${APP_USER:-springbot}"

if [ "$(id -u)" -ne 0 ]; then
  echo "Run as root or via sudo." >&2
  exit 1
fi

apt-get update
apt-get install -y ca-certificates curl gnupg ufw unzip

install -m 0755 -d /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  chmod a+r /etc/apt/keyrings/docker.gpg
fi

. /etc/os-release
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list

apt-get update
apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

systemctl enable --now docker

if ! id "$APP_USER" >/dev/null 2>&1; then
  useradd --system --create-home --shell /usr/sbin/nologin "$APP_USER"
fi
usermod -aG docker "$APP_USER" || true

mkdir -p "$APP_DIR"
chown -R "$APP_USER":"$APP_USER" "$APP_DIR"

if command -v ufw >/dev/null 2>&1; then
  ufw allow OpenSSH
  ufw --force enable
fi

echo "Server is ready."
echo "App directory: $APP_DIR"
echo "App user: $APP_USER"
