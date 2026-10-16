# Apps

An app is a GitHub repository that hakobu builds and runs. Every push to its default branch deploys it.

## Add an app

1. Open a project and choose **New → App**.
2. Pick a repository (give the GitHub App access to it first) and check the detected stack. The name and the `<app>.<your domain>` address are filled in; pick another domain from the list, or **private** for an app with no address.
3. The first deploy starts right away.

Link a [database](databases.md), [cache](caches.md) or [storage](storages.md) in the app's **Settings → Resources**, and set its [variables](variables.md). Changes apply on the next deploy.

## Build and start

[Railpack](https://railpack.com) detects how to build and start the app, or a Dockerfile builds it (**Settings → Build**, with the path in the repo for a monorepo). Railpack also reads a `railpack.json` or `Procfile` in the repo, and the app's `RAILPACK_*` variables (`RAILPACK_NODE_VERSION`, `RAILPACK_START_CMD`, …).

Variables frontend tools build into the browser's JavaScript (`VITE_*`, `NEXT_PUBLIC_*`, `PUBLIC_*`, `REACT_APP_*`) reach the build as well, also as a Dockerfile's `ARG`s; no other variable does.

**Start command** runs with `sh` instead of whatever the image starts, Railpack's or a Dockerfile's: `npm run migrate && node server.js`, say.

The app gets `PORT` to listen on; hakobu detects the port it actually listens on either way.

## Deploys without downtime

A deploy builds the new image, snapshots the app's database (so a rollback can undo a migration), starts the new version next to the running one and checks its health: any response for `/`, or a 2xx for the **Health check path** you set. Only then does traffic switch, and the old version stops. A version that doesn't come up never gets traffic, and the deploy's log says why.

An app with [volumes](volumes.md) is stopped before the new version starts (a few seconds of downtime), unless you let both versions share them.

**Redeploy** rebuilds the latest commit. **Rollback** returns to the build before; with **with data** it also restores the database snapshot taken before the live build's deploy, so a bad migration is undone too (rows written since are lost).

## Stop

**Stop app**, under **Settings → Danger zone**, stops the app and its worker, say while it moves elsewhere. It stays stopped through restarts of hakobu and its server, health checks and the watchdog leave it alone, and pushes don't deploy it. **Start**, in place of **Redeploy** at the top of its page, runs it again.

## Worker

**Settings → Worker** runs a background process from the app's image with its own start command and no port: a queue consumer like `celery -A tasks worker`. It gets the app's variables and links, plus its own **Worker variables**, and restarts after every deploy.

## Limits

**Settings → Limits** caps the app's memory and CPUs. An app killed for running out of memory shows it on its page, and you're emailed.

## Domains

**Settings → General → Address** puts the app on any domain of the project's Cloudflare account; hakobu manages the DNS record and HTTPS works right away. Leave the subdomain empty for the root domain. A **private** app has no address: other apps of the project reach it on the project's network (see [Apps calling apps](apps-calling-apps.md)).

## Disk

Each app keeps only its live image and one for rollback, and unused build cache is dropped daily. You're emailed when a server's disk passes 80% and 90%, and when it fills fast enough to be full within 2 hours.
