# Development

hakobu is open source under the MIT license: [github.com/hakobudev/hakobu](https://github.com/hakobudev/hakobu).

## Running it locally

```bash
go build -o hakobu . && ./hakobu agent --public-host <host that reaches 127.0.0.1:9000 over https>
# e.g. cloudflared tunnel --url http://127.0.0.1:9000 for a throwaway host
```

Requires Go 1.27+ and Docker (plus Railpack and a `buildkit` container for non-Dockerfile builds). State lives in `data/` (SQLite, clones) and its master key in `key/`; `run/` holds the panel's socket for cloudflared.

To look at the pages with made-up data, without GitHub or Cloudflare:

```bash
HAKOBU_UI_DEV=127.0.0.1:8090 go test ./cmd -run TestDevServer -timeout 0
```

`HAKOBU_UI_SHOWCASE=127.0.0.1:8091 go test ./cmd -run TestShowcase -timeout 0` is the same with everything healthy; `just screenshot` takes the README's picture from it.

## Tests

`just check` runs what CI runs: build, tests, govulncheck, golangci-lint and `sqlc diff`. Tests that need Docker (they start their own containers) run with `HAKOBU_DOCKER_TEST=1 go test ./...`; with `HAKOBU_TEST_LINK=1` too, the panel reaches the local machine as it would another server.

## The panel's pages

Its scripts, styles, fonts and icons are embedded in the binary (`cmd/static/`, `cmd/icons.svg`), so it loads nothing from other sites. Its pages are [templ](https://templ.guide) components (`cmd/ui_*.templ`) built from the design system in `cmd/ui_kit.templ` and `cmd/styles.css`; a test fails on colors or sizes written into a page instead. After changing a `.templ` file or classes, run `go generate ./cmd`: it generates the components and rebuilds the stylesheet (needs [bun](https://bun.sh)).

## The database

- Schema: `internal/store/migrations/`, applied at startup.
- Queries: `internal/store/queries.sql`, compiled by [sqlc](https://sqlc.dev) into `internal/store/*.gen.go`: run `just gen` after changing either.

## These docs

The pages of hakobu.dev/docs are made from `docs/*.md` (the menu is `docs/nav.md`) by `go run ./tools/docsgen` into `site/docs/`; a test fails when they're behind the Markdown.

## Releases

Pushing a `vX.Y.Z` tag builds and publishes a release (`.github/workflows/release.yml`), signed with the key in the `HAKOBU_SIGNING_KEY` secret of the `release` environment; without it the release fails rather than going out unsigned. `just release-check` builds a snapshot without signing.

## Layout

- `cmd/`: CLI, web panel, GitHub webhook, Sentry ingest, MCP
- `internal/ops/`: projects, apps, deploys, databases, caches, storages, backups
- `internal/node/`: what runs on a server, locally or over the link to another one
- `internal/deploy/`: Docker Engine API client
- `internal/proxy/`: per-app reverse proxy
- `internal/cloudflare/`: Cloudflare API
- `internal/build/`, `internal/detect/`: cloning and building repositories
- `internal/github/`: GitHub App, OAuth
- `internal/store/`: SQLite: schema, sqlc queries
- `internal/secret/`: encryption of secrets
- `internal/update/`: signed releases
- `internal/backup/`: pg_dump/restore and volume archives
