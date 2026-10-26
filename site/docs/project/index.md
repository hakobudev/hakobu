# A project

A project is a repository with a `hakobu.toml` at its root. With a [server](https://hakobu.dev/docs/install/index.md) and its MCP added to your agent, ask it to set the repository up for hakobu: its `init` tool registers the release image on the server and gives the files, which it writes, leaving any that's already there:

- **`hakobu.toml`**, the project and its server, with an app for the agent to fill in from the repository's Dockerfiles: hakobu builds each app from its own;
- **`hakobu.secrets.toml`**, empty, sealed to the server's key and yours;
- **`.github/workflows/hakobu.yml`**, which builds each push to the default branch and waits for its deploy;
- **`.claude/skills/hakobu/SKILL.md`**, a section in **`CLAUDE.md`** and **`.claude/settings.json`**: see [Agents](https://hakobu.dev/docs/agents/index.md).

The release image is `ghcr.io/OWNER/REPO`, the repository's.

## Then

1. **The file.** Each app gets its `domain`, a `health` path it answers once it's ready (`/up` by default), and the databases and caches it uses: see [hakobu.toml](https://hakobu.dev/docs/hakobu-toml/index.md). The `validate` tool checks it, as CI does.
2. **Secrets.** The server sets the commit's status with a [fine-grained token](https://github.com/settings/personal-access-tokens/new) with **Commit statuses: Read and write** on this repository alone. Make one, and seal it, in your own terminal:

   ```bash
   ssh -t kasl-1 secret set kasl hakobu status_token
   ```

   Your agent then adds it to `hakobu.secrets.toml`; your app's own secrets go the same way. See [Secrets](https://hakobu.dev/docs/secrets/index.md).
3. **Push.** CI builds the apps, pushes them and the release; within 30 seconds the server deploys it, and the job turns green once it's live. `ssh kasl-1 status` and the MCP tools show it.

   The first time, while the job waits, make the release image public: on GitHub, the package `REPO` (not `REPO-APP`), **Package settings → Change visibility → Public**. Everything in it is sealed to the server's key and yours, and the server polls it with no token; until then, `status` says the registry refused. The apps' images stay private: CI seals the job's own token in each release, and the server pulls them with it while the job waits for the deploy. The token dies with the job, so the server keeps none for ghcr.io.

   A private release works too, with a token for the server to read it: a classic token with `read:packages` alone, given with `ssh kasl-1 server set registry ghcr.io --username YOU` (the password typed, hidden). It reads every package of your account, so a public release is the safer one.

## An arm64 server

Images are built on CI's runner, for its architecture. For an arm64 server, set `runner: ubuntu-24.04-arm` in the workflow.

## Tests first

The workflow is a job like any other: put your tests in a job of their own and add `needs: [test]` to the deploy job.

## With hakobu on a laptop

`hakobu init --server kasl-1`, in the repository, writes the same files, with an app already for each directory (the root and one level down) that has a Dockerfile, or a stack it recognizes (Node, Python, Go, …), and `.mcp.json` for `hakobu mcp`; `hakobu secret set hakobu status_token` seals the token straight into the file.
