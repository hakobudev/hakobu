# Secrets

An app's secrets live in its repository, in `hakobu.secrets.toml` next to `hakobu.toml`, each value sealed on its own with [age](https://age-encryption.org) to two keys: the server's and yours. Anyone with the repository can add or replace a secret, an agent too; only the server and you can read one. The file is committed like any other, and its diffs show which secret changed, never a value.

## Adding one

You type the value, in your own terminal; the server seals it, and your agent puts it in the file:

```bash
ssh -t kasl-1 secret set kasl web STRIPE_KEY    # PROJECT APP NAME; typed, hidden
```

The server keeps the sealed value for 7 days. The agent's MCP tool `pending_secrets` gives it, as `STRIPE_KEY = "age:…"` for `[apps.web]`; the agent writes it into `hakobu.secrets.toml` (replacing the one there), pushes, and clears it. The value never goes through the agent, nor its chat: it sees only what's sealed, which it can't open. From a password manager, without typing:

```bash
op read op://crm/stripe/key | ssh kasl-1 secret set kasl web STRIPE_KEY
```

The names are in the file; to remove one, the agent deletes its line. An app gets its secrets as variables. A secret can't have the name of one the file already sets (in `env`, from `uses`, `PORT` or `HAKOBU_REVISION`): the server refuses that release rather than guess which wins.

## hakobu's own

`APP` `hakobu` is for hakobu itself. `status_token` is the GitHub token that sets the commit's `hakobu/deploy` status: a [fine-grained token](https://github.com/settings/personal-access-tokens/new) with **Commit statuses: Read and write** on the repository alone. A release from a GitHub repository without it is refused, so a deploy is never silent.

## How they reach the server

CI puts the file, still sealed, in the release image; the server opens it with its key as it deploys. Each value is sealed for its place, project, app and name: one key opens every project's on the server, so a value pasted into another project's file, or under another app or name, isn't opened there, and that release is refused. A [rollback](https://hakobu.dev/docs/deploys/index.md) takes the older image but today's secrets, so a rotated key stays rotated.

Database and cache passwords aren't secrets you keep: the server makes them.

Values sealed by hakobu before 1.6 have no place. The server still opens those its releases deployed before, where they were; any other is refused, and `secret set` seals it again with its place.

## A new server

A new server has a new key, which can't open what was sealed to the old one. Put the old server's key back from your copy of `/var/lib/hakobu/key.age`, or start the file anew for the new server and set each secret again (the file names them).

## With hakobu on a laptop

```bash
hakobu secret init               # start the file, sealed to the server's key and yours
hakobu secret set web STRIPE_KEY # read from stdin (or typed, hidden): never an argument
hakobu secret list               # names only
hakobu secret unset web STRIPE_KEY
```

These seal straight into the file, with no server: `secret init` asks it for its public key once.
