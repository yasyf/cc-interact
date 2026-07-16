# cc-interact Development Guide

A domain-agnostic framework for human-in-the-loop Claude agents — daemon, event stream, MCP channel, and optional web UI.

## Repository Structure

```
cc-interact/
├── cc-interact.go      # package ccinteract — Config, App, App.Run (composition root)
├── subject/            # Subject, Window, Store iface, Policy, Resolver (rotation/adopt CAS)
├── event/              # Event, Origin consts, Bus (append-only log primitives)
├── daemon/             # lazy daemon: unix-socket IPC, Envelope/Reply + handler registry,
│                       #   spawn/evict, edit-gate, Activity presence, per-repo lock
├── sse/                # /events SSE plane (REQUIRED) + Backend iface; StaticHandler(fs.FS) opt-in
├── consume/            # StreamSource, ConsumeEvents, persisted cursor (resilient SSE client)
├── channel/            # generic MCP stdio server + StreamEvents + Connectivity feature
├── store/              # modernc.org/sqlite: Open(path, migrate), core DDL, SubjectStore, event SQL
├── paths/              # ~/.<app> state-dir layout (Paths{App} value)
├── cmd/                # reusable cobra: Daemon/Watch/Status/Stop/SessionRecord/GuardEdit/Channel
├── vcs/                # OPTIONAL: git/jj snapshot (Root/Capture/Snapshot) + turn capture
├── examples/echo/      # headless non-review consumer (REST + channel, no SPA) — the quickstart
├── web/                # OPTIONAL npm package @cc-interact/react (Vite library mode)
├── plugin-template/    # templated plugin scaffold a consumer fills for its own agent UI
├── AGENTS.md           # This file — shared conventions
└── README.md           # Project overview
```

The library has no `internal/` directory: a consumer needs the surface, so hiding is by
doc comment and lowercase identifiers, not the compiler. The required surface is the
**API + daemon business logic** (daemon, event stream, SSE plane, channel, gate, store);
the **web frontend** (`web/` + `sse.StaticHandler`) is an opt-in client a consumer mounts
only for a browser UI. A headless consumer interacts via the API + MCP channel + CLI.
