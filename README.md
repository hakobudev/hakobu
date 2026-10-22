# hakobu

Run your projects on servers you own. A project says what it needs in a file in its repository, `hakobu.toml`; CI builds each commit into images; the server pulls the new release, swaps it in with no downtime and sets the commit's status. No panel and no database: the file is the state, and the parts that keep your data safe (deploys, backups, secrets, alerts) are written and tested once, here.

**Docs: [hakobu.dev/docs](https://hakobu.dev/docs/)**

- **Pull deploys**: the server polls a release image and deploys each new one; CI needs no access to it, and the commit goes red with the reason if the deploy fails.
- **Safe deploys**: migrations run in the new image first (a failure leaves the running version alone), the new version takes traffic only once it's healthy, and nothing a deploy does deletes data.
- **PostgreSQL databases and Valkey caches**, with passwords made on the server.
- **Secrets in the repository**, each sealed with [age](https://age-encryption.org) to the server and to you: anyone can add one, only the two of you can read it.
- **Backups** of databases and volumes to any S3 bucket and a copy elsewhere, sealed, and restored into scratch after every upload to prove they restore.
- **Alerts** by email when an app is down, a deploy or backup fails, the disk fills, or the audit finds a port or container that shouldn't be there; a pulse to a dead man's switch.
- **Agents welcome**: `hakobu init` gives a repository a skill for Claude Code, and `hakobu mcp` lets an agent see status and logs, deploy and roll back. Deleting or restoring data is a person's to do.
- **One Go binary**, rootless Docker, the API on your tailnet only, signed releases. No provider's API is called.

## Start

A server (Debian or Ubuntu, with a Tailscale auth key and your age public key):

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo \
  HAKOBU_NAME=web-1 HAKOBU_OWNER_KEY=age1… TS_AUTHKEY=tskey-auth-… HAKOBU_INGRESS=public bash
```

A project, in its repository, with the CLI from the [releases](https://github.com/hakobudev/hakobu/releases):

```bash
hakobu server add web-1 http://100.x.y.z:7070   # the operator token on stdin
hakobu init --server web-1
```

Then see [A project](https://hakobu.dev/docs/project/).

## Development

```bash
go build -o hakobu .
just check
```

See [Development](https://hakobu.dev/docs/development/). MIT license.
