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
#           HAKOBU_BINARY (install this local binary instead of downloading one),
#           HAKOBU_FROM_SOURCE=1 (build the main branch instead of a release),
#           HAKOBU_RESTORE=<key file> (bring back a panel from its backup, with the
#             key file from its Settings, instead of setting up a new one),
#           HAKOBU_ALLOW_UNSIGNED=1 (install a release from before releases were
#             signed, checked against its checksum only)
#
# Later updates: Settings → Updates in the panel, or sudo /opt/hakobu/hakobu update.
set -euo pipefail

HAKOBU_REPO="${HAKOBU_REPO:-x0ryz/hakobu}"
# Releases are signed (scripts/sign-release.sh); a download is installed
# only with a signature by this key over "hakobu <version>" and its
# checksums. The same key is in internal/update for `hakobu update`.
RELEASE_KEY="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAWBkGQ8EE1VzJYgZkPLxDY1JCviWclccA/tDM/FnQ0fI=
-----END PUBLIC KEY-----"
DATA=/opt/hakobu/data
KEY_DIR=/opt/hakobu/key # the master key, apart from data/

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

echo "==> installing dependencies (docker, git, openssl, railpack)"
command -v curl >/dev/null || { apt-get update && apt-get install -y curl; } || yum install -y curl
command -v openssl >/dev/null || { apt-get update && apt-get install -y openssl; } || yum install -y openssl
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
  echo "==> setting up rootless Docker for the hakobu-docker user"
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
  id hakobu-docker >/dev/null 2>&1 || useradd --system --create-home --home-dir /home/hakobu-docker --shell /usr/sbin/nologin hakobu-docker
  # hakobu reaches Docker's socket and the container dialer's through the
  # Docker user's group; the Docker user gets nothing of hakobu's.
  usermod -aG hakobu-docker hakobu
  DK_UID="$(id -u hakobu-docker)"
  DK_RUN="/run/user/$DK_UID"
  # Both sockets live here rather than in the Docker user's runtime
  # directory, which is private to it and recreated at every boot.
  SOCK_DIR=/run/hakobu-docker
  echo "d $SOCK_DIR 0750 hakobu-docker hakobu-docker -" > /etc/tmpfiles.d/hakobu-docker.conf
  systemd-tmpfiles --create /etc/tmpfiles.d/hakobu-docker.conf
  # Subordinate IDs for the containers' users, after every range in use.
  for f in /etc/subuid /etc/subgid; do
    touch "$f"
    if ! grep -q '^hakobu-docker:' "$f"; then
      echo "hakobu-docker:$(awk -F: 'BEGIN { m = 100000 } $2 + $3 > m { m = $2 + $3 } END { print m }' "$f"):65536" >> "$f"
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
  # Memory and CPU limits need the cgroup controllers delegated to the
  # Docker user's user manager.
  mkdir -p "/etc/systemd/system/user@$DK_UID.service.d"
  cat > "/etc/systemd/system/user@$DK_UID.service.d/delegate.conf" <<'EOF'
[Service]
Delegate=cpu cpuset io memory pids
EOF
  systemctl daemon-reload
  # Lingering starts the Docker user's manager (and its Docker) at boot.
  loginctl enable-linger hakobu-docker
  for _ in $(seq 1 30); do
    [ -S "$DK_RUN/bus" ] && break
    sleep 1
  done
  as_docker() {
    (cd / && runuser -u hakobu-docker -- env HOME=/home/hakobu-docker XDG_RUNTIME_DIR="$DK_RUN" DBUS_SESSION_BUS_ADDRESS="unix:path=$DK_RUN/bus" "$@")
  }
  if ! as_docker systemctl --user -q is-enabled docker 2>/dev/null; then
    as_docker dockerd-rootless-setuptool.sh install
  fi
  # Docker listens in the shared directory, its socket in group 0 of its
  # user namespace: the Docker user's own group on the host. The dialer runs
  # in RootlessKit's namespaces, which only the Docker user can enter, and
  # restarts with Docker, whose restart makes new ones.
  UNITS=/home/hakobu-docker/.config/systemd/user
  install -d -o hakobu-docker -g hakobu-docker "$UNITS/docker.service.d"
  cat > "$UNITS/docker.service.d/hakobu.conf" <<EOF
[Service]
ExecStart=
ExecStart=$(command -v dockerd-rootless.sh) -H unix://$SOCK_DIR/docker.sock --group 0
EOF
  cat > "$UNITS/hakobu-dialer.service" <<EOF
[Unit]
Description=hakobu's connections to containers
BindsTo=docker.service
After=docker.service

[Service]
ExecStart=/bin/sh -c 'exec nsenter -U --preserve-credentials -n -t "\$\$(cat "\$\$XDG_RUNTIME_DIR/dockerd-rootless/child_pid")" -- /opt/hakobu/hakobu dialer $SOCK_DIR/dialer.sock'
Restart=always
RestartSec=2

