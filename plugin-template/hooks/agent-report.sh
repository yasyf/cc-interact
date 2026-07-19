#!/usr/bin/env bash
# PostToolUse (Task|Agent): record the parent's subagent tool observation.
# Passes stdin hook JSON to `{{BINARY_NAME}} agent-report`.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

[ -x "$BIN" ] || exit 0
exec "$BIN" agent-report
