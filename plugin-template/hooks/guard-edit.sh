#!/usr/bin/env bash
# SUBSTRATE — keep this hook. PreToolUse(Edit|Write|NotebookEdit): deny edits
# while a session is open and awaiting the human's response. Exits 2 (the
# PreToolUse block signal) on deny, 0 on allow. Fails open if the binary or
# daemon is unavailable. The deny verdict + wording come from your domain's
# gate handler, not the framework.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

[ -x "$BIN" ] || exit 0
exec "$BIN" guard-edit
