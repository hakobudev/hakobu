#!/usr/bin/env bash
# One-command install for hako on a fresh Linux server.
# Usage (one line, nothing to download beforehand):
#   curl -fsSL https://raw.githubusercontent.com/x0ryz/hako/main/install.sh | sudo bash
#   curl -fsSL https://raw.githubusercontent.com/x0ryz/hako/main/install.sh | sudo HAKO_EDGE=tailscale bash
#   curl -fsSL https://raw.githubusercontent.com/x0ryz/hako/main/install.sh | sudo HAKO_EDGE=public HAKO_DOMAIN=panel.example.com bash
#
# Env (all optional):
#   HAKO_EDGE     tailscale|cloudflare|public|none  (skips the interactive menu; needed for curl|bash since stdin is the script itself)
#   HAKO_DOMAIN   required only when HAKO_EDGE=public — the domain to point at the panel
#                 (DNS must already resolve to this server; hako gets its own Let's Encrypt cert, no separate reverse proxy needed)
#   HAKO_VERSION  v1.0.0  (default: latest GitHub release; falls back to building from source)
#   HAKO_REPO     x0ryz/hako
#
# After install: open the printed URL -> /setup (no token needed on first run)
# -> set public host + GitHub App -> restart -> sign in with the API token.
set -euo pipefail

HAKO_REPO="${HAKO_REPO:-x0ryz/hako}"

EDGE="${HAKO_EDGE:-}"
DOMAIN="${HAKO_DOMAIN:-}"

if [ -z "$EDGE" ]; then
  echo "How should the panel be reachable?"
  echo "  1) tailscale   private, only your tailnet (recommended)"
  echo "  2) cloudflare  public quick-tunnel URL, no account needed"
  echo "  3) public      your own domain, hako gets HTTPS for it automatically"
  echo "  4) none        localhost only (reach it via ssh -L 9000:127.0.0.1:9000)"
  printf "Choice [1/2/3/4]: "
  read -r CHOICE
  case "$CHOICE" in
    1|"") EDGE="tailscale" ;;
    2) EDGE="cloudflare" ;;
    3) EDGE="public" ;;
    4) EDGE="none" ;;
    *) echo "unknown choice"; exit 1 ;;
  esac
fi

if [ "$EDGE" = "public" ] && [ -z "$DOMAIN" ]; then
  printf "Domain to point at the panel (DNS A record must already point here): "
  read -r DOMAIN
  [ -n "$DOMAIN" ] || { echo "domain required for public"; exit 1; }
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root (it installs docker/tailscale/cloudflared and a systemd unit)"
  exit 1
fi

# Releases ship linux/amd64 and linux/arm64 binaries (see .goreleaser.yml) —
# map uname -m to Go's arch names so both prebuilt-binary and build-from-source
# fallback paths below fetch the right thing on ARM servers too.
case "$(uname -m)" in
  x86_64|amd64) HAKO_ARCH="amd64" ;;
  aarch64|arm64) HAKO_ARCH="arm64" ;;
  *) HAKO_ARCH="" ;;
esac

echo "==> installing dependencies (docker, git, railpack, buildkit)..."
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi
if ! command -v git >/dev/null; then
  (apt-get update && apt-get install -y git curl) || (yum install -y git curl)
fi
# railpack builds project images (auto-install, no manual steps)
if ! command -v railpack >/dev/null; then
  echo "==> installing railpack..."
  curl -fsSL https://railpack.com/install.sh | bash -s -- --yes || {
    echo "WARNING: auto-install of railpack failed — Dockerfile builds still work."
    echo "  Retry manually: curl -fsSL https://railpack.com/install.sh | bash"
  }
fi
# buildkit daemon needed by railpack (runs as a privileged container)
if command -v docker >/dev/null; then
  if ! docker inspect buildkit >/dev/null 2>&1; then
    echo "==> starting buildkit container..."
    docker run --privileged -d --name buildkit --restart unless-stopped moby/buildkit || true
  fi
fi

echo "==> installing hako to /opt/hako..."
mkdir -p /opt/hako/data
if [ -f ./hako ]; then
  cp ./hako /opt/hako/hako
  chmod +x /opt/hako/hako
