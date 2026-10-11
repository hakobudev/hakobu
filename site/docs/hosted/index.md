# Hosted panel

[dash.hakobu.dev](https://dash.hakobu.dev) is a hakobu panel for everyone. It runs no apps itself: your projects run on servers you add, in your Cloudflare account.

## Sign in

Sign in with GitHub. Your first sign-in makes your account; there's nothing else to fill in. To deploy a repository, install the panel's GitHub App on your account (or an organization) and pick the repositories it may read; **New app** links there when it has none. It reaches only the repositories you pick.

## Add a server

Under **Settings → Servers**, add a server and give it a name. You get a command to run on it, as root:

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo HAKOBU_PANEL=https://dash.hakobu.dev HAKOBU_JOIN_TOKEN=… bash
```

It installs Docker and hakobu there and connects the server to the panel. The token in it works once, for an hour. The server dials out to the panel, so it needs no open port, and what goes between them is encrypted end to end.

Then keep a copy of the server's key away from it, as the panel asks: see [Server keys](https://hakobu.dev/docs/servers/index.md).

## Connect Cloudflare

Under **Settings → Cloudflare**, the link opens a new API token in your Cloudflare profile with what hakobu needs. Select **Continue to summary** → **Create Token** and paste it into the panel. hakobu connects every account the token reaches, yours and your clients' (see [Cloudflare accounts](https://hakobu.dev/docs/cloudflare/index.md)).

Nothing is made in your account until a project needs it: a tunnel with its first domain, a bucket with its first backup.

## Create a project

**New project** asks for a name, the server it runs on and the Cloudflare account its domains are in. Then add an app from a repository: see [Apps](https://hakobu.dev/docs/apps/index.md).

## What the panel sees

The panel keeps your projects' settings and variables, encrypted, and runs your deploys. It can't open your backups: they're sealed with your server's own key, which never leaves the server. Your apps' logs, errors and traces stay on your server; the panel reads them while you look.
