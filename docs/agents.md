# Agents (Claude Code, MCP)

hakobu is built so an agent can do the work that's different in each project (the file, the Dockerfile, migrations, DNS) while the parts that guard your data stay in tested code it can't change.

## MCP

Each server has its own MCP server, run on the server itself over SSH: the installer prints the line that adds it to Claude Code, named after the server.

```bash
claude mcp add --scope user kasl-1 -- ssh -T deploy@203.0.113.7 mcp
```

For another client, add a local (stdio) server whose command is `ssh -T deploy@203.0.113.7 mcp`. Nothing of hakobu runs on your machine: it's your `ssh` and your key, which may run hakobu's commands on that server and nothing else. The server tells the agent the address it was reached at, so when it asks you to run something (a secret to type, say), it gives the line whole, `ssh -t deploy@203.0.113.7 secret set …`: nothing about the server goes in `CLAUDE.md`, and your own `~/.ssh/config` may call it what you like.

Its tools: `status`, `deploys`, `logs`, `backups`, `settings` (the server's releases, buckets, mail, updates; no secrets), `deploy`, `rollback`, `backup`, `update` (hakobu itself, to a newer signed release), `init` (set a repository up), `validate` (check `hakobu.toml` and `hakobu.secrets.toml` as the server will) and `pending_secrets` (the secrets you sealed, for it to add).

## The skill

`.claude/skills/hakobu/SKILL.md`, which the `init` tool gives, tells Claude Code how deploys work, how to write `hakobu.toml`, that migrations must work with the version before, how to add secrets without seeing them, and what not to do. A short section in `CLAUDE.md` points to it and says to read the servers through the MCP tools, never a shell or docker. Each `init` gives this hakobu's version of both, and leaves the rest of `CLAUDE.md` alone.

## What an agent can't do

- **Delete or replace data.** `hakobu destroy` and `hakobu restore` are root's on the server, and need a person at a terminal typing a confirmation: without a terminal they refuse. The MCP server has neither, and `.claude/settings.json` denies both to Claude Code as well. The backups' buckets are root's too.
- **Read a secret, or see one.** You type a secret in your own terminal (`ssh -t kasl-1 secret set …`); the agent gets it sealed, and can't open it. The skill tells it never to ask for one in the chat.
- **Get a shell.** Its key runs hakobu's commands on the server and nothing else, as an operator: look, deploy, roll back, back up, update, change the settings but the buckets, or remove a release image that deploys a project (another could take the project then).

The logs an agent reads are written by your apps, and text in them could try to steer it; what it can do through its key is bounded the same either way.

## With hakobu on a laptop

`hakobu init` writes the same files, and `.mcp.json` for `hakobu mcp`, which runs on the laptop and talks to every server it knows, with the operator token: the same tools, for all of them.
