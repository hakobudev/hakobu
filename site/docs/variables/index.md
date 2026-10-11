# Variables and secrets

## Where they're set

- **Shared variables** on the project's page go to every app of the project.
- An app's **Variables** tab has its own, on top of the shared ones; its worker can have more (**Settings → Worker**).
- Linked resources add theirs: `DATABASE_URL` and `POSTGRES_*` from a [database](https://hakobu.dev/docs/databases/index.md), `REDIS_URL` and `REDIS_*` from a [cache](https://hakobu.dev/docs/caches/index.md), `S3_*` from a [storage](https://hakobu.dev/docs/storages/index.md). hakobu also sets `PORT` and `SENTRY_DSN`.

When a name is set in more than one place, the app's own value wins over the shared one, which wins over the linked ones. The **Variables** tab shows what the app gets in the end.

Changes apply on the next deploy.

## Sealed variables

A secret (an API key, a payment provider's token) belongs in a **sealed variable**. Its value is passed to the containers but never shown again, not even to you: someone who gets into the panel can't copy it. Seal the same name again to replace a value. Projects, apps and workers each have their own.

Claude never sets a secret: it asks you to seal it (see [Claude](https://hakobu.dev/docs/claude/index.md)).

## Encrypted at rest

Every variable, sealed or not, is encrypted in the panel's database with its master key, kept apart from the data: a copy of the database alone reads nothing. See [Security](https://hakobu.dev/docs/security/index.md).
