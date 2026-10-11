# Databases

A database is a PostgreSQL database with a role of its own, in one shared PostgreSQL server on the project's server. Only its project's apps reach it.

## Create and link

**New → Database** on the project's page. Link it to an app in the app's **Settings → Resources**: the app gets `DATABASE_URL`, `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_USER` and `POSTGRES_PASSWORD` on its next deploy. The password is generated.

## Backups

With backups on (one click in **Settings**), every database is backed up daily to a private R2 bucket hakobu creates in your Cloudflare account. Each backup is:

- **sealed** by the server the database runs on, with that server's own key, which the panel never has;
- **checked**: its SHA-256 is kept and verified before a restore, and it's restored into a scratch database right after upload, counting its tables;
- **locked** for 7 days by the bucket, even against a bug in hakobu;
- **rotated**: the last 7 are kept, and one a week for a month.

The database's page lists its backups, makes one now, checks one again and restores one. See [Backups and recovery](https://hakobu.dev/docs/backups/index.md) for the whole picture.

## Rollback with data

Before each deploy of an app, its database is snapshotted on the server. **Rollback with data** puts the snapshot back with the build before, so a migration that went wrong is undone. Rows written since are lost.

## Moving a database in

A database from another host comes in as a backup of a hakobu database. With backups on, on the panel's server:

```bash
ssh old-host 'pg_dump -Fc -U postgres mydb' | sudo -u hakobu /opt/hakobu/hakobu database import shop/mydb -
```

naming the database by its project, `shop`; when several users have a project of that name, `--user <GitHub login>` says whose.

Only pg_dump's custom format (`-Fc`) is taken. On the database's page, **Check** restores the dump into a scratch database and counts its tables; **Restore** replaces the database's data with it (objects become the database role's, grants are dropped). A restore that fails changes nothing, so a trial move and the real one later are the same two steps.
