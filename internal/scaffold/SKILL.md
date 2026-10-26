---
name: hakobu
description: How this repository deploys with hakobu, and what an agent may and may not do with it. Read before changing hakobu.toml, hakobu.secrets.toml, the Dockerfile, database migrations or the deploy workflow, and before deploying, rolling back, checking whether a push is live or reading a server's state or logs.
---

# Deploying with hakobu

hakobu runs this repository on a server the owner keeps. Nothing pushes to the server: on each push to the default branch, CI (`.github/workflows/hakobu.yml`) builds every app, pushes the images and a release image carrying `hakobu.toml` and `hakobu.secrets.toml`, and the server, polling that image every 30 seconds, deploys it. The CI job waits for the server's `hakobu/deploy` commit status, so a failed deploy turns the commit red with the reason.

A deploy: dump of each database the app uses (when it has a `release` command) → the `release` command in the new image (fails = stop, the live version keeps running) → the new container → its health check → traffic switches → the old version stops. A deploy never deletes data.

## hakobu.toml

Strict: an unknown key, a typo or a reference to nothing fails validation, and CI runs it. Check it with the MCP tool `validate` after every change.

```toml
project = "crm"            # lowercase letters, digits, -
server = "kasl-1"          # the server it deploys to (a guard: another server refuses it)

[apps.web]                 # an app: one image, serving HTTP
path = "."                 # its directory, with its Dockerfile
domain = "crm.example.com" # its address on the web; none = not served
health = "/up"             # the default; must answer 2xx once it takes requests
port = 8080                # default: the image's EXPOSE, else $PORT (always set)
release = "bin/migrate"    # once per deploy, in the new image, before traffic
worker = "bin/jobs"        # a background process from the same image
memory = "512mb"
cpus = 1
volumes = { uploads = "/app/uploads" }  # name → mount path; survives deploys
env = { LOG_LEVEL = "info" }            # plain values only; secrets go in hakobu.secrets.toml
uses = ["main", "cache"]   # or { main = "", cache = "JOBS" } for JOBS_REDIS_URL…

[databases.main]           # a database and its user on the server's PostgreSQL

[caches.cache]             # a Valkey (Redis-compatible) of the project's own
memory = "256mb"
```

