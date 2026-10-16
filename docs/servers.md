# Servers

One panel runs projects on several servers: yours, or a client's. A self-hosted panel's own server runs projects too; the hosted panel's runs none.

## Add a server

Under **Settings → Servers**, add one and run the command it shows on the new server, as root:

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo HAKOBU_PANEL=https://hakobu.example.com HAKOBU_JOIN_TOKEN=… bash
```

It installs Docker and hakobu there and joins the panel; the token works once, within an hour.

The pencil next to a server renames it. The name is only a label: its projects, keys and tunnels stay with it, and its tunnels in Cloudflare take the new name.

The server needs no open port: it connects to the panel through the panel's tunnel, and what goes between them is encrypted end to end, so Cloudflare doesn't see the variables and passwords it carries.

## Usage and disk

Each server's row shows how full its disk is (Docker's, where the databases, volumes, images and logs are), and **Usage** under it charts its CPU, memory, load and disk. The owner is emailed when the disk passes 80% and 90%, and when it fills fast enough to be full within 2 hours: a table or a log growing out of hand fills a disk in under an hour. Claude sees the same through `get_metrics` with `target: server`.

## When a disk fills

Each server keeps a reserve of 1 GB (2% of a small disk) in `/opt/hakobu/data/disk-reserve`, on the same disk as Docker's data. When under 256 MB are left or the disk is 98% full, hakobu deletes it, so the databases, the logs and hakobu itself can write again while you find the cause, and makes it again once the disk has room. Then:

1. Find what grew: a database's page (or Claude's `get_database`) shows its size, its growth over the last hour and day and its largest tables, with their dead rows; **Usage** under the server charts its disk.
2. Stop what makes it grow: the app's job, or the app itself (**Settings → Danger zone → Stop app**).
3. Give the space back. A table bloated by updates shrinks with `VACUUM FULL <table>` (it locks the table while it rewrites it, and needs room for the table's live rows: the reserve is for that). hakobu already drops unused build cache and old images daily; `docker system df` (below) shows what else takes room. Don't run `docker system prune`: it deletes stopped containers, a stopped app's among them.

hakobu runs its own rootless Docker, which a plain `docker` on the server doesn't see. As root, `/opt/hakobu/hakobu docker …` runs docker against it:

```bash
/opt/hakobu/hakobu docker ps
/opt/hakobu/hakobu docker exec -it hakobu-postgres psql -U postgres -d db_2
/opt/hakobu/hakobu docker system df
```

## Projects on a server

A project is put on a server when it's made; it can move only while it has no apps or databases. Its apps, databases, caches and volumes run there, reached through a tunnel of their Cloudflare account on that server, and its backups go from that server straight to R2 through URLs the panel signs for each part.

A server gets nothing of the panel's: not its master key or tokens, and for builds a GitHub token for the one repository.

## Server keys

Each server has keys of its own, which the panel never sees: they seal its backups and its apps' telemetry. Keep a copy away from the server; the panel asks for it (Settings → Servers, and by email) until you run, on the server,

```bash
cd /opt/hakobu && sudo ./hakobu key export > ~/hakobu-$(hostname).key
```

A new server takes a lost one's keys with `HAKOBU_NODE_KEYS=<that file>` next to the join command, or later with `sudo ./hakobu key import <file>` and a restart. A key that's replaced stays in the server's keyring, so what it sealed still opens.

> Without its key, a lost server's backups can't be opened by anyone: not you, not the panel.
