# Agents (Claude Code, MCP)

hakobu is built so an agent can do the work that's different in each project (the file, the Dockerfile, migrations, DNS) while the parts that guard your data stay in tested code it can't change. `hakobu init` sets a repository up for that.

## The skill

`.claude/skills/hakobu/SKILL.md` tells Claude Code how deploys work, how to write `hakobu.toml`, that migrations must work with the version before, how to add secrets without reading them, and what not to do. `hakobu init` writes it anew each time, so it matches the hakobu you run; a short section in `CLAUDE.md` points to it.

## MCP

`.mcp.json` adds hakobu's MCP server, `hakobu mcp`, which runs on your laptop and talks to every server you've added, with the operator token. Its tools: `status`, `deploys`, `logs`, `backups`, `deploy`, `rollback` and `backup`. For another client, add it as a local (stdio) server running `hakobu mcp`.

## What an agent can't do

- **Delete or replace data.** `hakobu destroy` and `hakobu restore` need the admin token, which belongs in your password manager, and a person at a terminal typing a confirmation: without a terminal they refuse. `.claude/settings.json` denies both to Claude Code as well. The MCP server has neither.
- **Read a secret.** It can seal a new one to the server, never open one. The skill tells it to ask you to run `hakobu secret set`.
- **Reach the server.** No SSH is needed for any of this, and the API is on your tailnet only.

The logs an agent reads are written by your apps, and text in them could try to steer it; what it can do with the operator token is bounded the same either way.
