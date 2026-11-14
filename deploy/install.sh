#!/bin/sh
# Pitlane installer: installs Docker (if missing), the pitlane binary and a
# systemd service, then prints the URL and setup token.
#
#   curl -fsSL https://raw.githubusercontent.com/TuroYT/pitlane/main/deploy/install.sh | sh
#
# Environment overrides: PITLANE_VERSION (default: latest), PITLANE_BASE_URL,
# PITLANE_PORT (default 8080).
set -eu

REPO="TuroYT/pitlane"
VERSION="${PITLANE_VERSION:-latest}"
PORT="${PITLANE_PORT:-8080}"

say() { printf '\033[1;31m==>\033[0m %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (or with sudo)"
command -v systemctl >/dev/null || die "systemd is required"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

if ! command -v docker >/dev/null 2>&1; then
  say "Installing Docker"
  curl -fsSL https://get.docker.com | sh
fi
systemctl enable --now docker >/dev/null 2>&1 || true
docker info >/dev/null 2>&1 || die "Docker is installed but not running"

if [ "$VERSION" = latest ]; then
  URL="https://github.com/$REPO/releases/latest/download/pitlane_linux_$ARCH"
else
  URL="https://github.com/$REPO/releases/download/v${VERSION#v}/pitlane_linux_$ARCH"
fi
say "Downloading $URL"
curl -fsSL "$URL" -o /usr/local/bin/pitlane.new
chmod 0755 /usr/local/bin/pitlane.new
mv /usr/local/bin/pitlane.new /usr/local/bin/pitlane

mkdir -p /etc/pitlane /var/lib/pitlane
chmod 700 /var/lib/pitlane
if [ ! -f /etc/pitlane/pitlane.env ]; then
  IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
  BASE="${PITLANE_BASE_URL:-http://${IP:-localhost}:$PORT}"
  cat > /etc/pitlane/pitlane.env <<ENV
# Address you open in your browser (used for GitHub redirects).
PITLANE_BASE_URL=$BASE
PITLANE_LISTEN=:$PORT
PITLANE_DATA=/var/lib/pitlane
ENV
fi

curl -fsSL "https://raw.githubusercontent.com/$REPO/main/deploy/pitlane.service" -o /etc/systemd/system/pitlane.service
systemctl daemon-reload
systemctl enable pitlane >/dev/null 2>&1
systemctl restart pitlane

. /etc/pitlane/pitlane.env
TOKEN=""
i=0
while [ $i -lt 20 ] && [ -z "$TOKEN" ]; do
  TOKEN="$(pitlane setup-token /var/lib/pitlane 2>/dev/null || true)"
  i=$((i + 1)); sleep 1
done

say "Pitlane $(pitlane version | cut -d' ' -f2) is running."
echo
echo "  Open:         $PITLANE_BASE_URL"
[ -n "$TOKEN" ] && echo "  Setup token:  $TOKEN"
echo
echo "  Logs:         journalctl -u pitlane -f"
echo "  Config:       /etc/pitlane/pitlane.env"
