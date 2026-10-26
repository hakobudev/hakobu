# Server config

A server's settings change through its API, from your terminal or your agent's: no one edits a file on the server.

```bash
ssh kasl-1 server get                          # what's set, never a password or key
ssh kasl-1 server set release ghcr.io/kasl/crm:main   # the init tool does this
ssh -t kasl-1 server set mail --host smtp.example.com --from hakobu@example.com --to you@example.com --username …
ssh kasl-1 server set pulse https://hc-ping.com/… https://hc-ping.com/…/fail
ssh kasl-1 server set updates --auto patch --at 04:00
ssh -t kasl-1 server set registry registry.example.com --username …   # ghcr.io needs none
```

The backups' buckets are root's, as a change of them could stop the backups, or send them elsewhere:

```bash
ssh -t root@kasl-1 hakobu server set backups --endpoint https://ACCOUNT.r2.cloudflarestorage.com --bucket kasl-backups --access-key …
ssh -t root@kasl-1 hakobu server set backups --copy hetzner --endpoint https://fsn1.your-objectstorage.com --region fsn1 --bucket kasl-copy --access-key …
```

Passwords and keys never go in an argument: each is typed, hidden (hence `-t`), or comes from its variable on a laptop (`HAKOBU_SMTP_PASSWORD`, `HAKOBU_S3_SECRET_KEY`, `HAKOBU_REGISTRY_PASSWORD`). A change is proved before it's kept: the mail server sends you a mail, a bucket takes an object and gives it back (a copy only takes one, as its key may only write). hakobu then restarts itself into it, as soon as no deploy or backup runs, and the command waits for that.

hakobu keeps them in `settings.toml` in its data directory.

## server.toml

The server's own file, `/etc/hakobu/server.toml`, readable by hakobu alone, is what the installer wrote: its name, the API's address, your key and the tokens' hashes, and how the web gets in. The settings above may be set there too (then `sudo systemctl restart hakobu`), and what it sets wins: the API refuses to change it. Like `hakobu.toml`, it's strict: an unknown key stops hakobu from starting, and `journalctl -u hakobu` says why.

```toml
name = "kasl-1"
listen = "100.101.102.103:7070"
owner_key = "age1…"
operator_token_sha256 = "…"
admin_token_sha256 = "…"

releases = ["ghcr.io/kasl/crm:main", "ghcr.io/kasl/site:main"]

[registries."registry.example.com"]   # ghcr.io needs none
username = "kasl"
password = "…"

[ingress]
mode = "public"
email = "you@example.com"

[backups]
at = "03:00"
[backups.primary]
endpoint = "https://ACCOUNT.r2.cloudflarestorage.com"
region = "auto"
bucket = "kasl-backups"
access_key = "…"
secret_key = "…"
[[backups.copies]]
name = "hetzner"
endpoint = "https://fsn1.your-objectstorage.com"
region = "fsn1"
bucket = "kasl-backups-copy"
access_key = "…"   # a key that can only write
secret_key = "…"

[alerts]
pulse_url = "https://hc-ping.com/…"
pulse_fail_url = "https://hc-ping.com/…/fail"
allow_ports = [22]
[alerts.smtp]
host = "smtp.example.com"
port = 465
username = "…"
password = "…"
from = "hakobu@example.com"
to = "you@example.com"
```

## Keys

| Key | |
|---|---|
| `name` | the server's name; a project's `server` must match it |
| `listen` | the API's address, on the tailnet or 127.0.0.1 (reached through SSH); any other address is refused |
| `socket` | the API's Unix socket, default `/run/hakobu-api/api.sock`: root may do anything there, the SSH user what an operator may |
| `data_dir` | default `/var/lib/hakobu`: the key, state, deploy history, dumps |
| `owner_key` | your age public key |
| `operator_token_sha256`, `admin_token_sha256` | the tokens' SHA-256 (`hakobu server token` makes a new pair) |
| `releases` | the release images to deploy, one a project, by tag |
| `poll_every` | how often to check them; default `30s` |
| `registries."HOST"` | `username` and `password` to pull from a registry; for ghcr.io, the token CI seals in each release does without (see [Deploys](deploys.md#the-release-image)) |
| `ingress` | `mode` (`public` or `tunnel`), `email` (Let's Encrypt's contact), `listen` (for `tunnel`, on loopback; default `127.0.0.1:8080`) |
| `backups` | `at` (UTC, default `03:00`), `keep_daily` (7), `keep_weekly` (4), `primary` and `copies`: see [Backups](backups.md) |
| `updates` | `auto`: the newer releases hakobu installs by itself, `patch` (the default: 1.2.x of the 1.2 running), `minor` (any 1.x) or `none`; `at` (UTC, default an hour after `backups.at`): see [Updates](updates.md) |
| `alerts` | `smtp`, `pulse_url`, `pulse_fail_url`, `allow_ports`: see [Alerts](alerts.md) |

A bucket has `endpoint`, `region`, `bucket`, `access_key`, `secret_key`, and `prefix` (default `hakobu/NAME/`); copies have a `name`.

## Reaching the API

The API listens on 127.0.0.1:7070, and on a Unix socket, `/run/hakobu-api/api.sock`, for root and the SSH user's group: `ssh kasl-1 …` and `ssh kasl-1 mcp` go through the socket (see [A server](install.md#the-ssh-user)). The audit expects SSH to listen.

With a Tailscale key at install, the API listens on the server's tailnet address instead of 127.0.0.1, for hakobu on a laptop. In your tailnet's policy, let your own devices reach the server's tag on port 7070; nothing else needs to. The installer leaves Tailscale's SSH, routes and DNS off.

## From a laptop

```bash
hakobu server create kasl-1 --ssh root@203.0.113.7   # once: installs, and remembers it
hakobu server add kasl-2 http://100.101.102.103:7070  # one installed otherwise; the operator token on stdin
hakobu server list
hakobu status              # every server
hakobu --server kasl-1 deploys
hakobu server set mail kasl-1 --host …               # the commands above, with the server's name
```

The backups' buckets need the admin token there (`HAKOBU_ADMIN_TOKEN`, or typed).
