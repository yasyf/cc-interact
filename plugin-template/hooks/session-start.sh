#!/usr/bin/env bash
# SUBSTRATE — keep this hook. SessionStart: ensure the binary is installed and
# current, then record the session's facts (best-effort — does nothing if the
# daemon isn't up). Reads the hook JSON on stdin and passes it through to
# `{{BINARY_NAME}} session-record`.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

bash "$ROOT/scripts/install-binary.sh" >/dev/null 2>&1 || true
[ -x "$BIN" ] && exec "$BIN" session-record
exit 0
