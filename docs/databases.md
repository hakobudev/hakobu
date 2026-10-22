# Databases

`[databases.NAME]` in [hakobu.toml](hakobu-toml.md) is a PostgreSQL database with its own user, in the server's one PostgreSQL, which every project on the server shares. Only the apps that `use` it get its address and password; the password is made on the server.

```toml
[apps.web]
uses = ["main"]

[databases.main]
```

The app gets `DATABASE_URL` (`postgres://…`), `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_USER` and `POSTGRES_PASSWORD`; with a prefix (`uses = { main = "APP" }`), `APP_DATABASE_URL` and so on.

## Its version

The PostgreSQL image is the one this hakobu release pins, for every project on the server: a database has no version of its own.

## Shared memory

PostgreSQL keeps the memory of parallel queries, hashes and sorts in `/dev/shm`, of which Docker gives a container 64 MB, too little for a large query (`could not resize shared memory segment … No space left on device`). hakobu gives PostgreSQL a quarter of the server's memory, up to 1 GB; it's memory only while a query uses it.

## Migrations

Run them as the app's `release` command, and keep them backward compatible: see [Deploys](deploys.md#migrations). Before each deploy with a `release`, the databases the app uses are dumped on the server.

## Backups

Every database is backed up daily, sealed, checked by a restore into scratch, and kept 7 days plus 4 weeks: see [Backups and recovery](backups.md).

## Deleting one

Removing it from the file keeps it, orphaned. `hakobu destroy PROJECT/NAME` deletes it after a last backup, with the admin token, at a terminal, typing its name to confirm.
