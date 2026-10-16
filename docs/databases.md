# Databases

A database is a PostgreSQL database with a role of its own, in one shared PostgreSQL server on the project's server. Only its project's apps reach it.

## Create and link

**New → Database** on the project's page. Link it to an app in the app's **Settings → Resources**: the app gets `DATABASE_URL`, `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_USER` and `POSTGRES_PASSWORD` on its next deploy. The password is generated.

## Shared memory

PostgreSQL keeps the memory of parallel queries, hashes and sorts in `/dev/shm`. Docker gives a container 64 MB of it, too little for a large query, which then fails with `could not resize shared memory segment … No space left on device`. hakobu gives the PostgreSQL server a quarter of the server's RAM, up to 1 GB; it's memory only while a query uses it. The database's page shows the server's PostgreSQL version and its shared memory.

A PostgreSQL server made by an older hakobu has Docker's 64 MB, and Docker can't change it in place: when such a server comes up after updating (the panel's own server as the panel starts, any other as it connects), hakobu stops PostgreSQL cleanly and makes its container anew with the same data. Apps lose their database connections for those few seconds, once. It waits while a backup or restore runs. `dynamic_shared_memory_type` is left as PostgreSQL's default, `posix`; don't change it with `ALTER SYSTEM`, or the larger `/dev/shm` goes unused.

## Size and tables

The database's page shows its size, how much it grew over the last hour and day (from the checks every 30 seconds), and its ten largest tables: their size with indexes and TOAST (where PostgreSQL keeps long values), live rows, dead rows and when they were last vacuumed. Many more dead rows than live ones means rows are updated faster than autovacuum clears their old versions: a table rewritten in place over and over grows until it fills the disk. Claude sees the same through `get_database`.

## Backups

With backups on (one click in **Settings**), every database is backed up daily to a private R2 bucket hakobu creates in your Cloudflare account. Each backup is:

- **sealed** by the server the database runs on, with that server's own key, which the panel never has;
- **checked**: its SHA-256 is kept and verified before a restore, and it's restored into a scratch database right after upload, counting its tables;
- **locked** for 7 days by the bucket, even against a bug in hakobu;
- **rotated**: the last 7 are kept, and one a week for a month.

The database's page lists its backups, makes one now, checks one again and restores one. See [Backups and recovery](backups.md) for the whole picture.

## Rollback with data

Before each deploy of an app, its database is snapshotted on the server. **Rollback with data** puts the snapshot back with the build before, so a migration that went wrong is undone. Rows written since are lost.

## Moving a database in

A database from another host comes in as a backup of a hakobu database. With backups on, on the panel's server:

```bash
ssh old-host 'pg_dump -Fc -U postgres mydb' | (cd /opt/hakobu && sudo -u hakobu ./hakobu database import shop/mydb -)
```

naming the database by its project, `shop`; when several users have a project of that name, `--user <GitHub login>` says whose. The running panel takes the dump and seals it on the database's server, so hakobu must be running and that server connected.

Only pg_dump's custom format (`-Fc`) is taken. On the database's page, **Check** restores the dump into a scratch database and counts its tables; **Restore** replaces the database's data with it (objects become the database role's, grants are dropped). A restore that fails changes nothing, so a trial move and the real one later are the same two steps.
