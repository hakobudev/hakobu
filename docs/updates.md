# Updates

To update a server, run on it, as root:

```bash
sudo hakobu server update            # the latest release
sudo hakobu server update --to v1.3.0
```

It installs only a newer release, and only one signed by hakobu's key; it runs that release's own installer (the `install.sh` it carries), so an update does what installing it by hand does. If the new version doesn't come up within three minutes, the binary from before goes back in and is started again. `/opt/hakobu/update-status.json` says how the last update went.

hakobu itself can ask for one: creating `update-request` in its data directory (empty, or a release's tag) makes systemd run the same update as root (`hakobu-update.path`). hakobu runs as a user that can't write its own binary, so the worst such a request does is install a newer signed release.

A server on v1.2.0 or older updates the first time by running the installer again, which sets this up:

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo HAKOBU_VERSION=v1.3.0 bash
```

The config, the key and the data stay. Apps keep running; their proxies live in hakobu, so they don't answer for about half a second while it restarts. Docker is restarted only if its settings changed.

The CLI on your laptop is the binary from the [release](https://github.com/hakobudev/hakobu/releases), and a project's workflow names its version (`version:` and the ref of `uses:`): bump both together.

## Signed releases

Releases are signed with an Ed25519 key that never leaves the release workflow. The installer and the workflow install a release only if its signature over `hakobu <version>` and its checksums is by that key, so neither a tampered download nor an older release under a newer name gets in.
