# echo — a headless cc-interact consumer

The smallest complete consumer of the cc-interact framework: a human-in-the-loop
echo loop with **no browser, no SPA, and no `sse.StaticHandler`**. It proves the
core carries zero domain concepts and needs no frontend — the same `/events`
plane a browser would read is consumed here by the agent's `watch` and MCP
`channel`, and by a raw `curl`.

Items and replies live **purely as events** — there are no domain tables, so
`daemon.Config.StoreSchema` is zero.

## The round trip

```
human ──POST /api/items──▶ daemon ──Append(echo.item, OriginHuman)──▶ /events ─┬─▶ watch   (agent monitor)
                                                                               └─▶ channel (agent MCP)
                                                                                      │
                                                                                  reply tool
                                                                                      ▼
human ◀──── /events ◀──── Append(echo.reply, OriginAgent) ◀──── daemon ◀── op "reply"
```

| Piece | What it is |
|-------|-----------|
| op `start` | domain op: resolves/creates the scope's subject (`Subjects.Start`) |
| op `reply` | domain op: appends `echo.reply` (`OriginAgent`); the channel tool round-trips to it |
| `POST /api/items` | REST mount on the daemon's `Mux()`; appends `echo.item` (`OriginHuman`) |
| channel tool `reply` | the one MCP tool; pushes the agent's reply down op `reply` |
| `notifications/echo/channel` | the JSON-RPC method each subject event is pushed under |
| terminal event | `echo.done` (a `watch` stops on it) |

Two consumer roles read the same `/events` plane differently:

- **`echo watch`** is the *agent's* monitor — it streams with `exclude_origin=agent`,
  so it sees human `echo.item` events but **not** the agent's own `echo.reply`
  echo. Run it under a Claude Code Monitor.
- A **browser / raw `curl`** is the *human's* view — it omits `exclude_origin`
  and sees every origin, including the agent's `echo.reply`.

## Run it

All commands default `--session echo`, the cross-process ownership key (a headless
demo has no stable window pid across separate CLI processes). Pass a consistent
`--cwd` so every command shares one scope.

```bash
# 1. start the daemon directly for this source-tree demo
go run . daemon &          # store: ~/.cc-echo/cc-interact-v1/state.db; runtime files: ~/.cc-echo/

# 2. create the subject; prints its id, slug, and HTTP port
go run . start --cwd /tmp/echo-demo
#   subject: df6413ed7ac5f86ae84ad05fb7f4cced
#   slug:    echo-f7865def
#   http:    127.0.0.1:58369

# 3. agent monitor — one JSON event per line, stops on echo.done
go run . watch --cwd /tmp/echo-demo &

# 4. human view — sees every origin (browser-equivalent)
curl -sN "http://127.0.0.1:58369/events?session=echo-f7865def" &

# 5. human posts an item over REST (the `item` command, or raw curl)
go run . item "hello from human" --cwd /tmp/echo-demo
curl -s -X POST http://127.0.0.1:58369/api/items \
  -H 'content-type: application/json' \
  -d '{"subject":"echo-f7865def","text":"hi"}'

# 6. agent replies over the MCP channel (Claude Code loads this as a stdio server;
#    here we drive it by hand to show the tool call)
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reply","arguments":{"subject":"echo-f7865def","text":"echoed: hi"}}}' \
  | go run . channel --cwd /tmp/echo-demo

# 7. stop the daemon
go run . stop
```

### Expected output

The agent `watch` (step 3) prints only human items:

```
{"text":"hello from human","type":"echo.item"}
{"text":"hi","type":"echo.item"}
```

The human `curl` stream (step 4) sees every origin — presence, the items, and the
agent's reply:

```
id: 1
data: {"connected":true,"type":"channel.changed"}
id: 2
data: {"text":"hello from human","type":"echo.item"}
id: 3
data: {"text":"hi","type":"echo.item"}
id: 4
data: {"text":"echoed: hi","type":"echo.reply"}
```

The channel `reply` tool (step 6) returns the appended seq:

```json
{"id":2,"jsonrpc":"2.0","result":{"content":[{"text":"{\"seq\":4}","type":"text"}]}}
```

