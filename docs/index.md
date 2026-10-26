# Introduction

hakobu runs your projects on servers you own. A project says what it needs in a file in its repository, `hakobu.toml`; CI builds each commit into images; the server pulls the new release and swaps it in with no downtime, then tells the commit how it went. There's no panel and no database: the file is the state, and the code that keeps your data safe (deploys, backups, secrets, alerts) is written and tested once, in hakobu.

## What it does

- **Deploys by pull.** CI pushes a release image; the server checks its tag every 30 seconds and deploys a new one. Nothing connects to the server from outside, and any CI that can push an image works. The commit gets a `hakobu/deploy` status, and the CI job waits for it, so a failed deploy is a red commit with the reason.
- **Safe deploys.** Before a deploy with migrations, hakobu dumps the databases; migrations run in the new image, and if they fail, the running version stays. The new version takes traffic only once it answers its health check. A deploy never deletes data: what the file no longer has is kept until a person deletes it.
- **Databases and caches.** PostgreSQL databases with their own users, and Valkey (Redis-compatible) caches, with passwords made on the server and handed to the apps as variables.
- **Secrets in the repository**, each sealed with [age](https://age-encryption.org) to the server and to you: anyone, an agent too, can add one; only the server and you can read them.
- **Backups** of every database and volume each day, sealed, to any S3 bucket and a copy elsewhere, each restored into scratch right after upload to prove it restores.
- **Alerts** by email when an app is down, a deploy or backup fails, the disk fills, or the audit finds a port open to the internet or a container hakobu didn't start; a pulse to a dead man's switch says hakobu itself is alive.
- **Agents welcome, nothing on your laptop.** Each server is its own MCP server, over SSH: your agent sees status and logs, deploys, rolls back and sets a repository up, with a skill for Claude Code that says how to change the file and migrations safely. You run the same commands, `ssh kasl-1 status`. Deleting or restoring data is root's, and the agent can't.

## What it needs

- A Linux server (Debian or Ubuntu, amd64 or arm64) with systemd, and SSH to it as root, once, to install. The installer sets up rootless Docker, and hakobu then updates itself.
- `ssh` on your laptop, which it has: you and your agent reach hakobu through it, with keys that run hakobu's commands and nothing else. hakobu's API listens on the server's loopback and nowhere else.
- A registry for the images (GitHub's ghcr.io works with no setup) and a CI to build them (the workflow hakobu provides is for GitHub Actions).
- For backups, an S3 bucket (Cloudflare R2, Hetzner Object Storage, AWS, any other); for alerts, an SMTP account.
- For the web, either ports 80 and 443 (hakobu gets Let's Encrypt certificates) or a Cloudflare Tunnel.

hakobu calls no provider's API: no DNS, no cloud, no Git host beyond the commit status. What's different in each project (its file, Dockerfile, migrations, DNS records) is for you or your agent to write.

Start with [a server](install.md), then [a project](project.md).