- `uses` a database sets `DATABASE_URL`, `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD`; a cache sets `REDIS_URL`, `VALKEY_URL`, `REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD`. With a prefix, `<PREFIX>_` goes in front. Passwords are made on the server; never put one in the file.
- `image = "name:version"` instead of `path` runs a ready image (nothing built); `env` values can name what hakobu gives as `${DATABASE_URL}`, `${PORT}`… for an image that wants other names.
- Apps of a project reach each other by app name: `http://web:8000` (the port it listens on); an app without `domain` is reached only so.
- Every app gets `HAKOBU_REVISION`, the commit it was deployed from (an error tracker's release, for one).
- The app must listen on `$PORT` (or `port`) on all interfaces, not 127.0.0.1, and answer `health`. A missing health endpoint is the most common failed deploy: add one, or point `health` at a path that answers 2xx without a login.
- `release` and `worker` run with `/bin/sh -c` in the app's image, so the image needs a shell (not distroless or scratch).
- An app with volumes is stopped before its new version starts (a few seconds down), so two versions never write the same files.
- The PostgreSQL version is the server's, not the project's. Renaming a volume or cache in the file doesn't rename it: the old one is kept, orphaned, and a new empty one is made; renaming a database stops the deploy. Don't rename them.
- Removing a database, cache or volume from the file doesn't delete it either. Deleting is `hakobu destroy`, a person's to run, as root on the server.

## Migrations

The live version keeps serving while `release` migrates the database, and if anything after it fails, the old version goes on running against the migrated schema. So every migration must work with the code before it:

- Add columns nullable or with a default; add tables freely.
- Rename or drop in two deploys: first stop using the column (and add the new one, writing both), deploy; then drop it in a later deploy.
- Don't change a column's type in place; add a new one and backfill.
- Long backfills don't belong in `release` (30 minutes at most); run them from the worker.

## Secrets

`hakobu.secrets.toml` is in the repository, each value sealed with age to the server's key and its owner's. You can add a secret but never read one, and you never see its value.

- Ask the person to run, in their own terminal, `ssh -t SERVER secret set PROJECT APP NAME` (SERVER: what the server's MCP command connects to, like `deploy@1.2.3.4`). They type the value there, unseen; the server seals it and keeps it for you. Then the MCP tool `pending_secrets` gives the sealed values: write each as `NAME = "age:…"` under `[apps.APP]` (`[hakobu]` for APP `hakobu`), keeping the file's `recipients`, push, and call `pending_secrets` again with `clear`. With hakobu on the laptop, `hakobu secret set APP NAME` seals one straight into the file.
- Never ask for a secret's value in the chat, never make one up, and never print, log or commit one.
- A secret mustn't have the name of a variable hakobu.toml already sets (`env`, `uses`, `PORT`, `HAKOBU_REVISION`).
- The status token, `hakobu status_token`: a fine-grained GitHub token with "Commit statuses: write" on this repository alone. Without it, the server refuses the release.

## Builds

Each app is built from the `Dockerfile` in its directory (`path`), and nothing else: if an app has none, write one. Keep it small and reproducible: a multi-stage build, a pinned base image, the app listening on `$PORT`, and a shell in the final image if the app has a `release` or `worker`. The image is built on CI's runner, so its architecture is the runner's: an arm64 server needs `runner: ubuntu-24.04-arm`.

## MCP and commands

Each server has its own MCP server, named after it (the `server` in hakobu.toml), which runs on the server itself over SSH (`ssh -T deploy@HOST mcp`); use its tools first:

- `status`: apps, their health, last deploy, open alerts.
- `deploys`, `logs` (`PROJECT/APP`, a worker's too): what happened and why.
- `deploy` checks for a new release now; with `force` it deploys the current one again (a restart).
- `rollback`: the release before, with today's secrets. It doesn't undo a migration.
- `backup`, `backups`: back up now, list them.
- `settings`: the server's settings; `validate`: check hakobu.toml and hakobu.secrets.toml; `init`: set a new repository up; `pending_secrets`: see Secrets.
- `update`: patches of hakobu install themselves at night; when status shows a newer minor or major one, update it. Only releases signed by hakobu and newer than the server's; one that doesn't come up is replaced by the one before. Don't update in the middle of something the user is watching (a migration, a deploy).

The person runs the same as commands: `ssh deploy@HOST status`, `ssh deploy@HOST logs PROJECT/APP`, `ssh deploy@HOST help` for the rest. That SSH key runs hakobu's commands and nothing else. With hakobu on the laptop, `hakobu mcp` and `hakobu status`… do the same for the servers in `~/.config/hakobu/servers.toml`.

`hakobu wait` is CI's: it needs the commit and a GitHub token. From here, the servers are read with these, and only these: not a shell on the server, not docker, not `printenv HAKOBU_REVISION` in a container.

After pushing, wait for its run of `.github/workflows/hakobu.yml` (`gh run list -w hakobu.yml -L 1`, then `gh run watch ID`; it waits for the deploy; another workflow going green says nothing) or watch `deploys` before saying a change is live; if it failed, read the reason there and in `logs`.

## Never

- `hakobu destroy` or `hakobu restore`: they delete or replace data, are root's on the server and need a person at the terminal, and are denied in `.claude/settings.json`. If one seems needed, say why and let the person run it.
- A shell on the server as root (once, to install hakobu, is the exception), or editing its `/etc/hakobu/server.toml`. Its settings (releases, registries, mail, updates) are `ssh deploy@HOST server set …`'s; the MCP tool `settings` shows them. A release is registered by the `init` tool; the backups' bucket is the owner's (root's).
- Pushing an image to the release tag by hand, or deploying from anywhere but the default branch.
- Weakening the health check or the workflow's wait to make a red deploy green.
