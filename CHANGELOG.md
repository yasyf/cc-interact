# Changelog

All notable changes to this project are documented here.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.0] - 2026-07-14

### Added
- `daemon.Config.ExtraHTTPListeners` — listener factories called once at HTTP
  start, each serving the same composed, auth-guarded handler as the primary
  bind (e.g. a TLS listener with certs from `tailscale cert`). A factory error
  fails startup, and one graceful shutdown drains every listener. The loopback
  token bypass stays per-connection (judged by peer address), so `New` refuses
  extra listeners with no `HTTPToken` (`ErrUnauthenticatedBind`).
- `daemon.Config.PublicHandler` — serves every request no mux route matches
  outside the auth guard, so a consumer's static SPA shell (index.html, assets,
  service worker) is fetchable before a remote browser holds the token. Routes
  mounted on the mux keep the full bearer/loopback semantics.
- `daemon.HTTPInfo.ExtraAddrs` — the handshake now records each extra
  listener's bound address, so a pairing CLI can verify a leg is actually
  served before advertising it.
- `daemon.Server.Background(fn)` — runs consumer fan-out on the daemon
  lifecycle: `fn` receives the serve context (cancelled at shutdown) and
  `Serve` drains it before closing the store.

### Fixed
- `vcs`: snapshot diffs now pass `--no-ext-diff`, so a `diff.external` tool in
  the user's git config (difftastic, delta) no longer replaces the parseable
  patch with its own output, which emptied the snapshot's file list.
- `daemon`: the `OnHTTPStart` hook now runs on the wait group, so `Serve`
  awaits its return after the serve context is cancelled. A hook that releases
  a resource on `ctx.Done()` — mDNS goodbye packets — no longer races the
  process exit, so a restart or shutdown withdraws the advertisement instead of
  leaving a stale record.

### Security
- The loopback auth bypass now also requires a loopback `Origin` (absent, an
  IP loopback, or `localhost`), closing a CSRF hole where a foreign page could
  send no-cors mutations to `127.0.0.1` through a local browser. An empty
  token no longer disables the check entirely — it fails closed to the
  loopback bypass alone.

## [0.6.0] - 2026-07-10

### Added
- `daemon.Config` gains three HTTP-plane knobs. `BindAddr` sets the address the
  listener binds — empty keeps the loopback-only `127.0.0.1` default, `0.0.0.0`
  exposes the plane to the LAN. `HTTPToken`, when set, requires every
  non-loopback request to carry `Authorization: Bearer <token>` (or the
  `?token=` query fallback that browser `EventSource` needs, since it cannot set
  headers); loopback always bypasses, and the token is compared in constant
  time. `OnHTTPStart(ctx, port)` fires once the plane is bound and its handshake
  published, so a consumer can hook mDNS advertising. `daemon.HTTPInfo` gains a
  `bind` field carrying the effective bind address.

## [0.5.0] - 2026-07-10

### Added
- `@cc-interact/react` (0.5.0): `ToastStack` — floating top-right toast column
  replacing the full-bleed `NotificationsBar` band. Auto-dismisses per kind
  (info 5s / warn 8s / error 10s), pauses on hover, shows the last four, uses
  `role=alert` for errors. `StreamToast.kind` is now the exported `ToastKind`
  union. `EventStreamValue` gains `caughtUp` (latched once the SSE replay
  flushes, so consumers can gate a loading skeleton) and `notify(toast)` (raise
  a toast from non-stream code, e.g. mutation failures).
  `OptimisticMutationConfig` gains `onError` so consumers can surface failed
  posts, and `scope` — mutations sharing a scope run serially in dispatch
  order, so the daemon's append order matches the user's action order.
  `base.css` gains the design-system tokens: modular type scale,
  `--leading-snug`, elevation ladder `--shadow-1/2/3` + `--shadow-up`, motion
  tokens `--dur-1/2/3` + `--ease-out`/`--ease-in-out`, `--surface-raised`,
  `--content-width`, a unified `:focus-visible` ring, and a global
  `prefers-reduced-motion` guard.

- `@cc-interact/react` (0.4.0): `CollapsedGroup` — a presentational collapsible
  group whose header button toggles a body that mounts only while expanded
  (collapsed content is unmounted, not hidden). It publishes a cooperative
  read-only signal through React context that interactive descendants read via
  the exported `useGroupReadOnly` hook (`false` outside any group), so a
  read-only group dims its body via `.cc-group-readonly` without the `inert`
  attribute — nested toggles inside an expanded read-only group keep working.
  Ships minimal `.cc-group` chrome in `base.css`.

