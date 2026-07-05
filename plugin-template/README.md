# plugin-template

A parameterized copy of [cc-review](https://github.com/yasyf/cc-review)'s plugin payload, de-domained so any consumer of the [cc-interact](https://github.com/yasyf/cc-interact) framework can ship its own human-in-the-loop agent UI as a Claude Code plugin.

It carries the substrate every cc-interact plugin needs — the channel MCP server wiring, the session-record + edit-guard hooks, the start skill, and an example background agent — with all the domain-specific (review) strings replaced by `{{VAR}}` placeholders. Fill the vars, render, then add your domain logic.

The binary installer is the one piece this template consumes rather than owns: `render.sh` grabs the canonical `install-binary.sh` template from [cc-skills](https://github.com/yasyf/cc-skills)' repo-bootstrap skill at render time and renders it into `scripts/`. Never edit a rendered copy by hand — fix the canonical template upstream, then run `render.sh --sync-scripts` to roll the fix into each consumer.

## What's here

```
plugin-template/
├── .claude-plugin/plugin.json   # manifest: name, mcpServers (channel) key, channels
├── scripts/mcp-channel.sh       # MCP stdio entrypoint → `<binary> <mcp-subcommand>`
├── hooks/
│   ├── hooks.json               # wires all four hooks below
│   ├── session-start.sh         # SUBSTRATE — install/refresh binary + record session
│   ├── guard-edit.sh            # SUBSTRATE — block edits until the human responds
│   ├── turn-start.sh            # OPTIONAL — open a turn (needs a turn-start handler)
│   └── turn-end.sh              # OPTIONAL — close a turn (needs a turn-end handler)
├── skills/start/SKILL.md        # the /<plugin>:start skill — generic session skeleton
├── agents/example-agent.md      # PLACEHOLDER background agent + MCP tool-name pattern
├── render.sh                    # substitute {{VAR}}s into an output dir
└── README.md                    # this file
```

Rendered output additionally gains `scripts/install-binary.sh`, fetched from the canonical repo-bootstrap template (see [render.sh](#rendersh)). `bin/` is not shipped — the installer provisions it on first use, and `bin/<binary>` is only ever a symlink: to a brew-installed binary, to a durable payload under the plugin data dir, or to a local dev build it refuses to clobber. On freshness the installer compares v-stripped: a release binary prints the bare tag (`0.5.0`) or the v-tag (`v0.5.0`) depending on how its ldflags were stamped, and the canonical script accepts both.

## Template variables

| var | meaning | example |
| --- | --- | --- |
| `{{PLUGIN_NAME}}` | plugin manifest `name`; also the plugin segment of MCP tool names and the channel id | `cc-review` |
| `{{DISPLAY_NAME}}` | human-facing label (manifest description, channel `displayName`, skill prose) | `cc-review` |
| `{{BINARY_NAME}}` | the installed binary's file name and how the scripts/hooks invoke it | `cc-review` |
| `{{RELEASE_REPO}}` | GitHub `owner/repo` for release-asset downloads and `homepage` | `yasyf/cc-review` |
| `{{BREW_PACKAGE}}` | Homebrew formula or cask the installer prefers over a static download | `yasyf/tap/cc-review` |
| `{{MCP_SUBCOMMAND}}` | the binary subcommand `mcp-channel.sh` execs to run the channel server | `mcp-channel` |
| `{{SKILL_NAME}}` | the start skill's invocation id (`<plugin>:start`) | `cc-review:start` |
| `{{MCP_SERVER_NAME}}` | the `mcpServers` key in `plugin.json` and the server segment of MCP tool names; usually equals `{{PLUGIN_NAME}}` | `cc-review` |

### Where each is used

- `{{PLUGIN_NAME}}` — `plugin.json` `name`; `agents/example-agent.md` (`subagent_type`, the `mcp__plugin_<PLUGIN_NAME>_…` tool prefix); `SKILL.md` (`--channels plugin:<PLUGIN_NAME>@<PLUGIN_NAME>`); the installer's default data-dir path.
- `{{DISPLAY_NAME}}` — `plugin.json` `description` + channel `displayName`; `SKILL.md` (description + prose); `agents/example-agent.md`.
- `{{BINARY_NAME}}` — the fetched installer (symlink path, asset name, log prefix), `scripts/mcp-channel.sh`, all four `hooks/*.sh` (bin path + `<binary> <subcommand>`), `SKILL.md` (every `${CLAUDE_PLUGIN_ROOT}/bin/<BINARY_NAME>` invocation).
- `{{RELEASE_REPO}}` — the fetched installer's download URLs; `plugin.json` `homepage`.
- `{{BREW_PACKAGE}}` — the fetched installer's `brew install`/`brew upgrade` target.
- `{{MCP_SUBCOMMAND}}` — `scripts/mcp-channel.sh` (`exec "$BIN" <MCP_SUBCOMMAND>`); `agents/example-agent.md` (prose).
- `{{SKILL_NAME}}` — `SKILL.md` (heading + "later rounds" prose); `agents/example-agent.md` (which skill dispatches it).
- `{{MCP_SERVER_NAME}}` — `plugin.json` (`mcpServers` key + channel `server`); `SKILL.md` (`<channel source="…">` tag); `agents/example-agent.md` (the server segment of the tool prefix).

## How to fill

1. **Pick your values** and render the tree (see `render.sh` below). The substrate (`session-start.sh`, `guard-edit.sh`, the channel wiring) works as-is once the vars are set.
2. **Edit the plain metadata** the template left as placeholders, not vars: `plugin.json` `version` (start at `0.1.0`; in the default pinned mode the installer resolves the release tag `v<version>` from it), `author`, and `license`.
3. **Implement the substrate subcommands** in your binary (provided by cc-interact's CLI layer): `session-record`, `guard-edit`, `watch`, `start`, `feedback`, `reply`, `channel-ack`, `setup-channels`, and the channel server (`{{MCP_SUBCOMMAND}}`). The hook scripts and skill call these by name.
4. **Decide on turn hooks.** `turn-start.sh`/`turn-end.sh` are optional — keep them only if your binary implements `turn-start`/`turn-end` handlers; otherwise delete the two scripts and their `UserPromptSubmit`/`Stop` entries in `hooks.json`.
5. **Fill the domain markers** in `SKILL.md` (the `<domain ...>` placeholders: your event types, reply kinds, and any background agent) and replace `agents/example-agent.md` with your real agent (or delete it if you dispatch none).
6. **Add reference docs** under `skills/start/reference/` if you want them — they are domain-specific (event schema, CLI cheatsheet, channel notes) and intentionally not templated here.

## render.sh

POSIX `sed`-based token replacement. Reads `VAR=value` pairs from the command line and/or the environment, copies the template tree (minus `render.sh` and `README.md`) into an output dir, and substitutes every `{{VAR}}`.

```bash
# command-line pairs
./render.sh ../my-plugin \
  PLUGIN_NAME=cc-review DISPLAY_NAME=cc-review BINARY_NAME=cc-review \
  RELEASE_REPO=yasyf/cc-review BREW_PACKAGE=yasyf/tap/cc-review \
  MCP_SUBCOMMAND=mcp-channel SKILL_NAME=cc-review:start

# or from the environment
PLUGIN_NAME=cc-review DISPLAY_NAME=cc-review BINARY_NAME=cc-review \
RELEASE_REPO=yasyf/cc-review BREW_PACKAGE=yasyf/tap/cc-review \
MCP_SUBCOMMAND=mcp-channel SKILL_NAME=cc-review:start \
  ./render.sh ../my-plugin
```

`MCP_SERVER_NAME` defaults to `PLUGIN_NAME` when unset. `BINARY_VERSION_MODE` picks how the installer resolves its target release: `pinned` (the default — the release tag comes from `plugin.json` `version`) or `latest` (the `releases/latest` redirect, for plugins whose version isn't coupled to binary releases). Values must not contain a `|` character (the sed delimiter). Executable bits on the rendered `*.sh` are preserved. The output dir must not already exist.

### The canonical installer grab

`render.sh` resolves the canonical `install-binary.sh` template through a tiered chain — first hit wins:

1. **Local sibling checkout** — `ccx repo locate cc-skills` when `ccx` is on PATH, else `~/Code/cc-skills`.
2. **The live marketplace clone** — `~/.claude/plugins/marketplaces/skills/…`.
3. **Anonymous raw fetch** — `raw.githubusercontent.com/yasyf/cc-skills` pinned to the `CC_SKILLS_REF` commit recorded in `render.sh`, never a floating branch.

Tier 1 and 2 hits are validated (the file must exist and still carry the `{{BINARY_NAME}}` token), so a checkout predating the template falls through instead of serving garbage. All tiers failing is a hard error; there is deliberately no vendored copy in this tree to drift from.

Every rendered copy carries a provenance stamp on line 2 — `# canonical: cc-skills/plugins/repo-bootstrap@<sha>` — naming the exact cc-skills commit it was rendered from. Fleet drift is one grep away:

```bash
grep -r "canonical: cc-skills/plugins/repo-bootstrap@" */scripts/install-binary.sh
```

### --sync-scripts: roll a fix out to an existing plugin

```bash
./render.sh --sync-scripts ../my-plugin
```

Re-fetches the canonical template and re-renders only `scripts/install-binary.sh` into an existing plugin dir, leaving everything else untouched. Token values come from the plugin itself — `plugin.json` `name` plus the rendered copy's own `NAME=`/`REPO=`/`BREW_PKG=` lines and version mode — so the command normally takes no arguments. The run is idempotent: syncing an already-current plugin rewrites the same bytes. A plugin whose installer predates the canonical format can't name its own tokens; pass them once (`BINARY_NAME=… RELEASE_REPO=… BREW_PACKAGE=…`) and later syncs are argument-free.
