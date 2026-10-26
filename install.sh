#!/usr/bin/env bash
# Installs hakobu v1 on a Linux server (Debian or Ubuntu), or updates it:
# rootless Docker for the apps, Tailscale for the API (or none), the server's key
# and config, and the hakobu service. Nothing asks: everything comes from
# the environment, so cloud-init or an agent can run it.
#
#   curl -fsSL https://hakobu.dev/install.sh | sudo \
#     HAKOBU_NAME=kasl-1 HAKOBU_OWNER_KEY=age1… TS_AUTHKEY=tskey-auth-… bash
#
# Needs, on a first install:
#   HAKOBU_NAME        the server's name (lowercase letters, digits, -)
#   HAKOBU_OWNER_KEY   your age public key: secrets and backups are sealed to
#                      it too, so you can open them without the server
# Optional:
#   TS_AUTHKEY         a Tailscale auth key: the API listens on the tailnet
#                      and nowhere else. Without one (and a server not in a
#                      tailnet already), it listens on 127.0.0.1, reached
#                      through SSH (ssh -L), and Tailscale isn't installed
#   HAKOBU_API         tailnet or loopback, to say which outright
#   TS_TAGS            the server's Tailscale tags, e.g. tag:hakobu
#   HAKOBU_INGRESS     public (80/443 with Let's Encrypt), tunnel (a Cloudflare
#                      Tunnel to loopback) or none (default)
#   HAKOBU_ACME_EMAIL  Let's Encrypt's contact, for public
#   CLOUDFLARE_TUNNEL_TOKEN  the tunnel's token, for tunnel: cloudflared runs
#                      on the server as a system service
#   HAKOBU_CONFIG_EXTRA  a file whose TOML goes into the config, after its
#                      top-level keys:
#                      releases, registries, backups, alerts
#   HAKOBU_VERSION     a release (default: the latest, pre-releases included)
#   HAKOBU_BINARY      install this local binary instead of downloading one
#   HAKOBU_REPO        default hakobudev/hakobu
#
# Run again to update: the config, the key and the data stay.
set -euo pipefail

HAKOBU_REPO="${HAKOBU_REPO:-hakobudev/hakobu}"
# Releases are signed (scripts/sign-release.sh); a download is installed
# only with a signature by this key over "hakobu <version>" and its
# checksums.
RELEASE_KEY="-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAWBkGQ8EE1VzJYgZkPLxDY1JCviWclccA/tDM/FnQ0fI=
-----END PUBLIC KEY-----"
BIN=/opt/hakobu/hakobu
DATA=/var/lib/hakobu
CONFIG=/etc/hakobu/server.toml
UNIT=/etc/systemd/system/hakobu.service
API_PORT=7070

die() { echo "error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU architecture $(uname -m)" ;;
esac
command -v apt-get >/dev/null || die "this installs on Debian or Ubuntu"
if [ -f "$UNIT" ] && grep -q 'hakobu agent\|hakobu node' "$UNIT"; then
  die "this server runs hakobu v0 (the panel): v1 goes on a new server, and its projects move there (see the docs)"
fi

FIRST=""
if [ ! -f "$CONFIG" ]; then
  FIRST=1
  API="${HAKOBU_API:-}"
  if [ -z "$API" ]; then
    if [ -n "${TS_AUTHKEY:-}" ] || { command -v tailscale >/dev/null && tailscale ip -4 >/dev/null 2>&1; }; then API=tailnet; else API=loopback; fi
  fi
  case "$API" in tailnet|loopback) ;; *) die "HAKOBU_API: tailnet or loopback" ;; esac
  [[ "${HAKOBU_NAME:-}" =~ ^[a-z][a-z0-9-]{0,30}$ ]] || die "HAKOBU_NAME: a name of lowercase letters, digits and -"
  [[ "${HAKOBU_OWNER_KEY:-}" =~ ^age1[a-z0-9]{58}$ ]] || die "HAKOBU_OWNER_KEY: your age public key (age1…); age-keygen makes a key pair, keep its secret half in your password manager"
  case "${HAKOBU_INGRESS:-none}" in public|tunnel|none) ;; *) die "HAKOBU_INGRESS: public, tunnel or none" ;; esac
  if [ "${HAKOBU_INGRESS:-none}" = tunnel ] && [ -z "${CLOUDFLARE_TUNNEL_TOKEN:-}" ]; then
    die "HAKOBU_INGRESS=tunnel needs CLOUDFLARE_TUNNEL_TOKEN"
  fi
  if [ -n "${HAKOBU_CONFIG_EXTRA:-}" ] && [ ! -r "$HAKOBU_CONFIG_EXTRA" ]; then
    die "can't read HAKOBU_CONFIG_EXTRA $HAKOBU_CONFIG_EXTRA"
  fi
