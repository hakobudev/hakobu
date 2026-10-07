# Claude (MCP)

hakobu is an MCP server at `https://<panel address>/mcp` (on the hosted panel, `https://dash.hakobu.dev/mcp`), with its own OAuth: an app asking for access sends you to the panel, you sign in with GitHub and allow it.

## Connect

- **claude.ai**, the desktop and mobile apps: Settings → Connectors → Add custom connector, with that address.
- **Claude Code**:

  ```bash
  claude mcp add --transport http hakobu https://dash.hakobu.dev/mcp
  ```

  then `/mcp` to sign in.
- **VS Code** (Copilot's agent mode): **MCP: Add Server** → HTTP, with that address.

## What it can do

Its tools list apps and show one in detail (variable names, never values), read deploy logs, the apps' output and errors with stack traces, and wait for a deploy to finish, so Claude can push a fix and check it landed.

With the **Deploy and set up** box ticked when you allow the app, it can also deploy, roll back and restart workers, and set a repository up from nothing: make a project, the app, a PostgreSQL database, a cache or a volume, set variables and how the app builds and starts. Tell your agent "deploy this repo" and it can do the rest.

## What it can't

Nothing deletes anything or reveals a secret. Secrets you add yourself as [sealed variables](variables.md#sealed-variables); the agent is told to ask you.

The logs and errors Claude reads are written by your apps, and text in them could try to steer it. You're emailed when an app connects; **Settings → AI apps** lists them and disconnects one, and **Replace all secrets** disconnects all. Tokens last an hour and are renewed for up to 30 days of disuse; a renewal token used twice disconnects its app.

## Self-hosted panels

The panel's Cloudflare zone must let Anthropic's servers (`160.79.104.0/21`) reach `/mcp`, `/oauth/*` and `/.well-known/*`: a WAF rule, Bot Fight Mode or Cloudflare Access in front of the panel blocks them.
