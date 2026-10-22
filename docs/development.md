# Development

hakobu is open source under the MIT license: [github.com/hakobudev/hakobu](https://github.com/hakobudev/hakobu).

## Building

```bash
go build -o hakobu .
```

Requires Go 1.27+. The server side needs Linux, systemd and Docker; `install.sh` with `HAKOBU_BINARY=./hakobu` installs a local build on a test machine.

## Tests

`just check` runs what CI runs: build, vet, tests, govulncheck and golangci-lint. Tests that need Docker (they start their own containers: PostgreSQL, Valkey, an S3 server) run with `HAKOBU_DOCKER_TEST=1 go test ./internal/apply ./internal/backups`.

## These docs

The pages of hakobu.dev/docs are made from `docs/*.md` (the menu is `docs/nav.md`) by `go run ./tools/docsgen` into `site/docs/`; a test fails when they're behind the Markdown.

## Releases

Pushing a `vX.Y.Z` tag builds and publishes a release (`.github/workflows/release.yml`), signed with the key in the `HAKOBU_SIGNING_KEY` secret of the `release` environment; without it the release fails rather than going out unsigned. `just release-check` builds a snapshot without signing.

## Layout

- `main.go`, `internal/cli/`: the command line and the MCP server
- `internal/spec/`: hakobu.toml
- `internal/seal/`: hakobu.secrets.toml
- `internal/server/`: the daemon: config, API, polling, monitoring
- `internal/apply/`: plans and deploys a release; watches the release images
- `internal/ci/`, `internal/scaffold/`: `hakobu build`/`wait`, `hakobu init` and the skill
- `internal/backups/`: sealed backups to S3, their proof and rotation
- `internal/alert/`, `internal/audit/`: email, pulse, the audit
- `internal/ingress/`: 80/443 with Let's Encrypt, or the tunnel's loopback
- `internal/node/`, `internal/deploy/`, `internal/proxy/`: Docker on the server, each app's proxy
- `internal/registry/`, `internal/status/`: the registry client, the commit status
