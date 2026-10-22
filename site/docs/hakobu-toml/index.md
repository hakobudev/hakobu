# hakobu.toml

What a project's server runs, at the root of its repository. It's strict: an unknown key, a typo or a reference to nothing is an error, so a mistake stops the deploy instead of quietly turning something off. `hakobu validate` checks it, and CI runs it before each build.

```toml
project = "crm"
server = "kasl-1"

[apps.web]
domain = "crm.example.com"
release = "bin/rails db:migrate"
worker = "bin/jobs"
uses = ["main", "cache"]
volumes = { uploads = "/rails/storage" }
env = { RAILS_LOG_LEVEL = "info" }

[apps.admin]
path = "admin"
domain = "admin.crm.example.com"
uses = ["main"]

[databases.main]

[caches.cache]
memory = "128mb"
```

Names (the project's, apps', databases', caches', volumes') are lowercase letters, digits and `-`, starting with a letter, up to 31 characters.

## The project

| Key | |
|---|---|
| `project` | the project's name, unique on its server |
| `server` | the server it deploys to; any other server refuses the release |

One repository is one project.

## Apps: `[apps.NAME]`

An app is one image serving HTTP, and optionally a worker from the same image.

| Key | |
|---|---|
| `path` | its directory in the repository, default the root; the app is built from the `Dockerfile` there |
| `image` | a ready image to run instead, `evoapicloud/evolution-api:v2.3.7` say: nothing is built. It names a version; the build pins the digest that version names then, which the server runs until the file names another |
| `domain` | where it's served on the web; without one, only the project's other apps reach it. One project's on the server: another's deploy with it is refused |
| `health` | a path that answers 2xx once the app takes requests; default `/up`. A deploy goes live only once it does |
| `port` | the port it listens on; default the image's `EXPOSE`, else 8080. It's in `$PORT` too |
| `release` | a command run once per deploy in the new image before it takes traffic, for migrations: if it fails, the running version stays. See [Deploys](https://hakobu.dev/docs/deploys/index.md) |
| `worker` | the command of a background process from the same image |
| `memory` | its limit, `512mb` or `1gb`; at least `64mb` |
| `cpus` | its CPU limit, `0.5` or `2` |
| `volumes` | name → mount path. Survives deploys and is backed up |
| `env` | variables, plain values only: secrets go in [hakobu.secrets.toml](https://hakobu.dev/docs/secrets/index.md). A value may name one hakobu gives the app as `${NAME}`: see below |
| `uses` | the databases and caches it uses: a list, or a map of name → prefix |

The app must listen on all interfaces (not only 127.0.0.1). `release` and `worker` run with `/bin/sh -c` in its image, which therefore needs a shell.

Each app also gets `HAKOBU_REVISION`, the commit it was deployed from: what to give an error tracker as the release, for one. An app whose image a commit didn't change keeps running, with the commit it was deployed from.

### `uses`

A database gives the app `DATABASE_URL`, `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_USER` and `POSTGRES_PASSWORD`; a cache `REDIS_URL`, `VALKEY_URL`, `REDIS_HOST`, `REDIS_PORT` and `REDIS_PASSWORD`. Two of a kind would set the same variables, so give one a prefix:

```toml
uses = { main = "", jobs = "JOBS" }   # JOBS_REDIS_URL, JOBS_REDIS_HOST, …
```

### One app to another

The project's apps reach each other by name: `http://web:8000` from another app of the project, or its worker, is web's live version, whichever that is during a deploy. The port is the one the app listens on. Nothing outside the project reaches it so.

### Variables from variables

A ready image often wants what hakobu gives under a name of its own. `${NAME}` in a value is a variable hakobu gives the app: one from `uses`, `PORT` or `HAKOBU_REVISION`.

```toml
[apps.wa]
image = "evoapicloud/evolution-api:v2.3.7"
uses = ["gateway"]
env = { DATABASE_CONNECTION_URI = "${DATABASE_URL}?schema=evolution" }
```

### Volumes

An app with volumes is stopped before its new version starts, so two versions never write the same files: a few seconds down on each deploy. The worker shares them.

## Databases: `[databases.NAME]`

A database and its own user on the server's PostgreSQL, which all its projects share; its version is the server's. It has no keys. See [Databases](https://hakobu.dev/docs/databases/index.md).

## Caches: `[caches.NAME]`

A Valkey server of the project's own, with its own password.

| Key | |
|---|---|
| `memory` | its memory, default `256mb`, at least `16mb` |

## What a change does

- **Adding** an app, database, cache or volume makes it on the next deploy.
- **Removing** one doesn't delete it: a database, cache or volume is kept, *orphaned*, and only `hakobu destroy` (a person's, with the admin token, after a last backup) deletes it.
- **Renaming** is removing and adding: the new one starts empty and the old one is orphaned. For a database, the deploy stops instead and says so: deploy the new one, move the data, then take the old one out.
