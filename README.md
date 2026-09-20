# hako — self-hosted deployment tool

Deploy apps from GitHub repos to your own server, without paying for a PaaS.
Push to `main`, hako builds and rolls out the new container with zero
downtime.

- **Auto-detects the stack** — Railpack or Dockerfile, no config needed for most repos.
- **Zero-downtime deploys** — blue/green containers behind an internal proxy that swaps traffic atomically.
- **Postgres + backups** built in — one-click database, scheduled dumps to S3/R2.
- **No reverse-proxy setup** — give a project a domain and hako gets its own Let's Encrypt cert and routes it automatically.
- **Bring your own edge if you'd rather** — Tailscale or a Cloudflare quick tunnel work too, for the panel and for individual projects.
- **Built-in error/log ingestion** — a Sentry-compatible endpoint, so deployed apps just need a `SENTRY_DSN`, no extra service to run.
- Single Go binary + SQLite. No external dependencies to operate beyond Docker.

## Install on a fresh server

One line, nothing to download beforehand — installs Docker/git/Railpack/buildkit
if missing, builds or downloads the `hako` binary, and sets up a systemd
service:

```bash
curl -fsSL https://raw.githubusercontent.com/x0ryz/hako/main/install.sh | sudo bash
```

That drops you into an interactive menu asking how the panel should be
reachable (Tailscale / Cloudflare quick tunnel / your own domain / localhost
only). For scripted installs (stdin isn't available for prompts when piping
into `sudo bash`), skip the menu with an env var instead:

```bash
curl -fsSL https://raw.githubusercontent.com/x0ryz/hako/main/install.sh | sudo HAKO_EDGE=tailscale bash
curl -fsSL https://raw.githubusercontent.com/x0ryz/hako/main/install.sh | sudo HAKO_EDGE=public HAKO_DOMAIN=panel.example.com bash
```

Requirements: a Linux server (root access), and that's it — `install.sh`
handles the rest.

After install, open the printed URL → `/setup` → set public host + connect a
GitHub App → restart → sign in with the API token
(`journalctl -u hako | grep 'API token'`).

## Local dev

```bash
go build -o hako . && ./hako agent   # panel on http://127.0.0.1:9000
```

Or, with Go installed: `go install github.com/x0ryz/hako@latest`.

Requires: Go 1.27+, Docker. Railpack + buildkit are optional, only needed for
non-Dockerfile projects:

```bash
curl -fsSL https://railpack.com/install.sh | bash -s -- --yes
docker run --privileged -d --name buildkit --restart unless-stopped moby/buildkit
```

## Layout

- `cmd/` — CLI (`agent`, web panel, API)
- `internal/build/` — builders (railpack, dockerfile)
- `internal/detect/` — repo preset scanner
- `internal/deploy/`, `internal/ops/` — containers, domains, databases
- `internal/edge/` — optional public TLS entrypoint (auto Let's Encrypt for the panel and per-project domains)
- `internal/backup/`, `internal/store/` — backups (S3/R2), sqlite state
- `install.sh` — one-command server install

## Notes

- Runtime state lives in `data/` (sqlite, logs, work clones) — git-ignored, never commit it.
- Old projects stored with `build_strategy='nixpacks'` still build: it's kept as an alias for `railpack`.

## License

[MIT](LICENSE)