fi

echo "==> packages"
export DEBIAN_FRONTEND=noninteractive
apt-get -o DPkg::Lock::Timeout=600 update -qq
apt-get -o DPkg::Lock::Timeout=600 install -y -qq curl openssl ca-certificates uidmap dbus-user-session unattended-upgrades >/dev/null
# Security updates install themselves; hakobu's audit checks they do.
dpkg-reconfigure -f noninteractive unattended-upgrades >/dev/null 2>&1 || true

echo "==> Docker, rootless, for the hakobu-docker user"
HAD_DOCKER=""
if command -v docker >/dev/null; then HAD_DOCKER=1; else curl -fsSL https://get.docker.com | sh >/dev/null; fi
apt-get -o DPkg::Lock::Timeout=600 install -y -qq docker-ce-rootless-extras >/dev/null
command -v dockerd-rootless-setuptool.sh >/dev/null || die "rootless Docker isn't available: hakobu needs Docker's own packages, not the distribution's"
id hakobu >/dev/null 2>&1 || useradd --system --create-home --home-dir /home/hakobu --shell /usr/sbin/nologin hakobu
id hakobu-docker >/dev/null 2>&1 || useradd --system --create-home --home-dir /home/hakobu-docker --shell /usr/sbin/nologin hakobu-docker
# hakobu reaches Docker's socket and the container dialer's through the
# Docker user's group; the Docker user gets nothing of hakobu's.
usermod -aG hakobu-docker hakobu
DK_UID="$(id -u hakobu-docker)"
DK_RUN="/run/user/$DK_UID"
SOCK_DIR=/run/hakobu-docker
echo "d $SOCK_DIR 0750 hakobu-docker hakobu-docker -" > /etc/tmpfiles.d/hakobu-docker.conf
systemd-tmpfiles --create /etc/tmpfiles.d/hakobu-docker.conf
for f in /etc/subuid /etc/subgid; do
  touch "$f"
  grep -q '^hakobu-docker:' "$f" || echo "hakobu-docker:$(awk -F: 'BEGIN { m = 100000 } $2 + $3 > m { m = $2 + $3 } END { print m }' "$f"):65536" >> "$f"
done
# Ubuntu 24.04+ lets unprivileged users make user namespaces only through
# an AppArmor profile: one for rootlesskit alone, as Docker's docs say.
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
# Memory and CPU limits need the cgroup controllers delegated.
mkdir -p "/etc/systemd/system/user@$DK_UID.service.d"
printf '[Service]\nDelegate=cpu cpuset io memory pids\n' > "/etc/systemd/system/user@$DK_UID.service.d/delegate.conf"
systemctl daemon-reload
loginctl enable-linger hakobu-docker
for _ in $(seq 1 30); do [ -S "$DK_RUN/bus" ] && break; sleep 1; done
as_docker() {
  (cd / && runuser -u hakobu-docker -- env HOME=/home/hakobu-docker XDG_RUNTIME_DIR="$DK_RUN" DBUS_SESSION_BUS_ADDRESS="unix:path=$DK_RUN/bus" "$@")
}
as_docker systemctl --user -q is-enabled docker 2>/dev/null || as_docker dockerd-rootless-setuptool.sh install >/dev/null
UNITS=/home/hakobu-docker/.config/systemd/user
install -d -o hakobu-docker -g hakobu-docker "$UNITS/docker.service.d"
DOCKER_CONF="[Service]
ExecStart=
ExecStart=$(command -v dockerd-rootless.sh) -H unix://$SOCK_DIR/docker.sock --group 0"
# Restarting Docker restarts every app: only when its settings change.
RESTART_DOCKER=""
if [ "$(cat "$UNITS/docker.service.d/hakobu.conf" 2>/dev/null)" != "$DOCKER_CONF" ]; then
  printf '%s\n' "$DOCKER_CONF" > "$UNITS/docker.service.d/hakobu.conf"
  RESTART_DOCKER=1
