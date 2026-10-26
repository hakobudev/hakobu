# Security

What hakobu does to keep your apps and data yours, and what it doesn't promise.

## Nothing comes in

The server pulls its releases; no CI or panel connects to it. You and your agent reach it over SSH alone, with keys that run hakobu's commands and nothing else (no shell, no files, no forwarding); hakobu's API listens on loopback and a Unix socket, or on the tailnet, and nowhere else (any other address is refused); hakobu's own releases are signed, and install only if newer, and the web reaches apps only through 80/443 for their own domains, or a Cloudflare Tunnel with no port open at all. The [audit](https://hakobu.dev/docs/alerts/index.md) emails you about any other port that listens.

## Who may do what

On the API's socket the kernel says who connects: root may do anything, the SSH user (`deploy`, your agent's and yours) what an operator may: look, deploy, roll back, back up, update hakobu, change the settings but the backups' buckets. No token is kept for either. Restore and destroy are root's, need a person at a terminal typing a confirmation, and delete only what the file no longer has, after a last backup.

For hakobu on a laptop, the API's tokens do the same over TCP: the **operator** token, and the **admin** token, which alone restores or deletes; the server keeps only their SHA-256.

SSH is as safe as its keys: turn passwords off (the installer warns while they're on), and guard your key as you would the server.

## Isolation

hakobu runs as the unprivileged `hakobu` user, in a sandboxed systemd unit, and [rootless Docker](https://docs.docker.com/engine/security/rootless/) as another, `hakobu-docker`: neither a break-in into hakobu nor a container escape gets root, and an escape can't read hakobu's data. Containers run with `no-new-privileges` and without raw sockets. Each project has its own Docker network; each database its own PostgreSQL user, and each cache its own server and password.

## Secrets and backups

Sealed with age to the server's key and yours: see [Secrets](https://hakobu.dev/docs/secrets/index.md) and [Backups](https://hakobu.dev/docs/backups/index.md). The repository, the registry and the backup buckets hold nothing readable without one of the two keys. Backup copies go with a key that may only write.

## What's beyond it

The release image is trusted as it comes: whoever can push to its tag deploys to the server. A public release shows its repository, its commits and its apps' image names; the rest is sealed. Guard the registry, and CI's ability to push, as you would SSH. A malicious app can read its own secrets and its own databases, as any app can. A release names its project, and only the release image that first deployed a project may deploy it again, so one repository's CI can't take another project, and its databases; removing that image from the server needs the admin token.

## Releases

Signed, and installed only after checking the signature: see [Updates](https://hakobu.dev/docs/updates/index.md).

## Reporting a problem

Found a vulnerability? Please report it privately through [GitHub's security advisories](https://github.com/hakobudev/hakobu/security/advisories/new) rather than in an issue.