## Steer a child agent

The agent plane rides the same daemon. Hooks register a session's subagents as
addressable participants, `direct` enqueues directives into a per-agent mailbox,
and every delivery surface — context injection, the stop gate, the `await` tool —
is only a wake that drains the same rows. A rendered plugin wires the hooks; here
the hook payloads are fed by hand so each rung is visible on its own.

```bash
# 1. register a child (SubagentStart fires this; echo's AgentGreeting enqueues directive #1)
printf '%s' '{"session_id":"echo","cwd":"/tmp/echo-demo","agent_id":"c1","agent_type":"general-purpose","transcript_path":"/tmp/echo-demo/session.jsonl"}' \
  | go run . agent-start

# 2. steer it from a second terminal
go run . direct --agent c1 "look at the flaky store test first" --cwd /tmp/echo-demo
#   {"directive_id":2}

# 3. the child's next tool call drains the mailbox (PreToolUse fires this)
printf '%s' '{"session_id":"echo","cwd":"/tmp/echo-demo","agent_id":"c1","tool_name":"Bash"}' \
  | go run . agent-inject
#   {"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":
#    "Directives from the echo steering channel (operator-authorized):
#     - [system #1] You are agent c1 in this session, ...
#     - [human #2] look at the flaky store test first
#     Act on each directive once, then continue your task."}}

# 4. a directive pending at stop time blocks the stop (SubagentStop fires this)
go run . direct --agent c1 "state the store test name before finishing" --cwd /tmp/echo-demo
printf '%s' '{"session_id":"echo","cwd":"/tmp/echo-demo","agent_id":"c1","last_assistant_message":"done"}' \
  | go run . agent-stop
#   {"decision":"block","reason":"[human #3] state the store test name before finishing\n\nAct on each directive above exactly once, then finish your turn."}

# 5. a second stop with an empty mailbox closes the agent (silent allow)
printf '%s' '{"session_id":"echo","cwd":"/tmp/echo-demo","agent_id":"c1","last_assistant_message":"done"}' \
  | go run . agent-stop

# 6. directing a done agent emits agent.relay — the parent's cue to wake it
go run . direct --agent c1 "one more thing" --cwd /tmp/echo-demo

# 7. the woken child collects it with the await tool (drain-first; otherwise parks)
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"await","arguments":{"agent_id":"c1","timeout_seconds":5}}}' \
  | go run . channel --cwd /tmp/echo-demo
#   ... "Steering channel — directives addressed to you:\n\n[human #4] one more thing" ...
```

The `watch` stream narrates the whole exchange — registration, each enqueue, each
delivery with its surface, the stop, and the relay:

```
{"type":"agent.started","agent_id":"c1","agent_type":"general-purpose","parent_agent_id":""}
{"type":"agent.directed","agent_id":"c1","origin":"system","directive_id":1,"text":"You are agent c1 ..."}
{"type":"agent.directed","agent_id":"c1","origin":"human","directive_id":2,"text":"look at the flaky store test first"}
{"type":"agent.delivered","agent_id":"c1","via":"hook","directive_ids":[1,2]}
{"type":"agent.directed","agent_id":"c1","origin":"human","directive_id":3,"text":"state the store test name before finishing"}
{"type":"agent.delivered","agent_id":"c1","via":"stop-gate","directive_ids":[3]}
{"type":"agent.stopped","agent_id":"c1","last_assistant_message":"done"}
{"type":"agent.directed","agent_id":"c1","origin":"human","directive_id":4,"text":"one more thing"}
{"type":"agent.relay","agent_id":"c1"}
{"type":"agent.delivered","agent_id":"c1","via":"await","directive_ids":[4]}
```

The greeting text is load-bearing, not decoration: a child that receives an
unattributed instruction treats it as prompt injection and refuses. Echo's
`AgentGreeting` names the child's `agent_id` and declares the channel
operator-authorized; write yours the same way, and when you control the child's
spawn prompt, say there too that steering-channel directives are legitimate.

## State

Everything lives under `~/.cc-echo/`. There are no migrations — on a core schema
change, wipe the directory (`rm -rf ~/.cc-echo`).
