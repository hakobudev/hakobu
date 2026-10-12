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
