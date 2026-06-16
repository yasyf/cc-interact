---
name: example-agent
description: PLACEHOLDER — a domain agent your plugin dispatches in the background to do work against the open session (e.g. analyze, summarize, or apply bulk actions) using the channel MCP tools. Replace this description with what your agent actually does and when /{{SKILL_NAME}} should dispatch it.
tools: mcp__plugin_{{PLUGIN_NAME}}_{{MCP_SERVER_NAME}}__reply, mcp__plugin_{{PLUGIN_NAME}}_{{MCP_SERVER_NAME}}__<your_tool>, Read, Grep, Glob
---

PLACEHOLDER agent. The `/{{SKILL_NAME}}` skill dispatches this in the background (Agent tool, `subagent_type: "{{PLUGIN_NAME}}:example-agent"`, `run_in_background: true`) with a JSON request as its prompt; it works the request to completion on its own while the main session keeps handling the human's feedback.

## MCP tool-name pattern

Every tool your channel MCP server exposes is addressable as:

```
mcp__plugin_{{PLUGIN_NAME}}_{{MCP_SERVER_NAME}}__<tool_name>
```

where `{{PLUGIN_NAME}}` is the plugin manifest `name`, `{{MCP_SERVER_NAME}}` is the key under `mcpServers` in `plugin.json` (both set in `.claude-plugin/plugin.json`), and `<tool_name>` is the tool registered by `{{BINARY_NAME}} {{MCP_SUBCOMMAND}}`. List exactly the tools this agent needs in the `tools:` frontmatter above — the agent is sandboxed to that allowlist. `reply` (post back into the session) is provided by the channel substrate; the rest are your domain's tools.

## What to do

1. Parse the request JSON from your dispatch prompt.
2. Read context via the channel tools (and `Read`/`Grep`/`Glob` on repo files the request needs).
3. Do the domain work — write results back through your channel MCP tools, never by editing repo files.
4. Close the loop on the request and emit a one-line final message: what you did, or why it failed.
