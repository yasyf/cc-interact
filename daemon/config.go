package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
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
// Version, and ActiveStatuses are the load-bearing inputs.
type Config struct {
	// AppName labels logs and user-facing daemon messages (cc-review: "cc-review").
	AppName string
	// Paths is the state-directory layout (socket, db, http handshake, locks).
	Paths paths.Paths
	// Version is this binary's own build version, used for same-or-newer-wins
	// socket eviction (compared via version.Newer).
	Version string
	// MaxFrameBytes overrides the control server's request-frame limit. Zero uses
	// the 64 MiB default.
	MaxFrameBytes int

	// ActiveStatuses is the subject status set Policy.Active treats as live and
	// resumable across session rotation (cc-review: {"open"}).
	ActiveStatuses []string

	// ScopeResolve canonicalizes the envelope's raw Scope once per RPC, so
	// handlers see a resolved Scope. It is canonicalization, not authorization:
	// return the raw value when there is no canonical form (cc-review: vcs.Root,
	// else the cwd as given) — resolution never rejects a request, and handlers
	// own their own scope preconditions. nil is the identity.
	ScopeResolve func(ctx context.Context, raw string) string

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

	// BindAddr is the address the HTTP plane binds. Empty means 127.0.0.1, the
	// loopback-only default; "0.0.0.0" exposes the plane to the LAN. A non-loopback
	// bind with no HTTPToken is refused (New returns ErrUnauthenticatedBind), since
	// the plane would otherwise serve every off-host request unauthenticated.
	BindAddr string
	// HTTPToken, when non-empty, requires every non-loopback HTTP request to
	// carry "Authorization: Bearer <token>" (or the ?token= query fallback that
	// browser EventSource needs). Loopback requests always bypass it.
	HTTPToken string
	// OnHTTPStart fires once the HTTP plane is bound and its handshake published;
	// consumers hook mDNS advertising here. Like Background, the ctx is cancelled
	// at shutdown and Serve waits for the hook to return, so cleanup on ctx.Done()
	// (mDNS goodbye packets) completes before the process exits.
	OnHTTPStart func(ctx context.Context, port int)
	// ExtraHTTPListeners are called once at HTTP start; each listener serves the
	// same auth-guarded handler as the primary bind (e.g. a TLS listener with
	// certs from `tailscale cert`). A factory error fails startup. The loopback
	// token bypass stays per-connection (peer address), and since extra peers may
	// be non-loopback, New refuses extra listeners with no HTTPToken
	// (ErrUnauthenticatedBind).
	ExtraHTTPListeners []func(ctx context.Context) (net.Listener, error)
	// PublicHandler, when set, serves every request no Mux route matches,
	// OUTSIDE the auth guard — the consumer's static SPA shell (index.html,
	// assets, service worker), which a remote browser must be able to fetch
	// before it has any script that could attach the token. Routes mounted on
	// Mux stay auth-guarded; never mount "/" on Mux alongside this.
	PublicHandler http.Handler
	// TrustedPeer, when set, is a third acceptance path beside the loopback
	// bypass and the bearer token: a non-loopback TCP peer whose IP the hook
	// reports as trusted passes without a token, under the same Origin gate as
	// the loopback bypass (see TrustedOrigin). The IP arrives Unmap()ed, so a
	// v4-in-v6 ::ffff:a.b.c.d compares as its v4 form. With TrustedPeer set,
	// New permits a non-loopback bind and extra listeners without an HTTPToken:
	// every off-host request still passes the hook or the token, so an
	// untrusted peer with no token configured is refused. The hook must be
	// safe for concurrent use: beyond per-request checks, it is re-polled
	// periodically for every live tokenless stream it admitted, and a stream
	// whose peer it stops trusting is closed.
	TrustedPeer func(ip netip.Addr) bool
	// TrustedOrigin widens the Origin gate on the no-token bypasses: a browser
	// request whose Origin names a non-loopback host the hook approves passes
	// where only absent, localhost, or loopback Origins did. It must approve
	// only hosts this daemon is itself served under (its MagicDNS name, its
	// own tailnet IPs) — never peer names, or a foreign page on a trusted
	// machine could drive the daemon.
	TrustedOrigin func(host string) bool
}
