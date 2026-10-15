# Errors, logs and alerts

## Logs

An app's **Logs** tab has its output and its worker's, and every deploy's build log is on **Deployments**.

## Errors and traces

hakobu speaks Sentry's protocol: every app gets `SENTRY_DSN` (and `SENTRY_PUBLIC_DSN` for browser SDKs), so any Sentry SDK reports to hakobu with no account anywhere. The app's **Errors** tab groups the same problem seen again into one issue, with how many times and when it was first and last seen: an exception by its type and where it was raised, a logged error by its text with the numbers, IDs and URLs taken out. **Resolve** takes an issue off the open ones until it happens again, which opens it again; **Resolved too** shows the resolved ones, and **With logs** every event one by one. **Performance** shows requests by route, with slow and failed ones kept whole for a week to see where the time went.

What your apps send is kept on the server they run on, in its own database sealed with its key. The panel reads it while that server is connected and keeps none.

The project's page counts each app's open problems seen in the last day (errors, crashes, out-of-memory kills, failed health checks) on its card.

## On the project's page

Each card shows how it's doing now. An app's and a cache's have their CPU and memory, with the last day as a line; a database's its size and open connections; a volume's its size, counted every hour. Every 30 seconds hakobu asks each database (on its server's Postgres) and each cache (a `PING`) whether it answers, and the card says **Running**, **Slow to answer** or, after 3 failed checks in a row, **Not answering**.

## Email alerts

**Settings → Notifications**: enter your address and confirm it from the email Cloudflare sends. hakobu then emails you when a deploy after a push fails, a backup fails, an app crashes or runs out of memory, a database or cache stops answering (and when it answers again), a [server](https://hakobu.dev/docs/servers/index.md) has been away from the panel for 3 minutes (and when it's back), the disk is almost full or the master key isn't downloaded; each problem once, again only if it's still there hours later, and once more when it's gone. It goes through Cloudflare Email Service, free for a confirmed address.

Alerts about a project come from the [Cloudflare account](https://hakobu.dev/docs/cloudflare/index.md) it's in, from `alerts@mail.<a domain there>`, so Cloudflare asks you once in each account to confirm the address; **Settings → Notifications** shows how far each account got. hakobu sets that up once a project of yours is in the account, and removes it when none is.

## Watched from outside

When the server, hakobu or the tunnel is down, hakobu can't email you itself. So each Cloudflare account with your projects gets a **watchdog**: a Worker there, free plan included, that asks each app's address from outside every minute and emails you when apps stop answering and when they're back. The panel's account has one too, which also asks the panel's `/healthz`. With D1 Edit on the token it keeps 30 days of that, which the app pages and Settings show as uptime and outages, including while the server was down. With more than 15 apps in an account, they take turns, a minute each. The admin can turn the panel's off in **Settings → Notifications**.
