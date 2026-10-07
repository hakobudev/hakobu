# Backups and recovery

## Turning backups on

One click in **Settings**: hakobu creates a private R2 bucket in the Cloudflare account, and from then on backs up every [database](databases.md) and [volume](volumes.md) daily. No S3 keys exist for it: hakobu writes with its Cloudflare token.

## What a backup is

- **Sealed** by the server the data lives on, with that server's own key, which the panel never has. A leak of the panel's database, of the bucket or of the panel's disk exposes nothing.
- **Checked**: its SHA-256 is kept and verified before a restore, and it's restored into a scratch database (or read back, for a volume) right after upload.
- **Locked** for 7 days by the bucket's lock, even against a bug in hakobu. It doesn't stop someone who has taken over the server: the token can change the lock.
- **Rotated**: the last 7, and one a week for a month.

A key that's replaced stays in the server's keyring, so older backups still restore. Keep each server's key away from it: see [Server keys](servers.md#server-keys).

## The panel's own backup

A self-hosted panel backs up its own database to the bucket daily too, sealed with its master key. The key stays off the bucket: download it from **Settings → Security → Download master key** (it asks you to sign in with GitHub again) and keep the file away from the server; download it again after **Replace all secrets**.

## Bringing a panel back on a new server

```sh
curl -fsSL https://hakobu.dev/install.sh -o install.sh
sudo HAKOBU_RESTORE=hakobu-master-key-hakobu.example.com.txt bash install.sh
```

It asks for a Cloudflare API token that can read R2 (or takes `CLOUDFLARE_API_TOKEN`), restores the newest panel backup and starts the panel on its old address, with its tunnel, GitHub App, projects and owner. Then restore each database from its backup page and each volume from its app's page, and redeploy the apps. If the old Cloudflare token was rolled since, run `setup --reconnect` (see [Self-hosting](install.md#if-something-goes-wrong)).

Caches aren't backed up: a cache's data comes back empty.
