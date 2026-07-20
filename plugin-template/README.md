# plugin-template

A parameterized copy of [cc-review](https://github.com/yasyf/cc-review)'s plugin payload, de-domained so any consumer of the [cc-interact](https://github.com/yasyf/cc-interact) framework can ship its own human-in-the-loop agent UI as a Claude Code plugin.

It carries the substrate every cc-interact plugin needs — the channel MCP server wiring, a capt-hook hook pack (session record, edit guard, turn lifecycle, and the agent steering plane), the start skill, and an example background agent — with all the domain-specific (review) strings replaced by `{{VAR}}` placeholders. Fill the vars, render, then add your domain logic.

The binary installer is the one piece this template consumes rather than owns: each rendered plugin declares `scripts/install-binary.sh` as a cc-guides fragment layout, and `cc-guides render` writes it from the canonical template in [cc-skills](https://github.com/yasyf/cc-skills) (see [The binary installer](#the-binary-installer)). Never edit the rendered copy by hand — the daily CI re-render reverts it; fix the canonical template upstream.

## What's here

```
plugin-template/
├── .claude-plugin/plugin.json   # manifest: name, mcpServers (channel) key, channels, captain-hook dependency
├── scripts/mcp-channel.sh       # MCP stdio entrypoint → `<binary> <mcp-subcommand>`
├── capt-hook/                   # capt-hook v11 pack — discovered from the enabled plugin; no hooks.json
│   ├── pack.toml                # pack descriptor (empty — identity derives from plugin.json)
│   └── hooks/
│       ├── common.py            # plugin root + bin path + fail-open call_bin forwarding
│       ├── session.py           # SUBSTRATE — install_binary provisioning + session-record
│       ├── guard_edit.py        # SUBSTRATE — block edits until the human responds
│       ├── agent_plane.py       # OPTIONAL — parent↔child steering, 4 hooks (delete if unused)
│       ├── turn_hooks.py        # OPTIONAL — turn-start/turn-end (need binary handlers)
│       └── tests/test_pack.py   # standalone stub-binary contract tests (uv run)
├── skills/start/SKILL.md        # the /<plugin>:start skill — generic session skeleton
├── agents/example-agent.md      # PLACEHOLDER background agent + MCP tool-name pattern
├── render.sh                    # substitute {{VAR}}s into an output dir
└── README.md                    # this file
```

`scripts/install-binary.sh` arrives separately, via cc-guides (see [The binary installer](#the-binary-installer)); the template's hooks deliberately exec that path. `bin/` is not shipped — the installer provisions it on first use, and `bin/<binary>` is only ever a symlink: to a brew-installed binary, to a durable payload under the plugin data dir, or to a local dev build it refuses to clobber. On freshness the installer compares v-stripped: a release binary prints the bare tag (`0.5.0`) or the v-tag (`v0.5.0`) depending on how its ldflags were stamped, and the canonical script accepts both.

## Template variables

| var | meaning | example |
| --- | --- | --- |
| `{{PLUGIN_NAME}}` | plugin manifest `name`; also the plugin segment of MCP tool names and the channel id | `cc-review` |
| `{{DISPLAY_NAME}}` | human-facing label (manifest description, channel `displayName`, skill prose) | `cc-review` |
| `{{BINARY_NAME}}` | the installed binary's file name and how the scripts/hooks invoke it | `cc-review` |
| `{{RELEASE_REPO}}` | GitHub `owner/repo`, the manifest's `homepage` | `yasyf/cc-review` |
| `{{MCP_SUBCOMMAND}}` | the binary subcommand `mcp-channel.sh` execs to run the channel server | `mcp-channel` |
| `{{SKILL_NAME}}` | the start skill's invocation id (`<plugin>:start`) | `cc-review:start` |
| `{{MCP_SERVER_NAME}}` | the `mcpServers` key in `plugin.json` and the server segment of MCP tool names; usually equals `{{PLUGIN_NAME}}` | `cc-review` |

### Where each is used

- `{{PLUGIN_NAME}}` — `plugin.json` `name` (the pack's identity derives from it — the v11 descriptor carries no name); `agents/example-agent.md` (`subagent_type`, the `mcp__plugin_<PLUGIN_NAME>_…` tool prefix); `SKILL.md` (`--channels plugin:<PLUGIN_NAME>@<PLUGIN_NAME>`); the installer's default data-dir path.
- `{{DISPLAY_NAME}}` — `plugin.json` `description` + channel `displayName`; `SKILL.md` (description + prose); `agents/example-agent.md`.
- `{{BINARY_NAME}}` — `scripts/mcp-channel.sh`, `capt-hook/hooks/common.py` (the `bin/` path every pack hook calls), `SKILL.md` (every `${CLAUDE_PLUGIN_ROOT}/bin/<BINARY_NAME>` invocation).
- `{{RELEASE_REPO}}` — `plugin.json` `homepage`.
- `{{MCP_SUBCOMMAND}}` — `scripts/mcp-channel.sh` (`exec "$BIN" <MCP_SUBCOMMAND>`); `agents/example-agent.md` (prose).
- `{{SKILL_NAME}}` — `SKILL.md` (heading + "later rounds" prose); `agents/example-agent.md` (which skill dispatches it).
- `{{MCP_SERVER_NAME}}` — `plugin.json` (`mcpServers` key + channel `server`); `SKILL.md` (`<channel source="…">` tag); `agents/example-agent.md` (the server segment of the tool prefix).

## How to fill

1. **Pick your values** and render the tree (see `render.sh` below). The substrate (`capt-hook/hooks/session.py`, `capt-hook/hooks/guard_edit.py`, the channel wiring) works as-is once the vars are set.
2. **Edit the plain metadata** the template left as placeholders, not vars: `plugin.json` `version` (start at `0.1.0`; the pinned installer resolves the release tag `v<version>` from it), `author`, and `license`.
3. **Wire the subcommands.** cc-interact's `cmd` layer provides `session-record`, `guard-edit`, `watch`, `channel-ack`, `setup-channels` (`cmd.SetupChannelsCmd`), and the channel server (`cmd.ChannelCmd` — set its `Use` to `{{MCP_SUBCOMMAND}}` when that isn't `channel`, keeping `channel` as an alias); implement the domain commands — `start`, `feedback`, `reply` — in your binary. The pack hooks and skill call all of these by name.
4. **Decide on the optional hooks.** `capt-hook/hooks/turn_hooks.py` needs `turn-start`/`turn-end` handlers in your binary, and `capt-hook/hooks/agent_plane.py` needs the agent plane (see `SKILL.md` §6); each is one file — keep it or delete it.
5. **Fill the domain markers** in `SKILL.md` (the `<domain ...>` placeholders: your event types, reply kinds, and any background agent) and replace `agents/example-agent.md` with your real agent (or delete it if you dispatch none).
6. **Add reference docs** under `skills/start/reference/` if you want them — they are domain-specific (event schema, CLI cheatsheet, channel notes) and intentionally not templated here.

## The capt-hook pack

The hooks ship as a [capt-hook](https://github.com/yasyf/captain-hook) pack, not raw hook
scripts. The fixed `capt-hook/` layout — `pack.toml` plus a `hooks/` dir — marks the
plugin as shipping a pack, and the captain-hook dispatcher discovers it from the enabled
plugin on every event; the plugin wires no `hooks.json` at all. Session-start
provisioning, the last job that needed one, runs through the `install_binary` primitive in
`capt-hook/hooks/session.py`.

`capt-hook/pack.toml` is the v11 pack descriptor. It carries no name, version, or
description — those derive from `plugin.json` — so it stays empty unless a hook needs
`resources` (NLP models to provision) or a `[tools.*]` table (MCP-tool gate semantics).

Three things make the contract, and `capt-hook pack test` enforces the first two:

1. The `capt-hook/{pack.toml, hooks/}` layout under the plugin root.
2. The `plugin.json` dependency:
   `{ "name": "captain-hook", "marketplace": "captain-hook", "version": ">=11.0.0" }`.
3. Your marketplace's `marketplace.json` allowlist:
   `"allowCrossMarketplaceDependenciesOn": ["captain-hook"]`. This line is load-bearing —
   without it Claude Code skips the cross-marketplace dependency at install with no error,
   and every hook stays dark.

On a machine that has never seen captain-hook, installing your plugin lands it disabled
with `dependency-unsatisfied`. The one-time fix belongs in your plugin's README:

```bash
claude plugin marketplace add yasyf/captain-hook
```

Every hook fails open. A missing binary, a downed daemon, garbage output, or a timeout is
an allow, never an error surfaced to the agent; `uvx capt-hook logs` is the diagnosis
surface, and the installer's output lands there too (replacing the old
`install-binary.log`). Validate and test a rendered plugin directly:

```bash
uvx --isolated capt-hook pack test .        # layout + dependency floor + inline tests
uv run capt-hook/hooks/tests/test_pack.py   # stub-binary contract tests
```

`pack test` validates the layout and the dependency floor, then runs the inline `tests={}`
shapes inside the hook files. Those same shapes run under `uvx capt-hook test` in every
repo the plugin is enabled in, which is why they are non-firing shapes only — the firing
coverage, against a stub binary, lives in `capt-hook/hooks/tests/test_pack.py`.

## render.sh

POSIX `sed`-based token replacement. Reads `VAR=value` pairs from the command line and/or the environment, copies the template tree (minus `render.sh` and `README.md`) into an output dir, and substitutes every `{{VAR}}`.

```bash
# command-line pairs
./render.sh ../my-plugin \
  PLUGIN_NAME=cc-review DISPLAY_NAME=cc-review BINARY_NAME=cc-review \
  RELEASE_REPO=yasyf/cc-review \
  MCP_SUBCOMMAND=mcp-channel SKILL_NAME=cc-review:start

# or from the environment
PLUGIN_NAME=cc-review DISPLAY_NAME=cc-review BINARY_NAME=cc-review \
RELEASE_REPO=yasyf/cc-review \
MCP_SUBCOMMAND=mcp-channel SKILL_NAME=cc-review:start \
  ./render.sh ../my-plugin
```

`MCP_SERVER_NAME` defaults to `PLUGIN_NAME` when unset. Values must not contain a `|` character (the sed delimiter) or a `"` (it would corrupt the rendered `plugin.json`). Executable bits on the rendered `*.sh` are preserved. The output dir must not already exist.

## The binary installer

`render.sh` does not produce `scripts/install-binary.sh`. Each plugin declares it as a [cc-guides](https://github.com/yasyf/cc-guides) fragment layout — `.claude/fragments/plugin/scripts/install-binary.sh/layout.toml` in the plugin's repo — and `cc-guides render` writes the script from the canonical template in cc-skills. cc-review's layout, verbatim:

```toml
fragments = [
  { use = "cc-skills:install-binary-pinned", args = { binary = "cc-review", brew = "yasyf/tap/cc-review", plugin = "cc-review", repo = "yasyf/cc-review" } },
]

[sources.cc-skills]
source = "github:yasyf/cc-skills@main"
```

`install-binary-pinned` resolves the release tag `v<version>` from `plugin.json`; `install-binary-latest` follows the `releases/latest` redirect instead, for plugins whose version isn't coupled to binary releases. A new plugin commits this layout in its own repo and runs `cc-guides render` once to produce the script. A daily CI render keeps every consumer's copy current — the canonical script is never forked, and a hand-edit to a rendered copy reverts on the next render.
