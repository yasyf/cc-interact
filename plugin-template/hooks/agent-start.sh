#!/usr/bin/env bash
# SubagentStart: register the child with the agent plane so directives can
# address its agent_id. Passes stdin hook JSON to `{{BINARY_NAME}} agent-start`.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

[ -x "$BIN" ] || exit 0
exec "$BIN" agent-start
