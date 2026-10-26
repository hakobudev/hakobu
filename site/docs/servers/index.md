# Server config

A server is configured in `/etc/hakobu/server.toml`, readable by hakobu alone. The installer writes the first part; the rest you add (or pass to the installer as `HAKOBU_CONFIG_EXTRA`), then `sudo systemctl restart hakobu`. Like `hakobu.toml`, it's strict: an unknown key stops hakobu from starting, and `journalctl -u hakobu` says why.

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
| `registries."HOST"` | `username` and `password` to pull from a registry; for ghcr.io, the token CI seals in each release does without (see [Deploys](https://hakobu.dev/docs/deploys/index.md)) |
| `ingress` | `mode` (`public` or `tunnel`), `email` (Let's Encrypt's contact), `listen` (for `tunnel`, on loopback; default `127.0.0.1:8080`) |
| `backups` | `at` (UTC, default `03:00`), `keep_daily` (7), `keep_weekly` (4), `primary` and `copies`: see [Backups](https://hakobu.dev/docs/backups/index.md) |
| `updates` | `auto`: the newer releases hakobu installs by itself, `patch` (the default: 1.2.x of the 1.2 running), `minor` (any 1.x) or `none`; `at` (UTC, default an hour after `backups.at`): see [Updates](https://hakobu.dev/docs/updates/index.md) |
| `alerts` | `smtp`, `pulse_url`, `pulse_fail_url`, `allow_ports`: see [Alerts](https://hakobu.dev/docs/alerts/index.md) |

A bucket has `endpoint`, `region`, `bucket`, `access_key`, `secret_key`, and `prefix` (default `hakobu/NAME/`); copies have a `name`.

## Reaching the API

By default the API listens on 127.0.0.1:7070 and nowhere else; your laptop reaches it through SSH, with the key `hakobu server create` made, which may reach that port and nothing else (see [A server](https://hakobu.dev/docs/install/index.md)). The audit expects SSH to listen.

With a Tailscale key at install, it listens on the server's tailnet address instead. In your tailnet's policy, let your own devices reach the server's tag on port 7070; nothing else needs to. The installer leaves Tailscale's SSH, routes and DNS off.

## From your laptop

```bash
hakobu server create kasl-1 --ssh root@203.0.113.7   # once: installs, and remembers it
hakobu server add kasl-2 http://100.101.102.103:7070  # one installed otherwise; the operator token on stdin
hakobu server list
hakobu status              # every server
hakobu --server kasl-1 deploys
```
