#!/bin/sh
# render.sh — render plugin-template/ into OUT_DIR, substituting {{VAR}} tokens.
# POSIX sed-based. Values come from VAR=value arguments and/or the environment;
# arguments override the environment. MCP_SERVER_NAME defaults to PLUGIN_NAME.
#
# scripts/install-binary.sh is not part of this tree: it renders from the
# canonical template owned by cc-skills/plugins/repo-bootstrap, fetched at
# render time through the tier chain in fetch_canonical below. Rendered copies
# carry a "# canonical: …@<sha>" provenance stamp; re-render one in place with
# `render.sh --sync-scripts <plugin-dir>`.
set -eu

# bump to the cc-skills merge sha when PR #2 merges; NEVER a floating branch.
CC_SKILLS_REF=7f143612aa5bb563f534aae36f6848d0ff6d2975
CANONICAL=plugins/repo-bootstrap/skills/repo-bootstrap/templates/plugin/install-binary.sh

usage() {
  cat >&2 <<'EOF'
usage: render.sh OUT_DIR [VAR=value ...]
       render.sh --sync-scripts PLUGIN_DIR [VAR=value ...]
  Required (command line or environment):
    PLUGIN_NAME DISPLAY_NAME BINARY_NAME RELEASE_REPO BREW_PACKAGE
    MCP_SUBCOMMAND SKILL_NAME
  Optional:
    MCP_SERVER_NAME       (defaults to PLUGIN_NAME)
    BINARY_VERSION_MODE   (pinned or latest; defaults to pinned)
  --sync-scripts re-renders only scripts/install-binary.sh into an existing
  plugin dir, reading token values from its rendered copy and plugin.json;
  VAR=value arguments fill anything a pre-canonical copy cannot provide.
EOF
  exit 2
}

[ $# -ge 1 ] || usage

MODE=render
if [ "$1" = --sync-scripts ]; then
  MODE=sync
  shift
  [ $# -ge 1 ] || usage
fi
OUT_DIR="$1"
shift

for arg in "$@"; do
  case "$arg" in
    *=*) export "${arg%%=*}=${arg#*=}" ;;
    *) echo "render.sh: bad argument: $arg" >&2; usage ;;
  esac
done

TPL="$(mktemp)"
trap 'rm -f "$TPL" "$TPL.render"' EXIT

# A candidate template is usable when it exists and still carries the
# {{BINARY_NAME}} token — a checkout predating the template (or one holding a
# rendered copy) fails the check and falls through to the next tier.
usable() {
  [ -f "$1" ] && grep -q '{{BINARY_NAME}}' "$1"
}

# fetch_canonical — resolve the canonical installer template into $TPL and set
# CANONICAL_REF to the commit it came from. Strict tier order, first hit wins:
# local sibling checkout, the live marketplace clone, then an anonymous raw
# fetch pinned to CC_SKILLS_REF. All tiers failing is a hard error — there is
# deliberately no vendored fallback to drift from.
fetch_canonical() {
  dir=""
  if command -v ccx >/dev/null 2>&1; then
    dir="$(ccx repo locate cc-skills 2>/dev/null | awk -F'\t' 'NR == 1 {print $2}')" || dir=""
  fi
  [ -n "$dir" ] || dir="$HOME/Code/cc-skills"
  if usable "$dir/$CANONICAL" && CANONICAL_REF="$(git -C "$dir" rev-parse HEAD 2>/dev/null)"; then
    cat "$dir/$CANONICAL" >"$TPL"
    echo "render.sh: canonical installer from $dir @$CANONICAL_REF" >&2
    return 0
  fi

  mkt="$HOME/.claude/plugins/marketplaces/skills"
  if usable "$mkt/$CANONICAL" && CANONICAL_REF="$(git -C "$mkt" rev-parse HEAD 2>/dev/null)"; then
    cat "$mkt/$CANONICAL" >"$TPL"
    echo "render.sh: canonical installer from $mkt @$CANONICAL_REF" >&2
    return 0
  fi

  url="https://raw.githubusercontent.com/yasyf/cc-skills/$CC_SKILLS_REF/$CANONICAL"
  if curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 -o "$TPL" "$url" && usable "$TPL"; then
    CANONICAL_REF="$CC_SKILLS_REF"
    echo "render.sh: canonical installer from $url" >&2
    return 0
  fi

  echo "render.sh: could not resolve the canonical installer template; tried" >&2
  echo "  sibling checkout:  $dir/$CANONICAL" >&2
  echo "  marketplace clone: $mkt/$CANONICAL" >&2
  echo "  raw fetch:         $url" >&2
  exit 1
}

