# hakobu

Run your projects on servers you own. A project says what it needs in a file in its repository, `hakobu.toml`; CI builds each commit into images; the server pulls the new release, swaps it in with no downtime and sets the commit's status. No panel and no database: the file is the state, and the parts that keep your data safe (deploys, backups, secrets, alerts) are written and tested once, here.

**Docs: [hakobu.dev/docs](https://hakobu.dev/docs/)**

- **Pull deploys**: the server polls a release image and deploys each new one; CI needs no access to it, and the commit goes red with the reason if the deploy fails.
- **Safe deploys**: migrations run in the new image first (a failure leaves the running version alone), the new version takes traffic only once it's healthy, and nothing a deploy does deletes data.
- **PostgreSQL databases and Valkey caches**, with passwords made on the server.
- **Secrets in the repository**, each sealed with [age](https://age-encryption.org) to the server and to you: anyone can add one, only the two of you can read it.
- **Backups** of databases and volumes to any S3 bucket and a copy elsewhere, sealed, and restored into scratch after every upload to prove they restore.
- **Alerts** by email when an app is down, a deploy or backup fails, the disk fills, or the audit finds a port or container that shouldn't be there; a pulse to a dead man's switch.
- **Agents welcome, nothing on your laptop**: each server is its own MCP server, over SSH (`ssh deploy@SERVER mcp`), so an agent sees status and logs, deploys, rolls back and sets a repository up, with a skill for Claude Code; you run the same, `ssh kasl-1 status`. Deleting or restoring data is root's.
- **One Go binary** on the server, rootless Docker, SSH keys that run hakobu's commands alone, signed releases that install themselves. No provider's API is called.

## Start

On a new Debian or Ubuntu server, once (you or your agent):

```bash
ssh root@203.0.113.7 'curl -fsSL https://hakobu.dev/install.sh | HAKOBU_NAME=web-1 HAKOBU_INGRESS=public HAKOBU_ACME_EMAIL=you@example.com bash'
```

It ends with the line that gives your agent the server's MCP (`claude mcp add … -- ssh -T deploy@203.0.113.7 mcp`); then ask the agent to set a repository up. See [A server](https://hakobu.dev/docs/install/) and [A project](https://hakobu.dev/docs/project/).

## Development

```bash
go build -o hakobu .
just check
```

See [Development](https://hakobu.dev/docs/development/). MIT license.
