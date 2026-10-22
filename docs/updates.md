# Updates

To update a server, run the installer again, with the version if you want a particular one:

```bash
curl -fsSL https://hakobu.dev/install.sh | sudo HAKOBU_VERSION=v1.0.0 bash
```

The config, the key and the data stay. Apps keep running; their proxies live in hakobu, so they don't answer for about half a second while it restarts. Docker is restarted only if its settings changed.

The CLI on your laptop is the binary from the [release](https://github.com/hakobudev/hakobu/releases), and a project's workflow names its version (`version:` and the ref of `uses:`): bump both together.

## Signed releases

Releases are signed with an Ed25519 key that never leaves the release workflow. The installer and the workflow install a release only if its signature over `hakobu <version>` and its checksums is by that key, so neither a tampered download nor an older release under a newer name gets in.