# render_installer DEST — resolve the whole-line {{#PINNED}}/{{#LATEST}}
# sections (exactly one mode survives; byte-compatible with repo-bootstrap's
# render.py for this template), substitute the installer tokens, and finalize
# the provenance stamp so fleet drift stays grep-able.
render_installer() {
  dest="$1"
  if [ "$BINARY_VERSION_MODE" = latest ]; then
    keep=LATEST drop=PINNED
  else
    keep=PINNED drop=LATEST
  fi
  sed \
    -e "/^[[:space:]]*{{#$keep}}[[:space:]]*\$/d" \
    -e "/^[[:space:]]*{{\/$keep}}[[:space:]]*\$/d" \
    -e "/^[[:space:]]*{{#$drop}}[[:space:]]*\$/,/^[[:space:]]*{{\/$drop}}[[:space:]]*\$/d" \
    -e "s|{{BINARY_NAME}}|$BINARY_NAME|g" \
    -e "s|{{RELEASE_REPO}}|$RELEASE_REPO|g" \
    -e "s|{{BREW_PACKAGE}}|$BREW_PACKAGE|g" \
    -e "s|{{PLUGIN_NAME}}|$PLUGIN_NAME|g" \
    -e "s|^# canonical: cc-skills/plugins/repo-bootstrap@pending\$|# canonical: cc-skills/plugins/repo-bootstrap@$CANONICAL_REF|" \
    "$TPL" >"$TPL.render"
  if grep -q '{{' "$TPL.render"; then
    echo "render.sh: unrendered tokens in the canonical installer:" >&2
    grep -n '{{' "$TPL.render" >&2
    exit 1
  fi
  # The @pending stamp carries no {{, so a missed substitution (an upstream
  # stamp-line change) evades the gate above — assert the stamp landed.
  if ! grep -q "^# canonical: cc-skills/plugins/repo-bootstrap@$CANONICAL_REF\$" "$TPL.render"; then
    echo "render.sh: provenance stamp missing — the canonical template's stamp line changed; update the stamp sed in render_installer" >&2
    exit 1
  fi
  mkdir -p "$(dirname "$dest")"
  cat "$TPL.render" >"$dest"
  chmod +x "$dest"
}

# check_vars — require each named var non-empty and make its value sed-safe:
# '|' (the delimiter) is rejected; '&' and '\' are escaped in place so they
# substitute literally, matching render.py's plain-string replacement.
check_vars() {
  missing=
  for v in "$@"; do
    eval "val=\${$v:-}"
    [ -n "$val" ] || missing="$missing $v"
    case "$val" in *'|'*) echo "render.sh: value of $v contains '|' (the sed delimiter)" >&2; exit 1 ;; esac
    # shellcheck disable=SC2034  # esc is consumed via the eval below
    esc="$(printf '%s\n' "$val" | sed 's/[&\\]/\\&/g')"
    eval "$v=\$esc"
  done
  [ -z "$missing" ] || { echo "render.sh: missing vars:$missing" >&2; return 1; }
}

case "${BINARY_VERSION_MODE:-pinned}" in
  pinned | latest) ;;
  *) echo "render.sh: BINARY_VERSION_MODE must be pinned or latest, got '$BINARY_VERSION_MODE'" >&2; exit 1 ;;
esac

if [ "$MODE" = sync ]; then
  MANIFEST="$OUT_DIR/.claude-plugin/plugin.json"
  [ -f "$MANIFEST" ] || { echo "render.sh: $OUT_DIR is not a plugin dir (no .claude-plugin/plugin.json)" >&2; exit 1; }
  SCRIPT="$OUT_DIR/scripts/install-binary.sh"
  # First "name" wins: the manifest's own comes before nested ones (author.name).
  [ -n "${PLUGIN_NAME:-}" ] || PLUGIN_NAME="$(sed -n 's/.*"name": *"\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)"
  if [ -f "$SCRIPT" ]; then
    # A canonical-format copy names its own tokens; latest mode is detected by
    # the releases/latest redirect only that mode's section contains.
    [ -n "${BINARY_NAME:-}" ] || BINARY_NAME="$(sed -n 's/^NAME="\(.*\)"$/\1/p' "$SCRIPT")"
    [ -n "${RELEASE_REPO:-}" ] || RELEASE_REPO="$(sed -n 's/^REPO="\(.*\)"$/\1/p' "$SCRIPT")"
    [ -n "${BREW_PACKAGE:-}" ] || BREW_PACKAGE="$(sed -n 's/^BREW_PKG="\(.*\)"$/\1/p' "$SCRIPT")"
    if [ -z "${BINARY_VERSION_MODE:-}" ] && grep -q 'releases/latest' "$SCRIPT"; then
      BINARY_VERSION_MODE=latest
    fi
  fi
  BINARY_VERSION_MODE="${BINARY_VERSION_MODE:-pinned}"
  check_vars BINARY_NAME RELEASE_REPO BREW_PACKAGE PLUGIN_NAME \
    || { echo "render.sh: --sync-scripts could not resolve them from $OUT_DIR; pass VAR=value" >&2; exit 1; }
  fetch_canonical
  render_installer "$SCRIPT"
  echo "render.sh: synced -> $SCRIPT (@$CANONICAL_REF)" >&2
  exit 0
fi

: "${MCP_SERVER_NAME:=${PLUGIN_NAME:-}}"
export MCP_SERVER_NAME
BINARY_VERSION_MODE="${BINARY_VERSION_MODE:-pinned}"

TREE_VARS="PLUGIN_NAME DISPLAY_NAME BINARY_NAME RELEASE_REPO MCP_SUBCOMMAND SKILL_NAME MCP_SERVER_NAME"

# shellcheck disable=SC2086
check_vars $TREE_VARS BREW_PACKAGE || usage

fetch_canonical

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

render_installer "$OUT_DIR/scripts/install-binary.sh"

echo "render.sh: rendered -> $OUT_DIR" >&2
