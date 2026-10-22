# A server

One command sets a server up, and asks nothing: everything comes from the environment, so cloud-init or an agent can run it.

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo \
  HAKOBU_NAME=kasl-1 \
  HAKOBU_OWNER_KEY=age1… \
  TS_AUTHKEY=tskey-auth-… TS_TAGS=tag:hakobu \
  HAKOBU_INGRESS=public HAKOBU_ACME_EMAIL=you@example.com \
  bash
```

It installs rootless Docker (as the user `hakobu-docker`), Tailscale, unattended upgrades, hakobu (its signature checked) as a sandboxed systemd service running as `hakobu`, writes `/etc/hakobu/server.toml` and makes the server's key. Run it again to update: the config, the key and the data stay.

## Before you start

- **Your key.** `age-keygen` makes one; keep the secret half in your password manager and give the public half (`age1…`) as `HAKOBU_OWNER_KEY`. Every secret and backup is sealed to it as well as to the server, so you can open them if the server is gone.
- **A Tailscale auth key** ([admin console](https://login.tailscale.com/admin/settings/keys)), with the tag you'll give the server. The API listens only on the server's tailnet address. Your tailnet's policy should let your devices reach the tag on port 7070, and nothing else need reach it.

## Settings

| Variable | |
|---|---|
| `HAKOBU_NAME` | the server's name: lowercase letters, digits and `-` |
| `HAKOBU_OWNER_KEY` | your age public key |
| `TS_AUTHKEY` | a Tailscale auth key, unless the server is in the tailnet already |
| `TS_TAGS` | the server's Tailscale tags, `tag:hakobu` say |
| `HAKOBU_INGRESS` | `public`, `tunnel` or `none` (the default): see below |
| `HAKOBU_ACME_EMAIL` | Let's Encrypt's contact, for `public` |
| `CLOUDFLARE_TUNNEL_TOKEN` | the tunnel's token, for `tunnel` |
| `HAKOBU_CONFIG_EXTRA` | a file whose TOML goes at the end of the config: releases, registries, backups, alerts (see [Server config](servers.md)) |
| `HAKOBU_VERSION` | a release; default the newest |

## How the web gets in

- **`public`**: hakobu answers ports 80 and 443 itself, with a Let's Encrypt certificate for each app's domain (and only those). Point each domain's DNS at the server.
- **`tunnel`**: cloudflared runs on the server with your tunnel's token and sends requests to hakobu on loopback; the server needs no open port. In Cloudflare, route each domain of the tunnel to `http://localhost:8080`.
- **`none`**: apps aren't served to the web; for a server of workers only, or while you set it up.

## The tokens

On a first install it prints two tokens, once:

- the **operator** token, for your laptop and your agent: status, logs, deploy, rollback, backup;
- the **admin** token, for your password manager only: it alone can restore and delete data.

On your laptop, with the hakobu binary from the [releases](https://github.com/hakobudev/hakobu/releases):

```bash
hakobu server add kasl-1 http://100.x.y.z:7070   # then paste the operator token
hakobu status
```

The installer's output names the API's address. Servers are remembered in `~/.config/hakobu/servers.toml`.

## Back up the server's key

`/var/lib/hakobu/key.age` opens the server's secrets and backups. Your own key does too, so losing the server's isn't fatal, but a copy off the server spares you re-sealing secrets on a new one.
