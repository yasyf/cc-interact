#!/bin/sh
# demo.sh — record the examples/echo round trip and render docs/assets/demo.png.
#
# Runs the real commands: build the echo example, start a subject, POST a human
# item over REST, reply through the agent's MCP channel tool, then read both
# events back off /events. The captured transcript is rendered with freeze.
#
# State is isolated under a short /tmp HOME (the daemon's unix socket path must
# stay under the kernel's sun_path limit) and wiped on exit.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
FREEZE="${FREEZE:-freeze}"
BAT="${BAT:-bat}"

# Colorize captured text with ANSI codes so freeze renders real syntax colors.
paint() { "$BAT" --plain --color=always --language "$1"; }
prompt() { printf '$ %s\n' "$(printf '%s\n' "$1" | paint bash)"; }
TMP="$(mktemp -d /tmp/cc-echo-demo.XXXXXX)"
trap 'HOME="$TMP/home" "$TMP/echo" stop >/dev/null 2>&1 || true; rm -rf "$TMP"' EXIT
mkdir -p "$TMP/home"

cd "$ROOT"
go build -o "$TMP/echo" ./examples/echo

run() { HOME="$TMP/home" "$TMP/echo" "$@"; }

START_OUT="$(run start --cwd "$ROOT")"
SLUG="$(printf '%s\n' "$START_OUT" | awk '/^slug:/{print $2}')"
PORT="$(printf '%s\n' "$START_OUT" | awk -F: '/^http:/{print $NF}')"

POST_CMD="curl -s localhost:$PORT/api/items -d '{\"subject\":\"$SLUG\",\"text\":\"hi\"}'"
POST_OUT="$(curl -s "localhost:$PORT/api/items" -d "{\"subject\":\"$SLUG\",\"text\":\"hi\"}")"

cat >"$TMP/reply.jsonl" <<EOF
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reply","arguments":{"subject":"$SLUG","text":"echoed: hi"}}}
EOF
CHANNEL_OUT="$(run channel --cwd "$ROOT" <"$TMP/reply.jsonl" | tail -1)"

EVENTS_OUT="$(curl -sN --max-time 2 "localhost:$PORT/events?session=$SLUG" | grep -v '^: ' | cat -s || true)"

{
  prompt 'go build -o echo ./examples/echo && ./echo start'
  printf '%s\n' "$START_OUT" | paint yaml
  echo
  prompt "$POST_CMD"
  printf '%s\n' "$POST_OUT" | paint json
  echo
  prompt "./echo channel < reply.jsonl | tail -1   # the agent's MCP reply tool"
  printf '%s\n' "$CHANNEL_OUT" | paint json
  echo
  prompt "curl -sN --max-time 2 \"localhost:$PORT/events?session=$SLUG\""
  printf '%s\n' "$EVENTS_OUT" | paint yaml
} >"$TMP/transcript.ansi"

"$FREEZE" "$TMP/transcript.ansi" --language ansi \
  --theme github-dark --background "#0d1117" --window --padding 24 --font.size 28 \
  --output "$ROOT/docs/assets/demo.png"
