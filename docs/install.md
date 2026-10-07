# Self-hosting

You need a Linux server and a domain on Cloudflare (the free plan is enough). The server needs no public IP or open ports: everything goes through a Cloudflare Tunnel.

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo bash
```

1. The installer shows a Cloudflare link to a new API token of yours with hakobu's permissions filled in: read your domains, manage their DNS records, create a tunnel, keep database backups in R2, email you when something needs you and run a Worker that watches the panel from outside. It's for every account you're a member of: the panel's, and your clients' (see [Cloudflare accounts](cloudflare.md)), so it's the only token you give hakobu. Select **Continue to summary** → **Create Token**, copy the token and paste it into the terminal (it isn't echoed). Or set `CLOUDFLARE_API_TOKEN` before running the installer.
2. Pick one of your domains from the list and the panel's subdomain (default `hakobu`). hakobu creates the tunnel and a DNS record for the panel, in that domain's account: the panel's account.
3. Open the printed link, `https://hakobu.example.com/setup?token=…`, click **Connect GitHub** (this registers a GitHub App for your panel; under it you can put the app on an organization and name it, as people see that name when they sign in), sign in with GitHub and you are the admin. Only that link can claim a fresh panel.

Every app then gets its own address in any domain of the account (`app.example.com`, `myapp.dev`, …); hakobu creates and removes the DNS records itself.

## Narrowing the token

Under **Account Resources** and **Zone Resources** you can narrow the token to the accounts and domains hakobu should use: whoever takes over the server gets the token, and with it the DNS of every domain it covers. **Client IP Address Filtering** makes a copy of the token that leaks elsewhere useless: list both of the server's addresses, the IPv6 one as its `/64`, since the server reaches Cloudflare over IPv6 when it has it.

## If something goes wrong

Lost the setup link: `journalctl -u hakobu | grep setup`.

Give hakobu a new Cloudflare token (after rolling it, say):

```bash
cd /opt/hakobu && sudo -u hakobu ./hakobu setup --reconnect
```

## Email notifications

**Settings → Notifications**: enter your address and confirm it from the email Cloudflare sends. See [Errors, logs and alerts](observability.md#email-alerts).

## Next

- Add [other servers](servers.md), a client's say.
- Let friends deploy on your panel: [Sharing a panel](sharing.md).
- Keep the master key safe: [Backups and recovery](backups.md).
