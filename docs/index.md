# Introduction

hakobu deploys apps from GitHub to servers you own. Push to the default branch and it builds the new version and swaps it in with no downtime, behind Cloudflare, so nothing on your servers is open to the internet.

## What it does

- **Apps** come from GitHub repositories, built with their Dockerfile or detected by [Railpack](https://railpack.com). Every push to the default branch deploys; a deploy goes live once it answers its health check, and **Rollback** returns to the build before.
- **Projects** group apps with their [databases](databases.md) (PostgreSQL), [caches](caches.md) (Valkey, Redis-compatible), [storages](storages.md) (R2 or S3) and [volumes](volumes.md). Each project has its own Docker networks: an app reaches its project's apps and data, never another project's.
- **Domains and HTTPS** through a Cloudflare Tunnel: hakobu creates the DNS records, and the server needs no public IP and no open port.
- **Backups** of databases and volumes go to R2 in your Cloudflare account every day, sealed with the server's own key, and each is restored into a scratch database right after upload to prove it restores.
- **Errors, logs and traces**: any Sentry SDK works, and what your apps send stays on the server they run on.
- **Claude** can read your apps' logs and errors, deploy and roll back, through hakobu's [MCP server](claude.md).
- **One binary**: hakobu is a single Go program with SQLite. It needs Docker, which the installer sets up rootless.

## Two ways to run it

The same program runs both.

- **The hosted panel** at [dash.hakobu.dev](https://dash.hakobu.dev): sign in with GitHub and add your servers. The panel runs no apps of its own and can't open your backups. See [Hosted panel](hosted.md).
- **Self-hosted**: install the panel on your own server with one command. See [Self-hosting](install.md).

Either way your apps run on your servers, and their domains, files and backups are in your Cloudflare account.

## What you need

- A Linux server (amd64 or arm64). Any VPS works; it needs no public IP.
- A domain on Cloudflare. The free plan is enough.
- A GitHub account, and the repositories you want to deploy.