[Install]
WantedBy=docker.service
EOF
  chown -R hakobu-docker:hakobu-docker /home/hakobu-docker/.config
  as_docker systemctl --user daemon-reload
  as_docker systemctl --user -q enable docker hakobu-dialer
  as_docker systemctl --user restart docker
  for _ in $(seq 1 60); do
    as_docker env DOCKER_HOST="unix://$SOCK_DIR/docker.sock" docker info >/dev/null 2>&1 && break
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
mkdir -p "$DATA" "$KEY_DIR" /opt/hakobu/run
chmod 700 "$DATA" "$KEY_DIR"
if [ -z "$ROOTFUL" ]; then
  # hakobu writes only its data, its key and the panel socket's directory,
  # which cloudflared mounts; the binary stays root's.
  chown hakobu:hakobu "$DATA" "$KEY_DIR" /opt/hakobu/run
fi
# The running binary can't be overwritten ("text file busy").
systemctl stop hakobu 2>/dev/null || true
# A failed update leaves the installed hakobu running as it was.
abort_install() {
  rm -f /opt/hakobu/hakobu.new
  echo "$1"
  [ -x /opt/hakobu/hakobu ] && systemctl start hakobu 2>/dev/null
  exit 1
}
# A release is installed unless asked otherwise: falling back to building
# whatever the main branch holds when a lookup fails would quietly install
# unreleased code.
VERSION="${HAKOBU_VERSION:-}"
if [ -z "${HAKOBU_BINARY:-}" ] && [ -z "${HAKOBU_FROM_SOURCE:-}" ] && [ -z "$VERSION" ]; then
  VERSION="$( (curl -fsSL "https://api.github.com/repos/${HAKOBU_REPO}/releases/latest" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4) || true)"
  if [ -z "$VERSION" ]; then
    abort_install "couldn't find the latest hakobu release (GitHub API unreachable or rate-limited); set HAKOBU_VERSION, e.g. HAKOBU_VERSION=v0.4.0"
  fi
fi
if [ -n "${HAKOBU_BINARY:-}" ]; then
  cp "$HAKOBU_BINARY" /opt/hakobu/hakobu
  echo "    local binary $HAKOBU_BINARY"
elif [ -z "${HAKOBU_FROM_SOURCE:-}" ]; then
  RELEASE_URL="https://github.com/${HAKOBU_REPO}/releases/download/${VERSION}"
  if ! curl -fsSL "$RELEASE_URL/hakobu-linux-${ARCH}" -o /opt/hakobu/hakobu.new; then
    abort_install "couldn't download hakobu-linux-${ARCH} ${VERSION}"
  fi
  SIG_DIR="$(mktemp -d)"
  if ! curl -fsSL "$RELEASE_URL/checksums.txt" -o "$SIG_DIR/checksums.txt"; then
    rm -rf "$SIG_DIR"
    abort_install "couldn't download the checksums of ${VERSION}"
  fi
  # The signature covers "hakobu <version>" and the checksums, so neither
  # the files nor the version they're of can be swapped.
  CHECKED="checksum verified"
  if curl -fsSL "$RELEASE_URL/checksums.txt.sig" -o "$SIG_DIR/checksums.txt.sig" 2>/dev/null; then
    printf '%s\n' "$RELEASE_KEY" > "$SIG_DIR/release-key.pub"
    { printf 'hakobu %s\n' "$VERSION"; cat "$SIG_DIR/checksums.txt"; } > "$SIG_DIR/signed"
    if ! openssl pkeyutl -verify -rawin -pubin -inkey "$SIG_DIR/release-key.pub" -in "$SIG_DIR/signed" -sigfile "$SIG_DIR/checksums.txt.sig" >/dev/null 2>&1; then
      rm -rf "$SIG_DIR"
      abort_install "the signature of hakobu ${VERSION} isn't hakobu's, not installing it"
    fi
    CHECKED="signature and checksum verified"
  elif [ -z "${HAKOBU_ALLOW_UNSIGNED:-}" ]; then
    rm -rf "$SIG_DIR"
    abort_install "hakobu ${VERSION} isn't signed (released before signing, or the signature is missing); set HAKOBU_ALLOW_UNSIGNED=1 to install it checked against its checksum only"
  fi
  # A download that doesn't match the release's checksum is never installed.
  WANT="$(awk -v f="hakobu-linux-${ARCH}" '$2 == f {print $1}' "$SIG_DIR/checksums.txt")"
  rm -rf "$SIG_DIR"
  GOT="$(sha256sum /opt/hakobu/hakobu.new | cut -d' ' -f1)"
  if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
    abort_install "checksum mismatch for hakobu-linux-${ARCH} ${VERSION}, not installing it"
  fi
  mv /opt/hakobu/hakobu.new /opt/hakobu/hakobu
  echo "    hakobu $VERSION ($CHECKED)"
