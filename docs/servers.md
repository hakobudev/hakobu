# Server config

A server is configured in `/etc/hakobu/server.toml`, readable by hakobu alone. The installer writes the first part; the rest you add (or pass to the installer as `HAKOBU_CONFIG_EXTRA`), then `sudo systemctl restart hakobu`. Like `hakobu.toml`, it's strict: an unknown key stops hakobu from starting, and `journalctl -u hakobu` says why.

```toml
name = "kasl-1"
listen = "100.101.102.103:7070"
owner_key = "age1…"
operator_token_sha256 = "…"
admin_token_sha256 = "…"

releases = ["ghcr.io/kasl/crm:main", "ghcr.io/kasl/site:main"]

[registries."ghcr.io"]
username = "kasl"
password = "github_pat_…"

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
| `registries."HOST"` | `username` and `password` to pull from a registry |
| `ingress` | `mode` (`public` or `tunnel`), `email` (Let's Encrypt's contact), `listen` (for `tunnel`, on loopback; default `127.0.0.1:8080`) |
| `backups` | `at` (UTC, default `03:00`), `keep_daily` (7), `keep_weekly` (4), `primary` and `copies`: see [Backups](backups.md) |
| `updates` | `auto`: the newer releases hakobu installs by itself, `patch` (the default: 1.2.x of the 1.2 running), `minor` (any 1.x) or `none`; `at` (UTC, default an hour after `backups.at`): see [Updates](updates.md) |
| `alerts` | `smtp`, `pulse_url`, `pulse_fail_url`, `allow_ports`: see [Alerts](alerts.md) |

A bucket has `endpoint`, `region`, `bucket`, `access_key`, `secret_key`, and `prefix` (default `hakobu/NAME/`); copies have a `name`.

## Reaching the API

By default the API listens on 127.0.0.1:7070 and nowhere else; your laptop reaches it through SSH (`ssh -fN -L 7070:127.0.0.1:7070 root@the-server`), and the audit expects SSH to listen.

With a Tailscale key at install, it listens on the server's tailnet address instead. In your tailnet's policy, let your own devices reach the server's tag on port 7070; nothing else needs to. The installer leaves Tailscale's SSH, routes and DNS off.

## From your laptop

```bash
hakobu server add kasl-1 http://127.0.0.1:7070   # through the tunnel, or its tailnet address; the operator token on stdin
hakobu server list
hakobu status              # every server
hakobu --server kasl-1 deploys
```