fi
# The dialer runs in RootlessKit's namespaces, which only the Docker user
# can enter, and restarts with Docker.
# It waits for Docker to answer, so the namespaces it enters are those of
# the Docker running now, and keeps trying: without it hakobu can't reach
# a container.
cat > "$UNITS/hakobu-dialer.service" <<EOF
[Unit]
Description=hakobu's connections to containers
BindsTo=docker.service
After=docker.service
StartLimitIntervalSec=0

[Service]
Environment=DOCKER_HOST=unix://$SOCK_DIR/docker.sock
ExecStartPre=/bin/sh -c 'for i in \$\$(seq 1 60); do docker info >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1'
# Docker 29's RootlessKit keeps the containers' network in a namespace of
# its own (--detach-netns), bound at netns in its mount namespace, which
# /proc/<pid>/root reaches without leaving the host's (where the sockets
# are). Older ones have it as the child process's.
ExecStart=/bin/sh -c 'R="\$\$XDG_RUNTIME_DIR/dockerd-rootless"; P="\$\$(cat "\$\$R/child_pid")"; N="/proc/\$\$P/root\$\$R/netns"; if nsenter -U --preserve-credentials -t "\$\$P" --net="\$\$N" -- true 2>/dev/null; then exec nsenter -U --preserve-credentials -t "\$\$P" --net="\$\$N" -- $BIN dialer $SOCK_DIR/dialer.sock; fi; exec nsenter -U --preserve-credentials -n -t "\$\$P" -- $BIN dialer $SOCK_DIR/dialer.sock'
Restart=always
RestartSec=2

[Install]
WantedBy=docker.service
EOF
chown -R hakobu-docker:hakobu-docker /home/hakobu-docker/.config
as_docker systemctl --user daemon-reload
as_docker systemctl --user -q enable docker hakobu-dialer
if [ -n "$RESTART_DOCKER" ]; then
  as_docker systemctl --user restart docker
else
  as_docker systemctl --user start docker
fi
for _ in $(seq 1 60); do
  as_docker env DOCKER_HOST="unix://$SOCK_DIR/docker.sock" docker info >/dev/null 2>&1 && break
  sleep 1
done
# A system Docker installed just now would sit unused as a root-equivalent
# socket; one that was there before may serve something else.
if [ -z "$HAD_DOCKER" ]; then
  systemctl disable --now docker.service docker.socket >/dev/null 2>&1 || true
  rm -f /var/run/docker.sock
fi
install -d -o hakobu -g hakobu -m 0700 /home/hakobu/.docker

# An update keeps the API where the config has it.
[ -n "$FIRST" ] || { grep -q '^listen = "127\.0\.0\.1:' "$CONFIG" && API=loopback || API=tailnet; }
if [ "$API" = loopback ]; then
  API_IP=127.0.0.1
else
  echo "==> Tailscale"
  command -v tailscale >/dev/null || curl -fsSL https://tailscale.com/install.sh | sh >/dev/null
  API_IP="$(tailscale ip -4 2>/dev/null | head -1 || true)"
  if [ -z "$API_IP" ]; then
    [ -n "${TS_AUTHKEY:-}" ] || die "the server isn't in a tailnet: set TS_AUTHKEY, or HAKOBU_API=loopback"
    # Nothing from the tailnet changes the server: no routes, no DNS, no SSH.
    tailscale up --authkey="$TS_AUTHKEY" --hostname="hakobu-${HAKOBU_NAME:-server}" \
      ${TS_TAGS:+--advertise-tags="$TS_TAGS"} --accept-routes=false --accept-dns=false --ssh=false
    API_IP="$(tailscale ip -4 | head -1)"
  fi
  echo "    $API_IP"
