#!/bin/sh
# render.sh — render plugin-template/ into OUT_DIR, substituting {{VAR}} tokens.
# POSIX sed-based. Values come from VAR=value arguments and/or the environment;
# arguments override the environment. MCP_SERVER_NAME defaults to PLUGIN_NAME.
set -eu

usage() {
  cat >&2 <<'EOF'
usage: render.sh OUT_DIR [VAR=value ...]
  Required (command line or environment):
    PLUGIN_NAME DISPLAY_NAME BINARY_NAME RELEASE_REPO
    MCP_SUBCOMMAND SKILL_NAME
  Optional:
    MCP_SERVER_NAME       (defaults to PLUGIN_NAME)
EOF
  exit 2
}

[ $# -ge 1 ] || usage

OUT_DIR="$1"
shift

for arg in "$@"; do
  case "$arg" in
    *=*) export "${arg%%=*}=${arg#*=}" ;;
    *) echo "render.sh: bad argument: $arg" >&2; usage ;;
  esac
done

# check_vars — require each named var non-empty and make its value sed-safe:
# '|' (the delimiter) is rejected; '&' and '\' are escaped in place so they
# substitute literally, matching render.py's plain-string replacement.
check_vars() {
  missing=
  for v in "$@"; do
    eval "val=\${$v:-}"
    [ -n "$val" ] || missing="$missing $v"
    case "$val" in *'|'*) echo "render.sh: value of $v contains '|' (the sed delimiter)" >&2; exit 1 ;; esac
    case "$val" in *'"'*) echo "render.sh: value of $v contains '\"' (breaks the rendered plugin.json)" >&2; exit 1 ;; esac
    # shellcheck disable=SC2034  # esc is consumed via the eval below
    esc="$(printf '%s\n' "$val" | sed 's/[&\\]/\\&/g')"
    eval "$v=\$esc"
  done
  [ -z "$missing" ] || { echo "render.sh: missing vars:$missing" >&2; return 1; }
}

: "${MCP_SERVER_NAME:=${PLUGIN_NAME:-}}"
export MCP_SERVER_NAME

TREE_VARS="PLUGIN_NAME DISPLAY_NAME BINARY_NAME RELEASE_REPO MCP_SUBCOMMAND SKILL_NAME MCP_SERVER_NAME"

# shellcheck disable=SC2086
check_vars $TREE_VARS || usage

SRC="$(cd "$(dirname "$0")" && pwd)"
[ -e "$OUT_DIR" ] && { echo "render.sh: $OUT_DIR already exists" >&2; exit 1; }
mkdir -p "$OUT_DIR"

# Copy the template tree (minus render.sh and README.md), preserving modes.
( cd "$SRC" && find . -type f ! -name render.sh ! -name README.md -print ) \
  | while IFS= read -r rel; do
      rel=${rel#./}
      dst="$OUT_DIR/$rel"
      mkdir -p "$(dirname "$dst")"
      cp -p "$SRC/$rel" "$dst"
    done

# Substitute in place. `cat tmp > f` truncates f and keeps its existing mode,
# so executable bits copied above survive.
find "$OUT_DIR" -type f -print | while IFS= read -r f; do
  sed \
    -e "s|{{PLUGIN_NAME}}|$PLUGIN_NAME|g" \
    -e "s|{{DISPLAY_NAME}}|$DISPLAY_NAME|g" \
    -e "s|{{BINARY_NAME}}|$BINARY_NAME|g" \
    -e "s|{{RELEASE_REPO}}|$RELEASE_REPO|g" \
    -e "s|{{MCP_SUBCOMMAND}}|$MCP_SUBCOMMAND|g" \
    -e "s|{{SKILL_NAME}}|$SKILL_NAME|g" \
    -e "s|{{MCP_SERVER_NAME}}|$MCP_SERVER_NAME|g" \
    "$f" > "$f.cci.tmp"
  cat "$f.cci.tmp" > "$f"
  rm -f "$f.cci.tmp"
done

echo "render.sh: rendered -> $OUT_DIR" >&2
