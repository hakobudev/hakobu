# Cloudflare accounts

hakobu works in your Cloudflare accounts with one API token of yours: it reads your domains, manages their DNS records, creates tunnels, keeps backups in R2 and sends you email.

## Yours and your clients'

A project can live in another Cloudflare account than the panel's: yours, or a client's, who keeps it. Clients invite you to theirs (**Manage Account → Members**); your token then reaches them all. hakobu checks it can do what it needs in each account and connects them, listed under **Settings → Cloudflare**. It keeps up every ten minutes: a client who invites you later shows up by itself, one who removes you goes (or, while a project still uses their account, you get an email). To leave an account out, leave it out of the token. **Replace token** swaps it for all of them.

Other users of a shared panel connect their accounts the same way, with a token of their own made with the link under **Settings → Cloudflare**.

## What's made in an account

Nothing, until a project there needs it. With the first domain of a project in it on a server, hakobu creates a tunnel there, which only that account's projects on that server go through, with no way to the panel. Their domains, R2 storages and database and volume backups are in the account; the buckets stay when the account goes from hakobu.

## Why a token, not "Sign in with Cloudflare"

A sign-in gives no keys for R2's S3 API, which backups go through, and Cloudflare's sign-in turns server networks away (Hetzner's always; see [workers-sdk#11081](https://github.com/cloudflare/workers-sdk/issues/11081)), so the panel couldn't keep it alive by itself. A token goes straight from the dashboard to your panel, never needs renewing and works from any network.
