#!/usr/bin/env bash
# SUBSTRATE — keep this hook. SessionStart: ensure the binary is installed and
# current, then record the session's facts (best-effort — does nothing if the
# daemon isn't up). Reads the hook JSON on stdin and passes it through to
# `{{BINARY_NAME}} session-record`.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

# Capture the installer's output to a log under the plugin root so a failed
# remote install is diagnosable after the fact; the hook itself stays non-fatal.
LOG="$ROOT/install-binary.log"
{
  echo "--- $(date -u '+%Y-%m-%dT%H:%M:%SZ') session-start install ---"
  bash "$ROOT/scripts/install-binary.sh"
  echo "install-binary.sh exit=$?"
} >"$LOG" 2>&1 || true

[ -x "$BIN" ] && exec "$BIN" session-record
exit 0
