# A server

From your laptop, with the hakobu CLI from the [releases](https://github.com/hakobudev/hakobu/releases), and SSH to a new Debian or Ubuntu server as root (or a user with sudo):

```bash
hakobu server create kasl-1 --ssh root@203.0.113.7 --ingress public --email you@example.com
hakobu status
```

That's the one time anything runs on the server by hand. It installs rootless Docker (as the user `hakobu-docker`), unattended upgrades, and hakobu (its signature checked) as a sandboxed systemd service running as `hakobu`; writes `/etc/hakobu/server.toml` and makes the server's key; and remembers the server on your laptop. After that hakobu updates itself (see [Updates](updates.md)), and you, your agent and the CLI reach it only through its API.

## Your key

Every secret and backup is sealed to your age key as well as to the server's, so you can open them if the server is gone. `age-keygen` makes one: keep the secret half in your password manager. `server create` asks for the public half (`age1…`) the first time, and keeps it in `~/.config/hakobu/owner.pub`; `--owner-key` gives another.

## Reaching the API

hakobu's API listens on the server's 127.0.0.1 and nowhere else. `server create` makes an SSH key for it on your laptop (`~/.config/hakobu/keys/NAME`) and puts its public half on the server for a user, `hakobu-tunnel`, that can't log in: the key may reach the API's port and nothing else, no shell, no other port. The CLI and `hakobu mcp` go through it by themselves (`ssh -W`, with your `ssh` and its config). The audit expects SSH to listen.

With `--tailscale`, the API listens on the server's tailnet address instead: give a Tailscale [auth key](https://login.tailscale.com/admin/settings/keys) as `TS_AUTHKEY` (or type it when asked) and the server's tags with `--tags tag:hakobu`; your tailnet's policy should let your devices reach the tag on port 7070.

## Options

| | |
|---|---|
| `--ssh` | how to reach the server: `user@host`, or a host of `~/.ssh/config` |
| `--ingress` | `public`, `tunnel` or `none` (the default): see below |
| `--email` | Let's Encrypt's contact, for `public` |
| `--tailscale`, `--tags` | the API on the tailnet |
| `--owner-key` | your age public key, if not the one kept |
| `--version` | a release; default the CLI's own |

Secrets never go in an argument: a tunnel's token comes from `CLOUDFLARE_TUNNEL_TOKEN` and a Tailscale key from `TS_AUTHKEY`, or each is asked for, hidden. `HAKOBU_SSH` names another ssh to run, as `GIT_SSH` does for git.

Run it again for the same server to put this CLI's release on it as an installer would: the tokens and the data stay.

## Without the laptop

For cloud-init, or a server you set up some other way, the same installer runs on the server, and asks nothing: everything comes from the environment.

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo \
  HAKOBU_NAME=kasl-1 HAKOBU_OWNER_KEY=age1… \
  HAKOBU_INGRESS=public HAKOBU_ACME_EMAIL=you@example.com \
  bash
```

It prints the tokens; on your laptop, `hakobu server add kasl-1 URL` with the operator one, through your own tunnel (`ssh -fN -L 7070:127.0.0.1:7070 root@the-server`, then `http://127.0.0.1:7070`) or the tailnet address.

| Variable | |
|---|---|
| `HAKOBU_NAME` | the server's name: lowercase letters, digits and `-` |
| `HAKOBU_OWNER_KEY` | your age public key |
| `TS_AUTHKEY` | a Tailscale auth key, for the API on the tailnet; without one (and a server not in a tailnet already), it's on 127.0.0.1, reached through SSH |
| `HAKOBU_API` | `loopback` or `tailnet`, to say which outright |
| `TS_TAGS` | the server's Tailscale tags, `tag:hakobu` say |
| `HAKOBU_INGRESS` | `public`, `tunnel` or `none` (the default): see below |
| `HAKOBU_ACME_EMAIL` | Let's Encrypt's contact, for `public` |
| `CLOUDFLARE_TUNNEL_TOKEN` | the tunnel's token, for `tunnel` |
| `HAKOBU_CONFIG_EXTRA` | a file whose TOML goes at the end of the config: releases, registries, backups, alerts (see [Server config](servers.md)) |
| `HAKOBU_VERSION` | a release; default the newest |
| `HAKOBU_TUNNEL_KEY` | an SSH public key (`ssh-ed25519 …`) that may reach the API and nothing else |

## How the web gets in

- **`public`**: hakobu answers ports 80 and 443 itself, with a Let's Encrypt certificate for each app's domain (and only those). Point each domain's DNS at the server.
- **`tunnel`**: cloudflared runs on the server with your tunnel's token and sends requests to hakobu on loopback; the server needs no open port. In Cloudflare, route each domain of the tunnel to `http://localhost:8080`.
- **`none`**: apps aren't served to the web; for a server of workers only, or while you set it up.

## The tokens

A first install makes two tokens:

- the **operator** token, for your laptop and your agent: status, logs, deploy, rollback, backup, updates. `server create` keeps it in `~/.config/hakobu/servers.toml`;
- the **admin** token, for your password manager only: it alone can restore and delete data. It's shown once, and kept nowhere.

## Back up the server's key

`/var/lib/hakobu/key.age` opens the server's secrets and backups. Your own key does too, so losing the server's isn't fatal, but a copy off the server spares you re-sealing secrets on a new one.
