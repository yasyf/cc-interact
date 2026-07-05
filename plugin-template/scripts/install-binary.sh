#!/usr/bin/env bash
# Download the prebuilt {{BINARY_NAME}} binary for this platform from the GitHub
# release matching the plugin version. The plugin payload is self-contained
# (no source ships), so the binary always comes from release assets.
# A stale release binary is replaced when the plugin version moves; local dev
# builds (anything not reporting vX.Y.Z) are left alone.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/{{BINARY_NAME}}"

VERSION="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$ROOT/.claude-plugin/plugin.json")"

if [ -x "$BIN" ]; then
  # Release builds print the bare goreleaser version ({{ .Version }}, no "v") —
  # keep that coupling or a stale release binary is never replaced.
  installed="$("$BIN" --version 2>/dev/null || true)"
  installed="${installed#v}"
  case "$installed" in
    "$VERSION") exit 0 ;;
    [0-9]*) ;;
    *) exit 0 ;;
  esac
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
esac
URL="https://github.com/{{RELEASE_REPO}}/releases/download/v${VERSION}/{{BINARY_NAME}}_${OS}_${ARCH}"

echo "{{BINARY_NAME}}: downloading ${URL}" >&2
mkdir -p "$ROOT/bin"
# Stage in bin/ (same filesystem) and rename: writing onto a running executable
# fails with ETXTBSY on Linux, and rename keeps the old inode alive for any
# daemon still executing it.
TMP="$(mktemp "$ROOT/bin/.{{BINARY_NAME}}.XXXXXX")"
trap 'rm -f "$TMP"' EXIT
# Bound connect + transfer so a stalled network fails fast and lands in the
# session-start log, rather than hanging the SessionStart hook (600s budget).
curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 -o "$TMP" "$URL"
chmod +x "$TMP"
mv -f "$TMP" "$BIN"
echo "{{BINARY_NAME}}: installed $BIN" >&2
