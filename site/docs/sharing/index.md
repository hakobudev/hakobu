# Sharing a panel

A self-hosted panel can have more users than you: friends or a team who deploy on it without asking you each time.

## Users

Everyone signs in with GitHub. Each user sees only their own projects, servers and Cloudflare accounts. You, the first user, are the admin and keep the panel's own settings: updates, backups, notifications, secrets, the master key and users. **Settings → Users** makes another user an admin, or removes one.

Someone joins with an invite link from **Settings → Users → Invite**: it works once, within a week. Others need the panel's GitHub App to be public to sign in: the panel tells you when it isn't, with the link to make it so. The app then reaches only the repositories each user picks.

By default, users deploy on the panel's server and in its Cloudflare account, as well as on their own.

## Panel-only

A panel shared with people whose apps you don't want on your server, a hosted one say, runs none on its own. Set it in a drop-in (`sudo systemctl edit hakobu`):

```ini
[Service]
Environment=HAKOBU_PANEL_ONLY=1
```

Then every project runs on a server of its owner's (**Settings → Servers**) and in a Cloudflare account of theirs (**Settings → Cloudflare**), with its domains, storages and backups there; the panel's server and account serve only the panel. The installer's token stays the panel's too, so [narrow it](https://hakobu.dev/docs/install/index.md) to the panel's account and domain; you connect your own accounts under **Settings → Cloudflare** like everyone else. Set the drop-in before you first sign in. Anyone with a GitHub account signs up by signing in: no invites. [dash.hakobu.dev](https://hakobu.dev/docs/hosted/index.md) is such a panel.

## Addresses from the panel

A panel-only panel can still give the people you trust addresses of its own, so they deploy without a Cloudflare account: a domain of the panel's account just for this, say `hakobu.app`. Add it to the panel's token (Zone → DNS → Edit on that zone) and to the drop-in:

```ini
[Service]
Environment=HAKOBU_PANEL_ONLY=1
Environment=HAKOBU_SHARED_DOMAIN=hakobu.app
```

**Settings → Users → Give hakobu.app** then lets a user make projects in the panel's account, still on a server of theirs. Their apps take addresses `<name>.hakobu.app` and nothing else of the account:

- one level under the domain, so Cloudflare's free certificate covers it;
- 3 to 63 lowercase letters, digits and dashes, not first or last, nor two after the first two characters (`xn--`);
- not one the panel keeps, such as `www`, `dash`, `docs`, `api`, `admin`, `mail`, `status`, `login`, `auth` or `cdn`;
- first come, first served.

Their backups go to the panel's R2 bucket, so keep an eye on its size (R2 is free up to 10 GB). Their projects get no R2 storages in the panel's account: an app's files go to S3 storage of theirs. **Take hakobu.app** stops them making more such projects and addresses; the addresses they have keep working until they make the apps private or move the projects to a Cloudflare account of theirs.
