---
name: hakobu
description: How this repository deploys with hakobu, and what an agent may and may not do with it. Read before changing hakobu.toml, hakobu.secrets.toml, the Dockerfile, database migrations or the deploy workflow, and before deploying, rolling back, checking whether a push is live or reading a server's state or logs.
---

# Deploying with hakobu

hakobu runs this repository on a server the owner keeps. Nothing pushes to the server: on each push to the default branch, CI (`.github/workflows/hakobu.yml`) builds every app, pushes the images and a release image carrying `hakobu.toml` and `hakobu.secrets.toml`, and the server, polling that image every 30 seconds, deploys it. The CI job waits for the server's `hakobu/deploy` commit status, so a failed deploy turns the commit red with the reason.

A deploy: dump of each database the app uses (when it has a `release` command) → the `release` command in the new image (fails = stop, the live version keeps running) → the new container → its health check → traffic switches → the old version stops. A deploy never deletes data.

## hakobu.toml

Strict: an unknown key, a typo or a reference to nothing fails `hakobu validate`, and CI runs it. Run `hakobu validate` after every change.

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
- Removing a database, cache or volume from the file doesn't delete it either. Deleting is `hakobu destroy`, a person's to run.

## Migrations

The live version keeps serving while `release` migrates the database, and if anything after it fails, the old version goes on running against the migrated schema. So every migration must work with the code before it:

- Add columns nullable or with a default; add tables freely.
- Rename or drop in two deploys: first stop using the column (and add the new one, writing both), deploy; then drop it in a later deploy.
- Don't change a column's type in place; add a new one and backfill.
- Long backfills don't belong in `release` (30 minutes at most); run them from the worker.

## Secrets

`hakobu.secrets.toml` is in the repository, each value sealed with age to the server's key and its owner's. You can add a secret but never read one.

- Ask the person to run `hakobu secret set APP NAME` (it reads the value from stdin, never an argument). Don't run it with a value you made up, and never print, log or commit a secret.
- `hakobu secret list` shows the names; `hakobu secret unset APP NAME` removes one.
- A secret mustn't have the name of a variable hakobu.toml already sets (`env`, `uses`, `PORT`, `HAKOBU_REVISION`).
- `hakobu secret set hakobu status_token`: a fine-grained GitHub token with "Commit statuses: write" on this repository alone. Without it, the server refuses the release.

## Builds

Each app is built from the `Dockerfile` in its directory (`path`), and nothing else: if an app has none, write one. Keep it small and reproducible: a multi-stage build, a pinned base image, the app listening on `$PORT`, and a shell in the final image if the app has a `release` or `worker`. The image is built on CI's runner, so its architecture is the runner's: an arm64 server needs `runner: ubuntu-24.04-arm`.

## Commands and MCP

The `hakobu` MCP server (`hakobu mcp`, in `.mcp.json`; its tools are the commands below, use them first) and the CLI talk to the servers listed in `~/.config/hakobu/servers.toml`:

- `hakobu status`: apps, their health, last deploy, open alerts, on every server.
- `hakobu deploys`, `hakobu logs PROJECT/APP [--worker]`: what happened and why.
- `hakobu deploy [PROJECT]` checks for a new release now; `--force` deploys the current one again (a restart).
- `hakobu rollback PROJECT`: the release before, with today's secrets. It doesn't undo a migration.
- `hakobu backup`, `hakobu backups PROJECT`: back up now, list them.
- `hakobu server update [NAME]` (MCP `update`): patches of hakobu install themselves at night; when status shows a newer minor or major one, update it. Only releases signed by hakobu and newer than the server's; one that doesn't come up is replaced by the one before. Don't update in the middle of something the user is watching (a migration, a deploy).

`hakobu wait` is CI's: it needs the commit and a GitHub token. From here, the servers are read with these, and only these: not ssh, not docker on the server, not `printenv HAKOBU_REVISION` in a container.

After pushing, wait for its run of `.github/workflows/hakobu.yml` (`gh run list -w hakobu.yml -L 1`, then `gh run watch ID`; it waits for the deploy; another workflow going green says nothing) or watch `hakobu deploys` before saying a change is live; if it failed, read the reason there and in `hakobu logs`.

## Never

- `hakobu destroy` or `hakobu restore`: they delete or replace data, need the admin token and a person at the terminal, and are denied in `.claude/settings.json`. If one seems needed, say why and let the person run it.
- SSH to the server, or editing its `/etc/hakobu/server.toml`: that's the owner's (releases, registries, backups, alerts).
- Pushing an image to the release tag by hand, or deploying from anywhere but the default branch.
- Weakening the health check or the workflow's wait to make a red deploy green.