fi

echo "==> hakobu"
install -d -m 0755 /opt/hakobu
if [ -n "${HAKOBU_BINARY:-}" ]; then
  install -m 0755 "$HAKOBU_BINARY" "$BIN.new"
  echo "    local binary $HAKOBU_BINARY"
else
  VERSION="${HAKOBU_VERSION:-}"
  if [ -z "$VERSION" ]; then
    # v1 is released as pre-releases until v1.0.0: the newest release of
    # any kind, not GitHub's latest (v0's).
    VERSION="$(curl -fsSL "https://api.github.com/repos/$HAKOBU_REPO/releases?per_page=20" | grep -o '"tag_name": *"v1[^"]*"' | head -1 | cut -d'"' -f4)" \
      || die "couldn't find the newest hakobu v1 release; set HAKOBU_VERSION"
    [ -n "$VERSION" ] || die "no hakobu v1 release yet; set HAKOBU_BINARY"
  fi
  URL="https://github.com/$HAKOBU_REPO/releases/download/$VERSION"
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  curl -fsSL "$URL/hakobu-linux-$ARCH" -o "$TMP/hakobu" || die "couldn't download hakobu $VERSION"
  curl -fsSL "$URL/checksums.txt" -o "$TMP/checksums.txt" || die "couldn't download the checksums of $VERSION"
  curl -fsSL "$URL/checksums.txt.sig" -o "$TMP/checksums.txt.sig" || die "hakobu $VERSION isn't signed"
  printf '%s\n' "$RELEASE_KEY" > "$TMP/key.pub"
  { printf 'hakobu %s\n' "$VERSION"; cat "$TMP/checksums.txt"; } > "$TMP/signed"
  openssl pkeyutl -verify -rawin -pubin -inkey "$TMP/key.pub" -in "$TMP/signed" -sigfile "$TMP/checksums.txt.sig" >/dev/null 2>&1 \
    || die "the signature of hakobu $VERSION isn't hakobu's, not installing it"
  WANT="$(awk -v f="hakobu-linux-$ARCH" '$2 == f {print $1}' "$TMP/checksums.txt")"
  [ -n "$WANT" ] && [ "$WANT" = "$(sha256sum "$TMP/hakobu" | cut -d' ' -f1)" ] || die "checksum mismatch for hakobu $VERSION, not installing it"
  install -m 0755 "$TMP/hakobu" "$BIN.new"
  echo "    hakobu $VERSION (signature and checksum verified)"
fi
# Renamed over the old one: a running binary can't be written.
mv "$BIN.new" "$BIN"

OP_TOKEN="" ADMIN_TOKEN=""
if [ -n "$FIRST" ]; then
  echo "==> config"
  install -d -m 0750 -o root -g hakobu /etc/hakobu
  install -d -m 0700 -o hakobu -g hakobu "$DATA"
  OP_TOKEN="hk_$(openssl rand -hex 32)"
  ADMIN_TOKEN="hk_$(openssl rand -hex 32)"
  sha() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }
  {
    echo "name = \"$HAKOBU_NAME\""
    echo "listen = \"$API_IP:$API_PORT\""
    echo "data_dir = \"$DATA\""
    echo "owner_key = \"$HAKOBU_OWNER_KEY\""
    echo "operator_token_sha256 = \"$(sha "$OP_TOKEN")\""
    echo "admin_token_sha256 = \"$(sha "$ADMIN_TOKEN")\""
    # The extra's top-level keys (releases) must come before any table, so
    # it goes before [ingress]; its own tables end where [ingress] starts.
    [ -z "${HAKOBU_CONFIG_EXTRA:-}" ] || { echo; cat "$HAKOBU_CONFIG_EXTRA"; }
    case "${HAKOBU_INGRESS:-none}" in
      public) printf '\n[ingress]\nmode = "public"\nemail = "%s"\n' "${HAKOBU_ACME_EMAIL:-}" ;;
      tunnel) printf '\n[ingress]\nmode = "tunnel"\n' ;;
    esac
  } > "$CONFIG.new"
  chown root:hakobu "$CONFIG.new"
  chmod 0640 "$CONFIG.new"
  mv "$CONFIG.new" "$CONFIG"
