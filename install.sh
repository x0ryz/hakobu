#!/usr/bin/env bash
# Installs hakobu on a Linux server: connects your Cloudflare account (you
# create an API token from a prefilled link and paste it), lets you pick the
# panel's domain and prints the link to connect GitHub. Needs a domain on Cloudflare; the server needs no
# public IP or open ports.
#
# hakobu runs as the unprivileged user "hakobu" with its own rootless Docker,
# so neither a break-in into hakobu nor a container escape is root on the
# server. Installs made before that keep running as root under the system
# Docker (their containers and databases live there); this script only
# updates their binary.
#
#   curl -fsSL https://hakobu.dev/install.sh | sudo bash
#
# Optional: CLOUDFLARE_API_TOKEN (skips the token prompt),
#           HAKOBU_VERSION (default: latest release), HAKOBU_REPO (default: x0ryz/hakobu),
#           HAKOBU_BINARY (install this local binary instead of downloading one)
set -euo pipefail

HAKOBU_REPO="${HAKOBU_REPO:-x0ryz/hakobu}"
DATA=/opt/hakobu/data

if [ "$(id -u)" -ne 0 ]; then
  if [ "$HAKOBU_REPO" = "x0ryz/hakobu" ]; then
    echo "run as root: curl -fsSL https://hakobu.dev/install.sh | sudo bash"
  else
    echo "run as root: curl -fsSL https://raw.githubusercontent.com/${HAKOBU_REPO}/main/install.sh | sudo bash"
  fi
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "unsupported CPU architecture $(uname -m)"; exit 1 ;;
esac

UNIT=/etc/systemd/system/hakobu.service
ROOTFUL=""
if [ -f "$UNIT" ] && ! grep -q '^User=hakobu$' "$UNIT"; then
  ROOTFUL=1
fi

echo "==> installing dependencies (docker, git, railpack)"
command -v curl >/dev/null || { apt-get update && apt-get install -y curl; } || yum install -y curl
HAD_DOCKER=""
if command -v docker >/dev/null; then
  HAD_DOCKER=1
else
  curl -fsSL https://get.docker.com | sh
fi
command -v git >/dev/null || { apt-get update && apt-get install -y git; } || yum install -y git
# Railpack is pinned and checked like hakobu itself; hakobu starts the
# BuildKit container it builds with (config/images.go) on the first build.
RAILPACK_VERSION="v0.40.1"
case "$ARCH" in
  amd64) RAILPACK_TARGET="x86_64-unknown-linux-musl"; RAILPACK_SHA256="2842de93e68713af9037e0bc0a398d7da78f3b96aa4804303a638db2bc69bd30" ;;
  arm64) RAILPACK_TARGET="arm64-unknown-linux-musl"; RAILPACK_SHA256="c24a064b586b8f4f8c2fab44dd5ef19253e4c6cc4e1df793b3ae19cd87f7a5d4" ;;
esac
if [ "$(railpack --version 2>/dev/null | grep -o 'v\?[0-9][0-9.]*' | head -1 | sed 's/^v*/v/')" != "$RAILPACK_VERSION" ]; then
  TMP="$(mktemp -d)"
  if curl -fsSL "https://github.com/railwayapp/railpack/releases/download/${RAILPACK_VERSION}/railpack-${RAILPACK_VERSION}-${RAILPACK_TARGET}.tar.gz" -o "$TMP/railpack.tar.gz" \
    && echo "${RAILPACK_SHA256}  $TMP/railpack.tar.gz" | sha256sum -c --quiet - \
    && tar -xzf "$TMP/railpack.tar.gz" -C "$TMP" railpack; then
    install -m 0755 "$TMP/railpack" /usr/local/bin/railpack
  else
    echo "WARNING: railpack install failed, only Dockerfile builds will work"
  fi
  rm -rf "$TMP"
fi

if [ -z "$ROOTFUL" ]; then
  echo "==> setting up rootless Docker for the hakobu user"
  if command -v apt-get >/dev/null; then
    apt-get install -y uidmap dbus-user-session docker-ce-rootless-extras >/dev/null
  else
    yum install -y shadow-utils docker-ce-rootless-extras >/dev/null
  fi
  if ! command -v dockerd-rootless-setuptool.sh >/dev/null; then
    echo "rootless Docker isn't available: hakobu needs Docker's own packages (https://docs.docker.com/engine/install/), not the distribution's"
    exit 1
  fi
  id hakobu >/dev/null 2>&1 || useradd --system --create-home --home-dir /home/hakobu --shell /usr/sbin/nologin hakobu
  HK_UID="$(id -u hakobu)"
  HK_RUN="/run/user/$HK_UID"
  # Subordinate IDs for the containers' users, after every range in use.
  for f in /etc/subuid /etc/subgid; do
    touch "$f"
    if ! grep -q '^hakobu:' "$f"; then
      echo "hakobu:$(awk -F: 'BEGIN { m = 100000 } $2 + $3 > m { m = $2 + $3 } END { print m }' "$f"):65536" >> "$f"
    fi
  done
  # Ubuntu 24.04+ lets unprivileged users create user namespaces only
  # through an AppArmor profile. Ubuntu ships one for /usr/bin/rootlesskit;
  # elsewhere this adds one for rootlesskit alone, as Docker's docs
  # describe, instead of lifting the limit for everything.
  ROOTLESSKIT="$(command -v rootlesskit)"
  if [ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null)" = 1 ] && command -v apparmor_parser >/dev/null \
    && ! grep -qsF "$ROOTLESSKIT" /etc/apparmor.d/*; then
    cat > /etc/apparmor.d/usr.bin.rootlesskit <<EOF
