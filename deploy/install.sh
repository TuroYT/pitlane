#!/bin/sh
# Pitlane installer: installs Docker (if missing), the pitlane binary and a
# systemd service, then prints the URL and setup token.
#
#   curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/install.sh | sudo sh
#
# Environment overrides: PITLANE_VERSION (default: latest, or e.g. 0.2.0),
# PITLANE_BASE_URL, PITLANE_PORT (default 8080).
#
# Everything comes from one release: "latest" is resolved to its tag first,
# and the binary is checked against that release's checksums.txt.
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
  # github.com/<repo>/releases/latest redirects to .../releases/tag/<tag>.
  LATEST="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")" \
    || die "cannot resolve the latest release"
  TAG="${LATEST##*/}"
else
  TAG="v${VERSION#v}"
fi
case "$TAG" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) die "invalid version '$TAG' (expected e.g. 0.2.0; is there a published release?)" ;;
esac
case "$TAG" in
  *[!0-9A-Za-z.-]*) die "invalid version '$TAG'" ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "sha256sum (coreutils) or shasum is required to verify the download"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
trap 'exit 1' INT TERM
BASE_URL="https://github.com/$REPO/releases/download/$TAG"
FILE="pitlane_linux_$ARCH"
say "Downloading Pitlane $TAG ($FILE)"
curl -fsSL "$BASE_URL/$FILE" -o "$TMP/$FILE" || die "download of $BASE_URL/$FILE failed"
curl -fsSL "$BASE_URL/checksums.txt" -o "$TMP/checksums.txt" || die "download of $BASE_URL/checksums.txt failed"
WANT="$(awk -v f="$FILE" '$2 == f || $2 == "*" f {print $1}' "$TMP/checksums.txt")"
[ -n "$WANT" ] || die "$FILE is not listed in checksums.txt of $TAG"
GOT="$(sha256 "$TMP/$FILE")"
[ "$GOT" = "$WANT" ] || die "checksum mismatch for $FILE: expected $WANT, got $GOT"
say "Checksum verified ($GOT)"

install -m 0755 "$TMP/$FILE" /usr/local/bin/pitlane.new
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

# Same content as deploy/pitlane.service (keep them in sync).
cat > /etc/systemd/system/pitlane.service <<'UNIT'
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
# Busy runners keep running during a restart and are adopted again.
KillMode=process
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
UNIT
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
