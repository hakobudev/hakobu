# Server config

A server's settings change from your laptop, through its API: no one edits a file on the server.

```bash
hakobu server get kasl-1                      # what's set, never a password or key
hakobu server set release kasl-1 ghcr.io/kasl/crm:main   # hakobu init does this
hakobu server set mail kasl-1 --host smtp.example.com --from hakobu@example.com --to you@example.com --username …
hakobu server set backups kasl-1 --endpoint https://ACCOUNT.r2.cloudflarestorage.com --bucket kasl-backups --access-key …
hakobu server set backups kasl-1 --copy hetzner --endpoint https://fsn1.your-objectstorage.com --region fsn1 --bucket kasl-copy --access-key …
hakobu server set pulse kasl-1 https://hc-ping.com/… https://hc-ping.com/…/fail
hakobu server set updates kasl-1 --auto patch --at 04:00
hakobu server set registry kasl-1 registry.example.com --username …   # ghcr.io needs none
```

Passwords and keys never go in an argument: each comes from its variable (`HAKOBU_SMTP_PASSWORD`, `HAKOBU_S3_SECRET_KEY`, `HAKOBU_REGISTRY_PASSWORD`) or is typed, hidden. A change is proved before it's kept: the mail server sends you a mail, a bucket takes an object and gives it back (a copy only takes one, as its key may only write). hakobu then restarts itself into it, as soon as no deploy or backup runs, and the command waits for that. The backups' buckets need the admin token (`HAKOBU_ADMIN_TOKEN`, or typed), as a change of them could stop the backups, or send them elsewhere; the rest, the operator's. `hakobu server create` asks for a bucket and a mail server at its end.

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
| `listen` | the API's address, on the tailnet or 127.0.0.1 (reached through SSH); an address on every interface is refused |
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

By default the API listens on 127.0.0.1:7070 and nowhere else; your laptop reaches it through SSH, with the key `hakobu server create` made, which may reach that port and nothing else (see [A server](install.md#reaching-the-api)). The audit expects SSH to listen.

With a Tailscale key at install, it listens on the server's tailnet address instead. In your tailnet's policy, let your own devices reach the server's tag on port 7070; nothing else needs to. The installer leaves Tailscale's SSH, routes and DNS off.

## From your laptop

```bash
hakobu server create kasl-1 --ssh root@203.0.113.7   # once: installs, and remembers it
hakobu server add kasl-2 http://100.101.102.103:7070  # one installed otherwise; the operator token on stdin
hakobu server list
hakobu status              # every server
hakobu --server kasl-1 deploys
```
