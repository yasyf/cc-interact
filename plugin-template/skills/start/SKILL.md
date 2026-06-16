---
name: start
description: Start or resume a {{DISPLAY_NAME}} session over the code Claude is working on. Opens a localhost web UI, streams the human's feedback back into this session in realtime so Claude can ask clarifying questions, and blocks edits until the human submits. Use when the user asks to start {{DISPLAY_NAME}}, says "/{{SKILL_NAME}}", or wants to give feedback before Claude proceeds.
---

# /{{SKILL_NAME}}

You are running a {{DISPLAY_NAME}} session. The human interacts with your work in a browser; their feedback streams to you here; you ask clarifying questions that render in the UI; you make **no edits** until they submit. Everything is CLI calls to `{{BINARY_NAME}}` — you are a thin wrapper around it.

The binary is `"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}"` — always invoke it by that absolute path, never as bare `{{BINARY_NAME}}`. If it's missing, run `bash "${CLAUDE_PLUGIN_ROOT}/scripts/install-binary.sh"` once.

> SKELETON: this is the substrate flow (start → wire delivery → react read-only → drain on submit → proceed). Fill the `<domain ...>` markers with your domain's event types, reply shapes, and any background agent you dispatch. Keep one canonical invocation per step.

## 1. Start the session and give the user the URL

```bash
"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" start --session "$CLAUDE_CODE_SESSION_ID" --cwd "$PWD"
```

It prints the session URL first, then a `channel:` line (`active|pending|inactive`) and a `setup:` line (the first-run channel-approval offer). **Show the URL to the user verbatim** and tell them to open it and submit when done. By default `start` resumes this window's open session; `--new` forces a fresh one.

## 2. Wire up event delivery — then keep working

- **`channel: active`** — this window's channel is proven and streaming. Do **not** arm a Monitor (you would receive every event twice). Events arrive as `<channel source="{{MCP_SERVER_NAME}}">` tags carrying the JSON event payloads.
- **`channel: pending`** or **`channel: inactive`** — launch a **Monitor** (persistent) wrapping:

  ```bash
  "${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" watch --session "$CLAUDE_CODE_SESSION_ID" --cwd "$PWD"
  ```

  Use the Monitor tool with `persistent: true`. Each line it prints is one JSON event. `pending` means the channel is wired but unproven — Claude Code may be silently dropping its notifications — so the Monitor is the route.

If a `<channel source="{{MCP_SERVER_NAME}}">` tag arrives while the Monitor is armed, run `"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" channel-ack --session "$CLAUDE_CODE_SESSION_ID" --cwd "$PWD"`, stop the Monitor with **TaskStop**, and rely on tags from then on. Delivery is at-least-once: dedupe by event id.

Either way: **do not block waiting.** Tell the user you're watching and let their feedback arrive.

### First run only: offer to approve the channel

If `start`'s `setup:` line printed `"offer":true`, once delivery is wired and you're idle, ask the user via **AskUserQuestion**: approve {{DISPLAY_NAME}} as a Claude channel? (one admin-password prompt, puts it on the approved allowlist so `--channels plugin:{{PLUGIN_NAME}}@{{PLUGIN_NAME}}` loads with no dev-channels warning).

- **Yes** — `"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" setup-channels --apply`
- **No** — `"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" setup-channels --decline`

If `offer` is false, skip silently.

## 3. React to each event — READ ONLY, make NO code changes

Each event (Monitor line or channel tag) is a JSON object with a `type`. Handle your domain's event types here:

- `<domain event>` — the human gave feedback. **`Read` referenced files for context only.** Do not edit anything. When useful, dispatch your `<domain agent>` (Agent tool, `run_in_background: true`) for background work — see `agents/`.
- **`submit`** — the human submitted. Go to step 4.
- Other types are informational.

To respond — it renders in the UI in realtime:

```bash
"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" reply --comment <id> --kind <domain kind> --body "<text>"
```

`reply` returns immediately. Then go back to waiting. **Never edit code in this phase** — the edit guard blocks it until submit anyway.

## 4. On the `submit` event — drain open questions, then proceed

```bash
"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" feedback --session "$CLAUDE_CODE_SESSION_ID" --cwd "$PWD"
```

This prints the frozen feedback JSON: the full thread history plus any questions the human didn't answer in the UI. For each open question, ask the human via **AskUserQuestion** (≤4 per call; loop if more) and write the answer back:

```bash
"${CLAUDE_PLUGIN_ROOT}/bin/{{BINARY_NAME}}" reply --answer-to <replyId> --answer "<the human's answer>"
```

**Only after the open questions are drained do you make code changes.** Apply the feedback.

## 5. Later rounds

After you make changes, the user can run `/{{SKILL_NAME}}` again. It resumes the **same** session as a new version against the new state, across `/clear` and resume in the same Claude window; `--new` forces a fresh one.
