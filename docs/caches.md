# Caches

A cache is a [Valkey](https://valkey.io) server of its own: Redis-compatible, BSD-licensed, for caching, queues (Celery, BullMQ, Sidekiq, arq) and sessions. Any Redis client works with it.

## Create and link

**New → Cache** on the project's page. Link it to an app in the app's **Settings → Resources**: on its next deploy the app gets

| Variable | Value |
| --- | --- |
| `REDIS_URL` | `redis://default:<password>@cache-12:6379/0` |
| `VALKEY_URL` | the same |
| `REDIS_HOST` | `cache-12` |
| `REDIS_PORT` | `6379` |
| `REDIS_PASSWORD` | the generated password |

Each cache runs in its own container on its project's network: only that project's apps reach it, and only with its password.

## Memory

A cache starts with 256 MB; change it on its page, and it restarts with its data. When it's full, writes fail and nothing is evicted, so a queue never silently loses jobs. For a plain cache, give keys a TTL.

## Its data

What's in a cache outlives restarts and redeploys (an append-only file in a volume), but it isn't backed up. Keep nothing there you can't rebuild; put it in a [database](databases.md).

**Replace all secrets** gives every cache a new password; it restarts with its data, and its apps get the password on their restart.