else
  # Remote mode (curl | bash): try a prebuilt release first, then build from source.
  HAKO_VER="${HAKO_VERSION:-}"
  if [ -z "$HAKO_VER" ]; then
    HAKO_VER="$(curl -fsSL "https://api.github.com/repos/${HAKO_REPO}/releases/latest" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4 || true)"
  fi
  GOT_BIN=0
  if [ -n "${HAKO_VER:-}" ] && [ -n "$HAKO_ARCH" ]; then
    echo "==> downloading hako $HAKO_VER ($HAKO_ARCH)..."
    if curl -fsSL "https://github.com/${HAKO_REPO}/releases/download/${HAKO_VER}/hako-linux-${HAKO_ARCH}" -o /opt/hako/hako; then
      chmod +x /opt/hako/hako
      GOT_BIN=1
    else
      echo "WARNING: no prebuilt binary for $HAKO_VER/$HAKO_ARCH, will build from source."
    fi
  elif [ -n "${HAKO_VER:-}" ]; then
    echo "WARNING: unrecognized CPU architecture $(uname -m), no prebuilt binary — will build from source."
  fi
  if [ "$GOT_BIN" != "1" ]; then
    echo "==> building hako from source..."
    # Need Go >= 1.27 (distro packages are often older, e.g. Fedora ships 1.26),
    # so install the official toolchain when missing or too old.
    GO_NEED="1.27.1"
    GO_OK=0
    if command -v go >/dev/null; then
      GO_VER="$(go version 2>/dev/null | grep -o 'go[0-9.]*' | head -1 | cut -c3-)"
      if [ "$(printf '%s\n%s' "$GO_NEED" "$GO_VER" | sort -V | head -1)" = "$GO_NEED" ]; then
        GO_OK=1
      else
        echo "    system go $GO_VER too old, installing official go $GO_NEED..."
      fi
    fi
    if [ "$GO_OK" != "1" ]; then
      GO_TGZ="go${GO_NEED}.linux-${HAKO_ARCH:-amd64}.tar.gz"
      curl -fsSL "https://go.dev/dl/${GO_TGZ}" -o /tmp/${GO_TGZ}
      rm -rf /usr/local/go
      tar -C /usr/local -xzf /tmp/${GO_TGZ}
      rm -f /tmp/${GO_TGZ}
      export PATH="/usr/local/go/bin:$PATH"
    fi
    export GOTOOLCHAIN=auto
    TMP_SRC="$(mktemp -d)"
    git clone --depth 1 "https://github.com/${HAKO_REPO}.git" "$TMP_SRC"
    (cd "$TMP_SRC" && go build -o /opt/hako/hako .)
    rm -rf "$TMP_SRC"
  fi
fi

echo "==> systemd unit..."
cat > /etc/systemd/system/hako.service <<'EOF'
[Unit]
Description=hako agent
After=docker.service network-online.target
Requires=docker.service

[Service]
WorkingDirectory=/opt/hako
ExecStart=/opt/hako/hako agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now hako

echo "==> edge: $EDGE"
case "$EDGE" in
  tailscale)
    if ! command -v tailscale >/dev/null; then
      curl -fsSL https://tailscale.com/install.sh | sh
    fi
    tailscale up || true
    tailscale serve --bg --https=443 http://127.0.0.1:9000 || true
    echo "Panel: https://$(tailscale whoami --json 2>/dev/null | grep -o '\"DNSName\":\"[^\"]*\"' | cut -d'\"' -f4 | sed 's/\.$//')/setup"
    ;;
  cloudflare)
    if ! command -v cloudflared >/dev/null; then
      curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared
      chmod +x /usr/local/bin/cloudflared
    fi
    cat > /etc/systemd/system/hako-panel-tunnel.service <<'EOF'
[Unit]
Description=hako panel quick tunnel
After=hako.service

[Service]
ExecStart=/usr/local/bin/cloudflared tunnel --url http://127.0.0.1:9000
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now hako-panel-tunnel
    echo "Panel URL: journalctl -u hako-panel-tunnel -f  (look for trycloudflare.com link), then open <url>/setup"
    ;;
  public)
    # No extra daemon needed: hako itself listens on :80/:443 and gets a
    # Let's Encrypt cert for any host it recognizes (see internal/edge).
    # It only recognizes the public host below, so nothing else changes.
    echo "$DOMAIN" > /opt/hako/data/public_host
    systemctl restart hako
    echo "Panel: https://$DOMAIN/setup  (make sure DNS for $DOMAIN points at this server's public IP, and ports 80+443 are open)"
    ;;
  none)
    echo "Panel: ssh -L 9000:127.0.0.1:9000 <server>  ->  http://localhost:9000/setup"
    ;;
esac

echo "API token: journalctl -u hako | grep 'API token'"
echo "Done."
