# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.6] - 2026-07-01

### Fixed
- `subject`: `Resolver.Rebind` (run on every session start) no longer adopts a
  subject owned by another window. It fell through to `FindAdoptableByScope` and
  could rebind a second live window's open review onto the rotating session when
  the owner was momentarily unheld — e.g. mid `--resume`, its pid already dead but
  its channel only just dropped — so two windows sharing a scope resolved the same
  subject and the human's review input routed to the wrong session. Rebind now only
  rebinds the window's own subject (rotated session id, else pid); adopting a dead
  window's orphan stays the job of an explicit `Start`.
- `daemon`: `Policy.Held` now treats a subject whose channel dropped within a new
  `heldGrace` (45s) as still held, closing the `--resume` gap where an explicit
  `Start` could otherwise steal a merely-restarting window's review. `attachGrace`
  (10s) is retained for status/`ConsumerConnected` reporting.

## [0.1.5] - 2026-07-01

### Removed
- `channel`: the eager `channel.hello` push at attach. Every delivered channel tag
  wakes the agent, so an unsolicited handshake burned a thinking turn in each idle
  session that attached to an existing subject. The channel is now silent until the
  subject's first real event — or until the daemon solicits a frame via `Inject`.

### Added
- `sse`: `(*Server).Inject` and the daemon passthrough `(*daemon.Server).InjectEvent` —
  write a one-shot, non-persisted frame to exactly one window's named consumer stream
  (keyed subject + consumer + pid). The frame carries no SSE id, so the consumer's
  cursor never advances and a reconnect can never replay it. Built for solicited
  delivery probes: a consumer's start op can prove the channel round trip on demand
  instead of pushing an unsolicited hello at attach.

## [0.1.4] - 2026-06-17

### Added
- `channel`: `ServerInfo.Instructions` is returned as the MCP `initialize` result's top-level `instructions`, so a consumer gives the agent always-present guidance (e.g. that `channel.hello` is a silent handshake). `Deps.ChannelTools` now returns this instructions string alongside the tools and notify method.

### Changed
- `channel`: the eager `channel.hello` tag carries a `note` ("system handshake; no reply needed") so an agent reading the raw payload does not mistake the attach handshake for a user request.

## [0.1.0] - 2026-06-16

### Added
- Initial release of the domain-agnostic agent ⟷ daemon ⟷ web framework, extracted from cc-review.
- Go core: `paths`, `event`, `subject` (rotation/adopt resolver), `store` (SQLite + subject CAS + gap-free event log), `consume`, `sse` (required `/events` plane + opt-in `StaticHandler`), `daemon` (generic envelope/registry, core ops, lazy lifecycle + eviction, edit-gate, presence), `channel` (MCP stdio server + `StreamEvents` + connectivity), `cmd` (reusable cobra), `version`.
- Optional `vcs` module: git/jj snapshot + turn capture.
- Opt-in `@cc-interact/react` npm package (Vite library mode): `createEventStream`, query primitives, app shell, theme/layout base CSS.
- `plugin-template/` scaffold and a headless `examples/echo` consumer.

[Unreleased]: https://github.com/yasyf/cc-interact/compare/v0.1.6...HEAD
[0.1.6]: https://github.com/yasyf/cc-interact/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/yasyf/cc-interact/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/yasyf/cc-interact/compare/v0.1.3...v0.1.4
[0.1.0]: https://github.com/yasyf/cc-interact/releases/tag/v0.1.0