fi
# Checks the config, and makes the server's key on the first run.
SERVER_KEY="$(runuser -u hakobu -- "$BIN" server init --config "$CONFIG" 2>/dev/null)" || {
  runuser -u hakobu -- "$BIN" server init --config "$CONFIG" || true
  die "$CONFIG isn't right (above); fix it and run this again"
}

if [ "${HAKOBU_INGRESS:-}" = tunnel ] && ! systemctl -q is-active cloudflared 2>/dev/null; then
  echo "==> cloudflared"
  install -d -m 0755 /usr/share/keyrings
  curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
  echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" > /etc/apt/sources.list.d/cloudflared.list
  apt-get -o DPkg::Lock::Timeout=600 update -qq && apt-get -o DPkg::Lock::Timeout=600 install -y -qq cloudflared >/dev/null
  cloudflared service install "$CLOUDFLARE_TUNNEL_TOKEN" >/dev/null
fi

echo "==> service"
cat > "$UNIT" <<EOF
[Unit]
Description=hakobu
Wants=network-online.target tailscaled.service
After=network-online.target tailscaled.service user@$DK_UID.service
Requires=user@$DK_UID.service

[Service]
User=hakobu
Group=hakobu
Environment=DOCKER_HOST=unix://$SOCK_DIR/docker.sock
Environment=HAKOBU_DIALER_SOCKET=$SOCK_DIR/dialer.sock
# Docker is the Docker user's service, starting alongside.
ExecStartPre=/bin/sh -c 'for i in \$\$(seq 1 120); do docker info >/dev/null 2>&1 && exit 0; sleep 1; done; exit 1'
ExecStart=$BIN serve --config $CONFIG
Restart=always
RestartSec=5
TimeoutStartSec=150
# Port 80 and 443, for a public server, without being root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

NoNewPrivileges=yes
RestrictNamespaces=yes
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=$DATA /home/hakobu/.docker
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
# hakobu can't write its binary: it asks for an update by creating
# update-request in its data, and root runs the update (hakobu server
# update), which installs only a newer signed release.
cat > /etc/systemd/system/hakobu-update.path <<EOF
[Unit]
Description=hakobu's requests to update it

[Path]
PathExists=$DATA/update-request

[Install]
WantedBy=multi-user.target
EOF
cat > /etc/systemd/system/hakobu-update.service <<EOF
[Unit]
Description=Update hakobu to a newer signed release, as it asked

[Service]
Type=oneshot
ExecStart=$BIN server update --requested --config $CONFIG
TimeoutStartSec=30min
EOF
systemctl daemon-reload
as_docker systemctl --user restart hakobu-dialer
systemctl enable hakobu >/dev/null 2>&1
systemctl enable --now hakobu-update.path >/dev/null 2>&1
systemctl restart hakobu

echo
echo "Done: $("$BIN" --version) runs on $API_IP:$API_PORT (journalctl -u hakobu)."
echo "The server's key: $SERVER_KEY"
echo "Back up $DATA/key.age somewhere off this server."
if [ -n "$OP_TOKEN" ]; then
  echo
  echo "These tokens show only now:"
  echo "  admin:    $ADMIN_TOKEN    (your password manager; never on a disk an agent reads)"
  echo "  operator: $OP_TOKEN"
  echo
  if [ "$API" = loopback ]; then
    echo "On your laptop, with a tunnel to the API (ssh -N -L $API_PORT:127.0.0.1:$API_PORT root@this-server):"
    echo "  hakobu server add $HAKOBU_NAME http://127.0.0.1:$API_PORT    (paste the operator token)"
  else
    echo "On your laptop, in the tailnet:"
    echo "  hakobu server add $HAKOBU_NAME http://$API_IP:$API_PORT    (paste the operator token)"
  fi
fi