abi <abi/4.0>,
include <tunables/global>

"$ROOTLESSKIT" flags=(unconfined) {
  userns,

  include if exists <local/usr.bin.rootlesskit>
}
EOF
    apparmor_parser -r /etc/apparmor.d/usr.bin.rootlesskit
  fi
  # Memory and CPU limits need the cgroup controllers delegated to hakobu's
  # user manager.
  mkdir -p "/etc/systemd/system/user@$HK_UID.service.d"
  cat > "/etc/systemd/system/user@$HK_UID.service.d/delegate.conf" <<'EOF'
[Service]
Delegate=cpu cpuset io memory pids
EOF
  systemctl daemon-reload
  # Lingering starts hakobu's user manager (and its Docker) at boot.
  loginctl enable-linger hakobu
  for _ in $(seq 1 30); do
    [ -S "$HK_RUN/bus" ] && break
    sleep 1
  done
  as_hakobu() {
    (cd / && runuser -u hakobu -- env HOME=/home/hakobu XDG_RUNTIME_DIR="$HK_RUN" DBUS_SESSION_BUS_ADDRESS="unix:path=$HK_RUN/bus" "$@")
  }
  if ! as_hakobu systemctl --user -q is-active docker; then
    as_hakobu dockerd-rootless-setuptool.sh install
  fi
  as_hakobu systemctl --user -q enable docker
  for _ in $(seq 1 60); do
    as_hakobu env DOCKER_HOST="unix://$HK_RUN/docker.sock" docker info >/dev/null 2>&1 && break
    sleep 1
  done
  # A system Docker installed just now would sit unused as a root-equivalent
  # socket; one that was there before may serve something else, so it stays.
  if [ -z "$HAD_DOCKER" ]; then
    systemctl disable --now docker.service docker.socket >/dev/null 2>&1 || true
    rm -f /var/run/docker.sock # left behind, nothing listens on it
  fi
  # The docker CLI keeps build state here, the one place in the home the
  # service may write.
  install -d -o hakobu -g hakobu -m 0700 /home/hakobu/.docker
fi

echo "==> installing hakobu to /opt/hakobu"
mkdir -p "$DATA" /opt/hakobu/run
chmod 700 "$DATA"
if [ -z "$ROOTFUL" ]; then
  # hakobu writes only its data and the panel socket's directory, which
  # cloudflared mounts; the binary stays root's.
  chown hakobu:hakobu "$DATA" /opt/hakobu/run
fi
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
if [ -n "$ROOTFUL" ]; then
  (cd /opt/hakobu && ./hakobu setup) < /dev/tty
else
  (cd /opt/hakobu && runuser -u hakobu -- env HOME=/home/hakobu XDG_RUNTIME_DIR="$HK_RUN" DOCKER_HOST="unix://$HK_RUN/docker.sock" CLOUDFLARE_API_TOKEN="${CLOUDFLARE_API_TOKEN:-}" ./hakobu setup) < /dev/tty
fi

if [ -n "$ROOTFUL" ]; then
  echo "    this install runs as root under the system Docker; to move to rootless Docker, install hakobu on a clean server"
  cat > "$UNIT" <<'EOF'
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
else
  cat > "$UNIT" <<EOF
[Unit]
Description=hakobu
Wants=network-online.target
After=network-online.target user@$HK_UID.service
Requires=user@$HK_UID.service

[Service]
User=hakobu
Group=hakobu
WorkingDirectory=/opt/hakobu
Environment=XDG_RUNTIME_DIR=$HK_RUN
Environment=DOCKER_HOST=unix://$HK_RUN/docker.sock
# Docker is a user service of hakobu's that starts alongside; the agent
# starts the tunnel once, so it waits for Docker to answer.
ExecStartPre=/bin/sh -c 'for i in \$\$(seq 1 120); do docker info >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1'
ExecStart=/opt/hakobu/hakobu agent
Restart=always
RestartSec=5
TimeoutStartSec=150

# Sandboxing. The container dialer enters rootless Docker's namespaces with
# nsenter, so RestrictNamespaces, PrivateUsers and SystemCallFilter stay off;
# ProtectProc stays default so container ports can be read from /proc.
NoNewPrivileges=yes
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/opt/hakobu/data /opt/hakobu/run /home/hakobu/.docker
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
fi
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
