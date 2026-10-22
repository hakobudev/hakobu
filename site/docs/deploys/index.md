# Deploys and CI

Nothing pushes to a server. CI builds each commit into a **release image** and pushes it to a registry under a tag (`main`); the server checks that tag every 30 seconds and deploys each new one. CI needs no access to the server, and the server no inbound connection.

## The release image

`hakobu build`, which the workflow runs, builds each app of `hakobu.toml` (with its Dockerfile, or Railpack), pushes it as `IMAGE-APP`, then pushes the release last: a tiny image whose labels carry `hakobu.toml`, the sealed `hakobu.secrets.toml`, each app's image pinned by digest, and the repository and commit it came from. The release goes last, so a server never sees one whose apps aren't pushed yet. Every app of a commit deploys together.

## A deploy

1. **Plan**: the file against what runs. A wrong file, a secret the server can't open or a renamed database stops here, before anything changes.
2. **A dump** of each database the app uses, if it has a `release` command.
3. **`release`** in the new image, with the app's variables. If it fails, the deploy stops and the running version goes on.
4. **The new version** starts, next to the old one, and must answer its `health` path with 2xx.
5. **Traffic switches** to it; the old version stops. No request is dropped (an app with volumes is the exception: it stops first, see [volumes](https://hakobu.dev/docs/hakobu-toml/index.md)).

A deploy never deletes data: databases, caches and volumes gone from the file are kept, orphaned.

## The commit status

The server sets the commit's `hakobu/deploy` status: pending as it starts, then success, or failure with the reason. The workflow's `hakobu wait` waits for it (15 minutes by default), so the CI job fails when the deploy does. Without the [status token](https://hakobu.dev/docs/secrets/index.md) the release is refused.

The failure email, `hakobu deploys` and `hakobu logs PROJECT/APP` say more.

## Migrations

`release` runs while the old version still serves, and if anything after it fails, the old version goes on against the migrated database. So a migration must work with the code before it:

- add columns nullable or with a default, and tables, freely;
- rename or drop in two deploys: first stop using the column, then drop it;
- don't change a column's type in place: add another and backfill;
- long backfills belong in the worker, not `release` (it's stopped after 30 minutes).

## Rollback

```bash
hakobu rollback crm
```

deploys the release before the current one, with today's secrets, and holds the current one back until a newer one comes. It doesn't undo migrations: the dump taken before the last deploy stays on the server, in `/var/lib/hakobu/snapshots`, and `hakobu restore` puts back a [backup](https://hakobu.dev/docs/backups/index.md). `hakobu deploy crm` checks for a release now instead of in 30 seconds; `--force` deploys the current one again.

## The workflow

`hakobu init` writes `.github/workflows/hakobu.yml`, which calls `hakobudev/hakobu/.github/workflows/build.yml` with:

| Input | |
|---|---|
| `version` | the hakobu release to build with; the same as the workflow's ref |
| `image` | the release image, without its tag; default `ghcr.io/OWNER/REPO` |
| `tag` | the tag the server polls; default `main` |
| `railpack` | install Railpack, for apps without a Dockerfile |
| `railpack-version` | which Railpack; its BuildKit frontend is the same version |
| `runner` | the runner, whose architecture the images get; `ubuntu-24.04-arm` for an arm64 server |
| `wait` | how long to wait for the deploy; default `15m` |

For a registry other than ghcr.io, pass `registry-username` and `registry-password` as secrets. Releases run one at a time per repository, each waiting for its deploy.

Another CI works the same way: run `hakobu build --image IMAGE --tag main` where it can push, then `hakobu wait` (it reads GitHub's statuses, so it needs `GITHUB_TOKEN` and the commit).
