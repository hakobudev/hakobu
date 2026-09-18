# hako — self-hosted deployment tool

Deploy apps from GitHub repos to your own server. Auto-detects the stack
(**Railpack** or **Dockerfile**), builds the image, runs the container behind
Caddy with automatic HTTPS, wires Postgres / env / backups.

## Quick start (fresh server)

```bash
# 1. build the binary (or download it from Releases)
go build -o hako .

# 2. copy hako + install.sh to the server, then on the server as root:
sudo bash install.sh --edge tailscale   # or: cloudflare | none
```

`install.sh` installs everything missing automatically:
**docker, git, railpack, buildkit** (+ tailscale / cloudflared depending on `--edge`).

After install open the printed URL → `/setup` → set public host + GitHub App →
restart → sign in with the API token (`journalctl -u hako | grep 'API token'`).

## Local dev

```bash
go build -o hako . && ./hako agent   # panel on http://127.0.0.1:9000
```

Requires: Go 1.27+, docker. Railpack + buildkit are optional for Dockerfile-only projects:

```bash
curl -fsSL https://railpack.com/install.sh | bash -s -- --yes
docker run --privileged -d --name buildkit --restart unless-stopped moby/buildkit
```

## Layout

- `cmd/` — CLI (`agent`, web panel, API)
- `internal/build/` — builders (railpack, dockerfile)
- `internal/detect/` — repo preset scanner
- `internal/deploy/`, `internal/ops/` — containers, domains, databases
- `internal/backup/`, `internal/store/` — backups (S3/R2), sqlite state
- `install.sh` — one-command server install

## Notes

- Runtime state lives in `data/` (sqlite, logs, work clones) — git-ignored, never commit it.
- Old projects stored with `build_strategy='nixpacks'` still build: it's kept as an alias for `railpack`.
