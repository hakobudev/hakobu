# A server

You need a new Debian or Ubuntu server you can SSH to as root (or a user with sudo), and nothing on your laptop but `ssh`. You, or your agent, run one line:

```bash
ssh root@203.0.113.7 'curl -fsSL https://hakobu.dev/install.sh | HAKOBU_NAME=kasl-1 HAKOBU_INGRESS=public HAKOBU_ACME_EMAIL=you@example.com bash'
```

That's the one time anything runs on the server by hand. It installs rootless Docker (as the user `hakobu-docker`), unattended upgrades, and hakobu (its signature checked) as a sandboxed systemd service running as `hakobu`; writes `/etc/hakobu/server.toml` and makes the server's key; and makes an SSH user, `deploy`, whose keys run hakobu's commands and nothing else. At its end it prints:

```
For your agent, once (Claude Code):
  claude mcp add --scope user kasl-1 -- ssh -T deploy@203.0.113.7 mcp
For you, in ~/.ssh/config:
  Host kasl-1
    HostName 203.0.113.7
    User deploy
then ssh kasl-1 status
```

The first line gives your agent the server's own MCP server, over SSH; the block lets you write `ssh kasl-1 status`, as these docs do. After that hakobu updates itself (see [Updates](https://hakobu.dev/docs/updates/index.md)), and you and your agent reach it only through those commands. Then take [your owner's key](#your-owners-key), and set up [a project](https://hakobu.dev/docs/project/index.md).

## The SSH user

`deploy` logs in with the keys root has (or `sudo`'s user), copied at the first install without their options; `HAKOBU_SSH_KEYS` gives others, one a line. Each key is `restrict`ed to one forced command, hakobu's: no shell, no files, no port forwarding, whatever the client asks for. Its home and keys are root's, so nothing running as it could add a key. `HAKOBU_SSH_USER` names it otherwise (the installer won't take over a `deploy` that's there already).

| | |
|---|---|
| `ssh kasl-1 status` | what runs, the last deploy, alerts, backups |
| `ssh kasl-1 deploys` | the last deploys, and why one failed |
| `ssh kasl-1 logs PROJECT/APP [--worker]` | an app's output |
| `ssh kasl-1 deploy PROJECT [--force]` | check for its release now; again with `--force` |
| `ssh kasl-1 rollback PROJECT` | back one release |
| `ssh kasl-1 backup`, `backups PROJECT` | back up now, list them |
| `ssh -t kasl-1 secret set PROJECT APP NAME` | seal a secret, typed unseen: see [Secrets](https://hakobu.dev/docs/secrets/index.md) |
| `ssh kasl-1 server get`, `server set …` | the server's settings: see [Server config](https://hakobu.dev/docs/servers/index.md) |
| `ssh kasl-1 server update [--to v1.5.0]` | update hakobu |
| `ssh kasl-1 mcp` | the MCP server, for an agent |

They ask hakobu's API on its Unix socket, `/run/hakobu-api/api.sock`, which only root and `deploy`'s group may open; the kernel tells hakobu who's on the other end, so no token is kept anywhere: root may do anything, `deploy` what an operator may. Restore, destroy and the backups' buckets are root's: `ssh root@kasl-1 hakobu restore …`.

To let another person or agent in, add their key as root to `/home/deploy/.ssh/authorized_keys`, with the same options as the line there.

## Your owner's key

Every secret and backup is sealed to your owner's key as well as to the server's, so you can open them if the server is gone. Without `HAKOBU_OWNER_KEY`, the installer makes one, and its secret half waits on the server, root's alone, for you to take it into your password manager, in your own terminal, not an agent's:

```bash
ssh -t root@kasl-1 hakobu server owner-key
```

It shows the key, and once you type `saved`, takes it off the server. Until then, the [audit](https://hakobu.dev/docs/alerts/index.md) reminds you. With [age](https://age-encryption.org) of your own, give the public half instead: `HAKOBU_OWNER_KEY=age1…` (`age-keygen` makes a pair).

## SSH itself

SSH is safe with keys alone. Many images take passwords too (Hetzner's Ubuntu does), and the installer warns if the server does; turn them off, as your key logs you in already:

```bash
ssh root@kasl-1 'echo "PasswordAuthentication no" > /etc/ssh/sshd_config.d/00-keys-only.conf && systemctl reload ssh'
```

The audit expects SSH to listen.

## Options

The installer asks nothing: everything comes from the environment.

| Variable | |
|---|---|
| `HAKOBU_NAME` | the server's name: lowercase letters, digits and `-` |
| `HAKOBU_OWNER_KEY` | your age public key; without it, the server makes one (above) |
| `HAKOBU_SSH_USER` | the SSH user for hakobu's commands; default `deploy` |
| `HAKOBU_SSH_KEYS` | its keys, one a line; default root's, the first time |
| `HAKOBU_INGRESS` | `public`, `tunnel` or `none` (the default): see below |
| `HAKOBU_ACME_EMAIL` | Let's Encrypt's contact, for `public` |
| `CLOUDFLARE_TUNNEL_TOKEN` | the tunnel's token, for `tunnel` |
| `TS_AUTHKEY`, `TS_TAGS` | a Tailscale auth key and tags, for the API to listen on the tailnet as well (for hakobu on a laptop, below) |
| `HAKOBU_CONFIG_EXTRA` | a file whose TOML goes at the end of the config: releases, registries, backups, alerts (see [Server config](https://hakobu.dev/docs/servers/index.md)) |
| `HAKOBU_VERSION` | a release; default the newest |

Run it again to update by hand: the config, the keys and the data stay.

## How the web gets in

- **`public`**: hakobu answers ports 80 and 443 itself, with a Let's Encrypt certificate for each app's domain (and only those). Point each domain's DNS at the server.
- **`tunnel`**: cloudflared runs on the server with your tunnel's token and sends requests to hakobu on loopback; the server needs no open port. In Cloudflare, route each domain of the tunnel to `http://localhost:8080`.
- **`none`**: apps aren't served to the web; for a server of workers only, or while you set it up.

## Back up the server's key

`/var/lib/hakobu/key.age` opens the server's secrets and backups. Your own key does too, so losing the server's isn't fatal, but a copy off the server spares you re-sealing secrets on a new one.

## hakobu on a laptop

You don't need it, but the CLI from the [releases](https://github.com/hakobudev/hakobu/releases) does the same from a laptop, for several servers at once: every `ssh kasl-1 COMMAND` above is `hakobu COMMAND` there, and `hakobu mcp` serves them all to an agent. Install a server with it, and it remembers the server and its operator token:

```bash
hakobu server create kasl-1 --ssh root@203.0.113.7 --ingress public --email you@example.com
```

It reaches the API (on the server's 127.0.0.1:7070) through SSH, with a key it makes for a user, `hakobu-tunnel`, that may reach that port and nothing else; `--tailscale` (with `TS_AUTHKEY`) has the API listen on the tailnet instead. Your age key is asked for the first time and kept in `~/.config/hakobu/owner.pub`. The API's tokens: the **operator**'s, kept in `~/.config/hakobu/servers.toml`, looks, deploys, rolls back, backs up and updates; the **admin**'s, shown once for your password manager, also restores and deletes. A server installed otherwise doesn't show them: `hakobu server create` it from the start.
