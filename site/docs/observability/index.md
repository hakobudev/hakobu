# Errors, logs and alerts

## Logs

An app's **Logs** tab has its output and its worker's, and every deploy's build log is on **Deployments**.

## Errors and traces

hakobu speaks Sentry's protocol: every app gets `SENTRY_DSN` (and `SENTRY_PUBLIC_DSN` for browser SDKs), so any Sentry SDK reports to hakobu with no account anywhere. The app's **Errors** tab groups exceptions with their stack traces; **Performance** shows requests by route, with slow and failed ones kept whole for a week to see where the time went.

What your apps send is kept on the server they run on, in its own database sealed with its key. The panel reads it while that server is connected and keeps none.

The project's page counts each app's problems of the last day (errors, crashes, out-of-memory kills, failed health checks) on its card.

## Email alerts

**Settings → Notifications**: enter your address and confirm it from the email Cloudflare sends. hakobu then emails you when a deploy after a push fails, a backup fails, an app crashes or runs out of memory, the disk is almost full or the master key isn't downloaded; each problem once, again only if it's still there hours later, and once more when it's gone. It goes through Cloudflare Email Service, free for a confirmed address.

## Watched from outside

When the server, hakobu or the tunnel is down, hakobu can't email you itself. While emails are on, hakobu sets up a **watchdog**: a Worker in your Cloudflare account, free plan included, that asks the panel's `/healthz` and each app's address from outside every minute and emails the same address when one stops answering and when it's back. With D1 Edit on the token it keeps 30 days of that, which the app pages and Settings show as uptime and outages, including while the server was down. You can turn it off in **Settings → Notifications**.
