# Backups and recovery

With a bucket in the [server's config](https://hakobu.dev/docs/servers/index.md), every database and volume on the server is backed up each day at `backups.at` (UTC). Caches aren't: they're caches.

## Each backup is

- **sealed** with [age](https://age-encryption.org) to the server's key and to yours, so the bucket alone reads nothing, and you can open a backup without the server;
- **copied**: written to the primary bucket and to each of `backups.copies`, another provider ideally, with a key that may only write, so a leaked key of the server can't delete the copies;
- **proved**: read back from the primary bucket and restored into scratch (a database counts its tables, a volume's archive is read through) before it counts as done;
- **rotated**: the newest of each of the last 7 days and one a week for 4 weeks are kept (`keep_daily`, `keep_weekly`); one made by hand counts as its day's. A bucket with object lock may refuse a deletion (a backup made by hand the day of another, under a 7-day lock); it's tried again on each rotation, and reported only if it's still refused a month on, a lock longer than the rotation keeps. The parts of a backup that never finished go a day later.

There's no catalog to lose: the backups are listed from the primary bucket itself.

A volume's app and worker are paused for the seconds the archive takes, so files that belong together (SQLite and its journal) are copied as of one moment.

A failed or unproved backup emails you (see [Alerts](https://hakobu.dev/docs/alerts/index.md)).

## Commands

```bash
ssh nova-1 backup              # back up everything now
ssh nova-1 backups crm         # list, newest first, with what reading each back found
```

## Restoring

```bash
ssh -t root@nova-1 hakobu restore crm/main               # a database, from its newest backup
ssh -t root@nova-1 hakobu restore crm/web/uploads --at 2026-10-20T03:00:12Z
```

A restore replaces the data, so it's root's, and needs a person at a terminal: an agent can't run it. (On a laptop, `hakobu restore` needs the admin token, from `HAKOBU_ADMIN_TOKEN` or typed, hidden.) The database or volume must exist on the server (deploy the project first).

## Before each deploy

A deploy with a `release` command first dumps the databases its app uses, on the server, in `/var/lib/hakobu/snapshots`: the state just before a migration, if it goes wrong.
