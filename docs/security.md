# Security

What hakobu does to keep your apps and data yours, and what it doesn't promise.

## Nothing open

Servers need no public IP and no open port. Apps are served through a Cloudflare Tunnel; servers connect to the panel through the panel's tunnel, end to end encrypted. cloudflared runs in its own container and sends app traffic straight to the app's container.

## Isolation

Each project has its own Docker networks: an app reaches its project's apps, databases and caches, never another project's. Each database has its own PostgreSQL role, and each cache its own server and password. On a shared panel, every page, API call and MCP tool finds only what the signed-in user owns; anything else answers as if it didn't exist.

hakobu runs as the unprivileged `hakobu` user, and [rootless Docker](https://docs.docker.com/engine/security/rootless/) as another one, `hakobu-docker`: neither a break-in into hakobu nor a container escape gets root on the server, and an escape can't read hakobu's data. Containers run with `no-new-privileges` and without raw sockets.

## Secrets

Variables, database and cache passwords, storage keys and tokens, but also build logs, worker commands and your apps' errors and logs, are encrypted in the panel's database with a master key kept apart from the data. Without the key nothing secret can be read, and hakobu refuses to start rather than make a new key over data it can't read.

[Sealed variables](variables.md#sealed-variables) are never shown again, not even to you.

**Settings → Security → Replace all secrets** gives every database and cache a new password, every app a new Sentry key, every tunnel a new token, signs everyone out, disconnects AI apps and rotates the master key; it lists what only you can replace (the GitHub App's key, the Cloudflare token).

## Backups

Sealed by each server's own key, which the panel never has: see [Backups and recovery](backups.md). A leak of the panel's database, the backup bucket or the panel's disk exposes nothing of them. A malicious operator of a panel still commands its servers; a self-hosted panel is the way not to trust one.

## Releases

Signed, and installed only by a root service that checks the signature: see [Updates](updates.md#signed-releases).

## Reporting a problem

Found a vulnerability? Please report it privately through [GitHub's security advisories](https://github.com/x0ryz/hakobu/security/advisories/new) rather than in an issue.
