# A project

A project is a repository with a `hakobu.toml` at its root. In the repository, with a [server](https://hakobu.dev/docs/install/index.md) added:

```bash
hakobu init --server kasl-1
```

It writes, leaving any file that's already there:

- **`hakobu.toml`**, with an app for each directory (the root and one level down) that has a Dockerfile or a stack Railpack builds;
- **`.github/workflows/hakobu.yml`**, which builds each push to the default branch and waits for its deploy;
- **`.claude/skills/hakobu/SKILL.md`**, a section in **`CLAUDE.md`**, **`.mcp.json`** and **`.claude/settings.json`**: see [Agents](https://hakobu.dev/docs/agents/index.md).

The image goes to `ghcr.io/OWNER/REPO` (from `origin`); `--image` gives another.

## Then

1. **The file.** Give each app its `domain`, a `health` path it answers once it's ready (`/up` by default), and the databases and caches it uses: see [hakobu.toml](https://hakobu.dev/docs/hakobu-toml/index.md). `hakobu validate` checks it.
2. **Secrets.** `hakobu secret init` starts `hakobu.secrets.toml`, sealed to the server's key and yours. Add the token that lets the server set the commit's status, a [fine-grained token](https://github.com/settings/personal-access-tokens/new) with **Commit statuses: Read and write** on this repository alone:

   ```bash
   hakobu secret set hakobu status_token
   ```

   and your app's own secrets with `hakobu secret set APP NAME`. See [Secrets](https://hakobu.dev/docs/secrets/index.md).
3. **The server** needs to know the release image, and to be allowed to pull it. In its `/etc/hakobu/server.toml`, then `sudo systemctl restart hakobu`:

   ```toml
   releases = ["ghcr.io/kasl/crm:main"]

   [registries."ghcr.io"]
   username = "kasl"
   password = "github_pat_…"   # a token that may read the packages
   ```

   See [Server config](https://hakobu.dev/docs/servers/index.md).
4. **Push.** CI builds the apps, pushes them and the release; within 30 seconds the server deploys it, and the job turns green once it's live. `hakobu status` and `hakobu deploys` show it from your laptop.

## An arm64 server

Images are built on CI's runner, for its architecture. For an arm64 server, set `runner: ubuntu-24.04-arm` in the workflow.

## Tests first

The workflow is a job like any other: put your tests in a job of their own and add `needs: [test]` to the deploy job.
