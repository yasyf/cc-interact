package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/yasyf/cc-interact/event"
	"github.com/yasyf/cc-interact/paths"
	"github.com/yasyf/cc-interact/subject"
)

// AppendFunc persists an event then publishes its subject's wakeup — the single
// persist→publish chokepoint. Handlers receive it via HandlerCtx; lifecycle
// hooks receive the *Server (whose Append is this func).
type AppendFunc func(ctx context.Context, e *event.Event) (int64, error)

// ToolCall is the edit-guard's view of a PreToolUse call: the tool name plus its
// raw, un-interpreted input. The gate decides; this package never parses Input.
type ToolCall struct {
	Name  string
	Input json.RawMessage
}

// GateFunc is the edit-gate verdict the consumer injects. It runs only for a
// resolved subject; allow=false carries a human-readable reason.
type GateFunc func(ctx context.Context, s subject.Subject, tool ToolCall) (allow bool, reason string)

// Config builds a Server. The zero value is not runnable; AppName, Paths,
// Version, ActiveStatuses, and WindowAlive are the load-bearing inputs.
type Config struct {
	// AppName labels logs and user-facing daemon messages (cc-review: "cc-review").
	AppName string
	// Paths is the state-directory layout (socket, db, http handshake, locks).
	Paths paths.Paths
	// Version is this binary's own build version, used for same-or-newer-wins
	// socket eviction (compared via version.Newer).
	Version string

	// ActiveStatuses is the adoptable subject status set, both for the store's
	// FindAdoptableByScope and the in-memory Policy.Active twin (cc-review: {"open"}).
	ActiveStatuses []string
	// WindowAlive reports whether a pid-bound window still owns its subject; it
	// is the pid arm of Policy.Held (cc-review: process liveness).
	WindowAlive func(pid int) bool

	// ScopeResolve maps the envelope's raw Scope to the canonical ownership scope
	// once per RPC, so handlers see a resolved Scope (cc-review: vcs.Root). nil is
	// the identity.
	ScopeResolve func(ctx context.Context, raw string) (string, error)

	// Gate is the edit-guard verdict (cc-review: block while a review is open).
	// nil allows every edit for a resolved subject.
	Gate GateFunc
	// GateErrorReason is the fail-closed message returned when a subject's status
	// cannot be read (guard-edit blocks rather than silently permit).
	GateErrorReason string
	// GateObserve, when set, records every resolved verdict (a ledger hook).
	GateObserve func(ctx context.Context, s subject.Subject, tool ToolCall, allow bool, reason string)

	// OnPresenceChange fires when a named consumer's connectivity to a subject
	// flips. It receives the live Server so it can Append a domain presence event
	// (cc-review: channel.changed). nil disables emission; Attach still runs.
	OnPresenceChange func(ctx context.Context, s *Server, subjectID string, connected bool)
	// PresenceEventType is the event Type the presence change emits; frames of
	// this type are filtered from named consumers. Empty delivers every frame.
	PresenceEventType string
	// PresenceDebounce overrides the SSE default when non-zero.
	PresenceDebounce time.Duration

	// BootReconcile runs once at boot, after the socket binds but before the HTTP
	// plane accepts, so it sees an empty presence registry (cc-review: close out
	// orphaned channel.changed state). nil is a no-op.
	BootReconcile func(ctx context.Context, s *Server) error

	// Migrate adds the consumer's own tables after the core schema is applied.
	Migrate func(ctx context.Context, db *sql.DB) error

	// FixedPort pins the HTTP plane to a known port (the Vite dev proxy); 0 binds
	// the last-published port if free, else an ephemeral one.
	FixedPort int
}
