#!/usr/bin/env bash
# Installs hakobu on a Linux server: connects your Cloudflare account (you
# authorize in the browser), lets you pick the panel's domain and prints the
# link to connect GitHub. Needs a domain on Cloudflare; the server needs no
# public IP or open ports.
#
#   curl -fsSL https://raw.githubusercontent.com/x0ryz/hakobu/main/install.sh | sudo bash
#
# Optional: HAKOBU_VERSION (default: latest release), HAKOBU_REPO (default: x0ryz/hakobu),
#           HAKOBU_BINARY (install this local binary instead of downloading one)
set -euo pipefail

HAKOBU_REPO="${HAKOBU_REPO:-x0ryz/hakobu}"
DATA=/opt/hakobu/data

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root: curl -fsSL https://raw.githubusercontent.com/${HAKOBU_REPO}/main/install.sh | sudo bash"
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "unsupported CPU architecture $(uname -m)"; exit 1 ;;
esac

echo "==> installing dependencies (docker, git, railpack, buildkit)"
command -v curl >/dev/null || { apt-get update && apt-get install -y curl; } || yum install -y curl
command -v docker >/dev/null || curl -fsSL https://get.docker.com | sh
command -v git >/dev/null || { apt-get update && apt-get install -y git; } || yum install -y git
if ! command -v railpack >/dev/null; then
  curl -fsSL https://railpack.com/install.sh | bash -s -- --yes || echo "WARNING: railpack install failed, only Dockerfile builds will work"
fi
docker inspect buildkit >/dev/null 2>&1 || docker run --privileged -d --name buildkit --restart unless-stopped moby/buildkit >/dev/null

echo "==> installing hakobu to /opt/hakobu"
mkdir -p "$DATA"
chmod 700 "$DATA"
# The running binary can't be overwritten ("text file busy").
systemctl stop hakobu 2>/dev/null || true
VERSION="${HAKOBU_VERSION:-}"
if [ -z "${HAKOBU_BINARY:-}" ] && [ -z "$VERSION" ]; then
  VERSION="$( (curl -fsSL "https://api.github.com/repos/${HAKOBU_REPO}/releases/latest" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4) || true)"
fi
if [ -n "${HAKOBU_BINARY:-}" ]; then
  cp "$HAKOBU_BINARY" /opt/hakobu/hakobu
  echo "    local binary $HAKOBU_BINARY"
elif [ -n "$VERSION" ] && curl -fsSL "https://github.com/${HAKOBU_REPO}/releases/download/${VERSION}/hakobu-linux-${ARCH}" -o /opt/hakobu/hakobu.new; then
  # A download that doesn't match the release's checksum is never installed.
  SUMS="$(curl -fsSL "https://github.com/${HAKOBU_REPO}/releases/download/${VERSION}/checksums.txt")"
  WANT="$(printf '%s\n' "$SUMS" | awk -v f="hakobu-linux-${ARCH}" '$2 == f {print $1}')"
  GOT="$(sha256sum /opt/hakobu/hakobu.new | cut -d' ' -f1)"
  if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
    rm -f /opt/hakobu/hakobu.new
    echo "checksum mismatch for hakobu-linux-${ARCH} ${VERSION}, not installing it"
    exit 1
  fi
  mv /opt/hakobu/hakobu.new /opt/hakobu/hakobu
  echo "    hakobu $VERSION (checksum verified)"
else
  echo "==> no release binary found, building from source"
  GO_NEED="1.27.1"
  if ! command -v go >/dev/null || [ "$(printf '%s\n%s' "$GO_NEED" "$(go env GOVERSION | cut -c3-)" | sort -V | head -1)" != "$GO_NEED" ]; then
    curl -fsSL "https://go.dev/dl/go${GO_NEED}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz
    export PATH="/usr/local/go/bin:$PATH"
  fi
  SRC="$(mktemp -d)"
  git clone --depth 1 "https://github.com/${HAKOBU_REPO}.git" "$SRC"
  (cd "$SRC" && go build -o /opt/hakobu/hakobu .)
  rm -rf "$SRC"
fi
chmod +x /opt/hakobu/hakobu

echo "==> connecting Cloudflare"
# stdin is the script itself under curl | bash, so setup talks to the terminal.
(cd /opt/hakobu && ./hakobu setup) < /dev/tty

cat > /etc/systemd/system/hakobu.service <<'EOF'
[Unit]
Description=hakobu
After=docker.service network-online.target
Requires=docker.service

[Service]
WorkingDirectory=/opt/hakobu
ExecStart=/opt/hakobu/hakobu agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable hakobu >/dev/null 2>&1
systemctl restart hakobu

for _ in $(seq 1 30); do
  curl -fs -o /dev/null http://127.0.0.1:9000/login && break
  sleep 1
done

HOST="$(cat "$DATA/public_host")"
echo
if [ -s "$DATA/setup_token" ]; then
  echo "Done. The panel may take a minute to answer while DNS and the tunnel come up."
  echo "Open this link to connect GitHub and sign in (only this link can claim the panel):"
  echo
  echo "  https://$HOST/setup?token=$(cat "$DATA/setup_token")"
else
  echo "Done. Panel: https://$HOST"
fi
echo
