#!/usr/bin/env bash
# SubagentStop: drain pending directives, else consult the stop-gate (a block
# keeps the child running). Passes stdin hook JSON to `{{BINARY_NAME}} agent-stop`.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

[ -x "$BIN" ] || exit 0
exec "$BIN" agent-stop