else
  echo "==> building hakobu from the main branch"
  GO_NEED="1.27.1"
  case "$ARCH" in
    amd64) GO_SHA256="63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445" ;;
    arm64) GO_SHA256="3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec" ;;
  esac
  if ! command -v go >/dev/null || [ "$(printf '%s\n%s' "$GO_NEED" "$(go env GOVERSION | cut -c3-)" | sort -V | head -1)" != "$GO_NEED" ]; then
    TMP="$(mktemp -d)"
    curl -fsSL "https://go.dev/dl/go${GO_NEED}.linux-${ARCH}.tar.gz" -o "$TMP/go.tar.gz"
    if ! echo "${GO_SHA256}  $TMP/go.tar.gz" | sha256sum -c --quiet -; then
      rm -rf "$TMP"
      abort_install "checksum mismatch for go${GO_NEED}, not installing it"
    fi
    tar -C /usr/local -xzf "$TMP/go.tar.gz"
    rm -rf "$TMP"
    export PATH="/usr/local/go/bin:$PATH"
  fi
  SRC="$(mktemp -d)"
  git clone --depth 1 "https://github.com/${HAKOBU_REPO}.git" "$SRC"
  (cd "$SRC" && go build -o /opt/hakobu/hakobu .)
  rm -rf "$SRC"
fi
chmod +x /opt/hakobu/hakobu
# What a rollback would put back belongs to an update this install
# replaced.
rm -f /opt/hakobu/hakobu.prev /opt/hakobu/update-state.json

if [ -n "${HAKOBU_RESTORE:-}" ]; then
  echo "==> restoring the panel from its backup"
  [ -r "$HAKOBU_RESTORE" ] || { echo "can't read the key file $HAKOBU_RESTORE"; exit 1; }
  if [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
    read -rsp "Cloudflare API token that can read R2 (it isn't echoed): " CLOUDFLARE_API_TOKEN < /dev/tty
    echo
  fi
  # The key file goes in on stdin: hakobu needn't be able to read where it is.
  if [ -n "$ROOTFUL" ]; then
    (cd /opt/hakobu && CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" ./hakobu restore --key -) < "$HAKOBU_RESTORE"
  else
    (cd /opt/hakobu && runuser -u hakobu -- env HOME=/home/hakobu CLOUDFLARE_API_TOKEN="$CLOUDFLARE_API_TOKEN" ./hakobu restore --key -) < "$HAKOBU_RESTORE"
  fi
fi

echo "==> connecting Cloudflare"
# stdin is the script itself under curl | bash, so setup talks to the terminal.
if [ -n "$ROOTFUL" ]; then
  (cd /opt/hakobu && ./hakobu setup) < /dev/tty
else
  # The dialer runs the binary installed just now.
  as_docker systemctl --user restart hakobu-dialer
  (cd /opt/hakobu && runuser -u hakobu -- env HOME=/home/hakobu DOCKER_HOST="unix://$SOCK_DIR/docker.sock" HAKOBU_DIALER_SOCKET="$SOCK_DIR/dialer.sock" CLOUDFLARE_API_TOKEN="${CLOUDFLARE_API_TOKEN:-}" ./hakobu setup) < /dev/tty
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
After=network-online.target user@$DK_UID.service
Requires=user@$DK_UID.service

[Service]
User=hakobu
Group=hakobu
WorkingDirectory=/opt/hakobu
Environment=DOCKER_HOST=unix://$SOCK_DIR/docker.sock
Environment=HAKOBU_DIALER_SOCKET=$SOCK_DIR/dialer.sock
# Docker is a user service of hakobu-docker's that starts alongside; the
# agent starts the tunnel once, so it waits for Docker to answer.
ExecStartPre=/bin/sh -c 'for i in \$\$(seq 1 120); do docker info >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1'
ExecStart=/opt/hakobu/hakobu agent
Restart=always
RestartSec=5
TimeoutStartSec=150

# Sandboxing. The container dialer is the Docker user's, so hakobu enters no
# namespaces. ProtectProc stays default so container ports can be read from
# /proc; PrivateUsers would hide the Docker user's group.
NoNewPrivileges=yes
RestrictNamespaces=yes
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/opt/hakobu/data /opt/hakobu/key /opt/hakobu/run /home/hakobu/.docker
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
# The panel's Update and Roll back buttons: hakobu, which can't write its
# own binary, asks through a file in data/ and systemd does the work as
# root. Each version writes its own units (internal/update/units.go).
if ! /opt/hakobu/hakobu install-units >/dev/null; then
  echo "    WARNING: no Update button: this hakobu can't install its updater (update with sudo /opt/hakobu/hakobu update)"
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
