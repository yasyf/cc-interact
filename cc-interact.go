// Package ccinteract is the composition root of the cc-interact framework: it wires a
// Config (store, daemon handler registry, edit gate, scope resolver, optional web client)
// into a runnable App. The domain-agnostic substrate lives in the sibling packages —
// subject, event, daemon, sse, consume, channel, store, paths, cmd, and the optional vcs
// module. A consumer registers a reducer and a handful of handlers; the framework owns the
// daemon lifecycle, the append-only event log, the SSE event plane, the edit gate, and the
// MCP channel scaffold.
package ccinteract
