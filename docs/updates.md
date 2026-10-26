# Updates

Each server looks for a newer hakobu once a day, and `hakobu status` (and the MCP `status`) shows one that's out. To update, from your laptop:

```bash
hakobu server update kasl-1            # the latest release
hakobu server update kasl-1 --to v1.3.0
hakobu server update                   # every server
```

It waits and says how it went. An agent does the same with the MCP tool `update`, with the operator token: an update installs only a newer release signed by hakobu's key, so the most it can do is move a server forward.

On the server, the API only asks: hakobu runs as a user that can't write its own binary, so it creates `update-request` in its data directory, and systemd (`hakobu-update.path`) runs the update as root. That checks the release's signature and checksum, refuses one that isn't newer, and runs the new release's own installer (the `install.sh` it carries), so an update does what installing it by hand does. If the new version doesn't come up within three minutes, the binary from before goes back in and is started again. `/opt/hakobu/update-status.json` says how the last update went, and status shows a failed one.

On the server itself, as root, `sudo hakobu server update [--to v1.3.0]` does the same without the API.

## By itself

Fixes install themselves: at `updates.at` (UTC, an hour after the backups by default) a server installs the latest release if it's a patch of the version it runs (1.2.3 → 1.2.4), and not during a deploy or the backups (it waits for the next day then). A release that didn't install isn't tried again by itself. Newer minor and major releases wait for `hakobu server update`; one that's been out for 14 days mails you.

```toml
[updates]
auto = "patch"   # or "minor" (any 1.x), or "none"
at = "04:00"
```

A server on v1.2.0 or older has no updater yet: run the installer on it once, which sets it up.

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo HAKOBU_VERSION=v1.3.0 bash
```

The config, the key and the data stay. Apps keep running; their proxies live in hakobu, so they don't answer for about half a second while it restarts. Docker is restarted only if its settings changed.

The CLI on your laptop is the binary from the [release](https://github.com/hakobudev/hakobu/releases), and a project's workflow names its version (`version:` and the ref of `uses:`): bump both together, after its server is on that version or newer, as a release built by a newer hakobu may need it (from v1.4.0, `hakobu.toml` is sealed in the release).

## Signed releases

Releases are signed with an Ed25519 key that never leaves the release workflow. The installer and the workflow install a release only if its signature over `hakobu <version>` and its checksums is by that key, so neither a tampered download nor an older release under a newer name gets in.
