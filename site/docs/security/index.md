# Security

What hakobu does to keep your apps and data yours, and what it doesn't promise.

## Nothing comes in

The server pulls its releases; no CI, panel or agent connects to it. hakobu's API listens on the tailnet only (an address on every interface is refused), and the web reaches apps only through 80/443 for their own domains, or a Cloudflare Tunnel with no port open at all. The [audit](https://hakobu.dev/docs/alerts/index.md) emails you about any other port that listens.

## Two tokens

The **operator** token (laptop, agent) can look, deploy, roll back and back up. The **admin** token alone can restore or delete data; keep it in your password manager. The server keeps only their SHA-256. Restore and destroy also need a person at a terminal typing a confirmation, and delete only what the file no longer has, after a last backup.

## Isolation

hakobu runs as the unprivileged `hakobu` user, in a sandboxed systemd unit, and [rootless Docker](https://docs.docker.com/engine/security/rootless/) as another, `hakobu-docker`: neither a break-in into hakobu nor a container escape gets root, and an escape can't read hakobu's data. Containers run with `no-new-privileges` and without raw sockets. Each project has its own Docker network; each database its own PostgreSQL user, and each cache its own server and password.

## Secrets and backups

Sealed with age to the server's key and yours: see [Secrets](https://hakobu.dev/docs/secrets/index.md) and [Backups](https://hakobu.dev/docs/backups/index.md). The repository, the registry and the backup buckets hold nothing readable without one of the two keys. Backup copies go with a key that may only write.

## What's beyond it

The release image is trusted as it comes: whoever can push to its tag deploys to the server. Guard the registry, and CI's ability to push, as you would SSH. A malicious app can read its own secrets and its own databases, as any app can.

## Releases

Signed, and installed only after checking the signature: see [Updates](https://hakobu.dev/docs/updates/index.md).

## Reporting a problem

Found a vulnerability? Please report it privately through [GitHub's security advisories](https://github.com/hakobudev/hakobu/security/advisories/new) rather than in an issue.
