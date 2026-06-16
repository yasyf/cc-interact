#!/usr/bin/env bash
# OPTIONAL (domain) — UserPromptSubmit: open a turn (e.g. capture a pre-edit
# working-tree snapshot). Requires your binary to implement a `turn-start`
# handler; drop this hook (and its UserPromptSubmit entry in hooks.json) if
# your domain has no per-turn state. Always exits 0 and prints nothing —
# UserPromptSubmit stdout is injected into Claude's context.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

[ -x "$BIN" ] || exit 0
exec "$BIN" turn-start
