#!/usr/bin/env bash
# PreToolUse (no matcher): drain the child's pending directives and inject them
# as context. Passes stdin hook JSON to `{{BINARY_NAME}} agent-inject`.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

[ -x "$BIN" ] || exit 0
exec "$BIN" agent-inject
