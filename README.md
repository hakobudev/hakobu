# hakobu

Deploy apps from GitHub to servers you own. Push to the default branch and hakobu builds the new version and swaps it in with no downtime, behind Cloudflare: nothing on your servers is open to the internet.

**Docs: [hakobu.dev/docs](https://hakobu.dev/docs/)** · Hosted panel: [dash.hakobu.dev](https://dash.hakobu.dev)

- **Apps** from GitHub repositories, built with their Dockerfile or [Railpack](https://railpack.com); blue/green deploys, rollback with the database included, workers, volumes, limits.
- **Data** per project: PostgreSQL databases, Valkey caches (Redis-compatible), R2 or S3 storages.
- **Cloudflare Tunnel** for domains and HTTPS: servers need no public IP and no open port.
- **Backups** of databases and volumes to R2 in your account, sealed with the server's own key and test-restored after every upload.
- **Errors, logs and traces** from any Sentry SDK, kept on the server the app runs on.
- **Claude** reads your apps' logs and errors and deploys for you, through hakobu's MCP server.
- **Several servers**, your clients' Cloudflare accounts, and a panel shared with others.
- **One Go binary** with SQLite; Docker runs rootless. Releases are signed.

## Install

You need a Linux server and a domain on Cloudflare (the free plan is enough).

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo bash
```

The installer asks for a Cloudflare API token (it shows a link that makes one) and the panel's subdomain, then prints a link to claim the panel with GitHub. See [Self-hosting](https://hakobu.dev/docs/install/) for the details, and [Hosted panel](https://hakobu.dev/docs/hosted/) to use ours instead.

## Development

```bash
go build -o hakobu . && just check
```

See [Development](https://hakobu.dev/docs/development/) for running it locally, tests, the panel's pages and releases. The docs are `docs/*.md`, built into `site/docs/` with `just docs`; the landing page is `site/`.
