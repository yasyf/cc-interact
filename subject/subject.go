// Package subject is the single ownership resolver. A subject belongs to a
// window — one Claude Code process — not to a session id: ids rotate on /clear,
// resume, and compact, while the pid stays put for the window's whole life. The
// scope is an opaque ownership domain (a repo root, a project, a document); the
// resolver never interprets it, and a fresh subject's slug is supplied
// precomputed so no domain detail enters this package.
package subject

import (
	"context"
	"time"
)

// Subject is one unit of work keyed to a window (pid) + opaque scope. Status is
// a generic lifecycle string the domain owns; this package treats it only
// through Policy and Lifecycle, never by value.
type Subject struct {
	ID        string
	Slug      string // opaque URL name, supplied precomputed at creation
	SessionID string // empty when NULL
	Scope     string // opaque ownership domain (cc-review: repo root)
	ClaudePID int    // 0 when detached (no live window owns it)
	Status    string // generic lifecycle state
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Window identifies one Claude Code process: the current (rotating) session id
// plus the stable pid. ClaudePID 0 means manual CLI use outside any window.
type Window struct {
	Session   string
	ClaudePID int
}

// Policy injects the two domain predicates the resolver must not hardcode.
// Held reports whether the window owning a subject is still alive, vetoing
// adoption of a subject another live window holds. Active reports whether a
// subject is a candidate for adoption (cc-review: status is open); it is the
// in-memory twin of the store's active-set filter in FindAdoptableByScope, and
// the resolver applies it to a window's pid-latest subject (which the store
// returns in any status) before rebinding.
type Policy struct {
	Held   func(ctx context.Context, s Subject) bool
	Active func(s Subject) bool
}

// Lifecycle names the two status values the resolver writes when it mutates a
// subject's state. Initial is the lifecycle state a freshly created subject is
// born in. Closed is the terminal state a fresh start assigns the window's prior
// subject before recreating, removing it from the adoptable set. Adoption
// candidacy itself is decided by Policy.Active and the store's active-set, not
// by these values.
type Lifecycle struct {
	Initial string
	Closed  string
}
