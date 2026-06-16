#!/usr/bin/env bash
# OPTIONAL (domain) — Stop: close the open turn (e.g. capture a post-edit
# working-tree snapshot). Requires your binary to implement a `turn-end`
# handler; drop this hook (and its Stop entry in hooks.json) if your domain has
# no per-turn state. Always exits 0 and prints nothing.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

[ -x "$BIN" ] || exit 0
exec "$BIN" turn-end
