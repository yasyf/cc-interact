# echo — a headless cc-interact consumer

The smallest complete consumer of the cc-interact framework: a human-in-the-loop
echo loop with **no browser, no SPA, and no `sse.StaticHandler`**. It proves the
core carries zero domain concepts and needs no frontend — the same `/events`
plane a browser would read is consumed here by the agent's `watch` and MCP
`channel`, and by a raw `curl`.

Items and replies live **purely as events** — there are no domain tables, so
`daemon.Config.Migrate` is `nil`.

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
# 1. start the daemon (or let any command lazy-spawn it)
go run . daemon &          # writes ~/.cc-echo/{state.db,daemon.sock,http.json,daemon.log}

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

## State

Everything lives under `~/.cc-echo/`. There are no migrations — on a core schema
change, wipe the directory (`rm -rf ~/.cc-echo`).
