#!/usr/bin/env bash
# MCP entrypoint for the opt-in channel server: install or refresh the binary,
# then exec it. stdout is the MCP stdio transport, so download output goes to
# stderr. A failed refresh falls back to the installed binary; with none
# installed the exec fails loudly.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

bash "$ROOT/scripts/install-binary.sh" 1>&2 || true
exec "$BIN" {{MCP_SUBCOMMAND}}