### Changed
- `plugin-template/render.sh` renders `scripts/install-binary.sh` from the
  canonical template owned by cc-skills' repo-bootstrap skill, resolved through
  a tiered fetch (local sibling checkout, then the live marketplace clone,
  then a raw fetch pinned to `CC_SKILLS_REF`) and stamped with the source commit
  (`# canonical: cc-skills/plugins/repo-bootstrap@<sha>`). New required
  `BREW_PACKAGE` var and optional `BINARY_VERSION_MODE` (`pinned`/`latest`).
  A new `--sync-scripts <plugin-dir>` mode re-fetches the canonical template
  and re-renders only the installer into an existing plugin, reading token
  values from its rendered copy and `plugin.json`; the run is idempotent.

### Fixed
- `plugin-template/render.sh` substitutes values containing `&` or `\`
  literally; previously sed replacement specials silently corrupted rendered
  files.

### Removed
- `@cc-interact/react` (0.5.0): `NotificationsBar`, `NotificationsBarProps`,
  and `AppShellProps.notifications` — the band had no consumers; toasts render
  as an `AppShell` sibling via `ToastStack`.
- `plugin-template/scripts/install-binary.sh` — the template consumes the
  canonical installer instead of owning a copy. Rendered plugins now get the
  brew-first, sha256-verified, dev-build-safe installer whose `bin/<name>` is
  only ever a symlink.

## [0.1.9] - 2026-07-02

### Changed
- **Breaking:** `daemon.Config.ScopeResolve` is now
  `func(ctx context.Context, raw string) string` — no error. Scope resolution
  is canonicalization, not authorization: the resolver returns the raw value
  when there is no canonical form (cc-review: `vcs.Root`, falling back to the
  cwd as given outside a repo). A fallback scope matches no subject, so every
  core degradation falls out of resolution itself — guard-edit allows,
  session-record no-ops, status reports bare liveness. One behavior change:
  `resolve` outside any canonical scope now returns OK with no subject instead
  of erroring, so a stream consumer there waits rather than failing loudly.

### Removed
- `daemon.Server.RegisterScopeOptional` and per-op scope policy — introduced in
  0.1.8, superseded before any consumer shipped on it. With a resolver that
  cannot fail there is no policy dimension left; every op registers via
  `Register`. The 0.1.8 registry unification (core ops as ordinary
  registrations, `reserved` = health/shutdown) stays.

## [0.1.8] - 2026-07-02

### Added
- `daemon.Server.RegisterScopeOptional`: register a domain op that also serves
  requests from outside any resolvable scope — the handler sees `Scope == ""`.
  For ops that span scopes (listings, cross-scope repair), which previously had
  no way to run outside a repo: dispatch hard-errored on the failed
  `ScopeResolve` with no consumer hook.

### Changed
- `daemon`: scope policy is a per-op registration property, not dispatch
  hardcoding. The core subject ops are ordinary registrations made in `New` —
  `session-record`, `guard-edit`, `status`, and `channel-ack` scope-optional,
  `resolve` scope-required — and their unresolvable-scope degradation (guard-edit
  allows, session-record no-ops, status reports bare liveness) now falls out of
  each handler resolving no subject for an empty scope, replacing dispatch's
  hardcoded per-op switch. Behavior is unchanged; `reserved` shrinks to
  `health`/`shutdown` (the pre-protocol ops), and re-registering a core op
  panics as a duplicate.

## [0.1.7] - 2026-07-02

### Removed
- `subject`: scope-wide subject **adoption**. `Resolver.Start`/`Rebind` no longer
  fall back to `FindAdoptableByScope`, `Resolver.Peek` is gone, and the `Store`
  interface drops `FindAdoptableByScope`. A window now resolves only its **own**
  subject — by session id, then by window pid — and never takes over a review
  another window opened. This makes cross-session mis-routing impossible by
  construction, superseding the v0.1.6 interim whose `Held` grace only narrowed
  the race.
- `subject.Policy.Held` and `daemon.Config.WindowAlive` (with the server's `held`
  predicate and `heldGrace`): liveness no longer gates ownership, so they have no
  callers. `NewSubjectStore` drops its `activeStatuses` argument. `Config.ActiveStatuses`
  remains — it still feeds `Policy.Active`, which gates resuming a pid-latest
  subject across session rotation.

### Changed
- A resume that rotates *both* the session id and the pid (rare) now creates a
  fresh subject instead of adopting the scope's newest open review.

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

[Unreleased]: https://github.com/yasyf/cc-interact/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/yasyf/cc-interact/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/yasyf/cc-interact/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/yasyf/cc-interact/compare/v0.1.9...v0.5.0
[0.1.9]: https://github.com/yasyf/cc-interact/compare/v0.1.8...v0.1.9
[0.1.8]: https://github.com/yasyf/cc-interact/compare/v0.1.7...v0.1.8
[0.1.7]: https://github.com/yasyf/cc-interact/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/yasyf/cc-interact/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/yasyf/cc-interact/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/yasyf/cc-interact/compare/v0.1.3...v0.1.4
[0.1.0]: https://github.com/yasyf/cc-interact/releases/tag/v0.1.0
