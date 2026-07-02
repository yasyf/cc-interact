package subject

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Resolver maps windows to the subjects they own, driving Store with Policy as
// the only domain-specific input.
type Resolver struct {
	Store  Store
	Policy Policy
}

// Find returns the window's subject without ever writing: the exact
// (session, scope) binding, else the window's latest subject by pid in any
// status — covering a stale session id and activity after a session rotation.
func (rs Resolver) Find(ctx context.Context, w Window, scope string) (Subject, bool, error) {
	if s, ok, err := rs.Store.FindBySessionScope(ctx, w.Session, scope); err != nil || ok {
		return s, ok, err
	}
	return rs.Store.FindLatestByWindowScope(ctx, w.ClaudePID, scope)
}

// Peek is the read-only twin of Start: it returns the subject a non-fresh start
// would resume — the exact binding, the window's pid-latest subject, or the
// adoptable latest subject in the scope — without writing, so the caller can act
// against the would-be-resumed subject before any state changes.
func (rs Resolver) Peek(ctx context.Context, w Window, scope string) (Subject, bool, error) {
	if s, ok, err := rs.Find(ctx, w, scope); err != nil || ok {
		return s, ok, err
	}
	if s, ok, err := rs.Store.FindAdoptableByScope(ctx, scope); err != nil {
		return Subject{}, false, err
	} else if ok && rs.Policy.Active(s) && !rs.Policy.Held(ctx, s) {
		return s, true, nil
	}
	return Subject{}, false, nil
}

// Start returns the subject a start attaches to and whether that is a resume (an
// existing subject) versus a fresh create:
//
//  1. exact (session, scope) binding             → resume
//  2. the window's pid-latest subject            → rebind to the new session id, resume
//  3. adoptable subject with no live window       → adopt, resume
//  4. otherwise                                   → create (status lc.Initial)
//
// fresh=true skips adoption: it closes (status lc.Closed) and detaches the
// window's own subject (rows 1–2 only), then creates. slug is the precomputed
// name a freshly created subject takes.
func (rs Resolver) Start(ctx context.Context, w Window, scope, slug string, lc Lifecycle, fresh bool) (Subject, bool, error) {
	if fresh {
		if s, ok, err := rs.Find(ctx, w, scope); err != nil {
			return Subject{}, false, err
		} else if ok {
			if err := rs.Store.SetStatus(ctx, s.ID, lc.Closed); err != nil {
				return Subject{}, false, err
			}
			if err := rs.Store.Detach(ctx, s.ID); err != nil {
				return Subject{}, false, err
			}
		}
		return rs.create(ctx, w, scope, slug, lc.Initial)
	}

	if s, ok, err := rs.Store.FindBySessionScope(ctx, w.Session, scope); err != nil {
		return Subject{}, false, err
	} else if ok {
		if w.ClaudePID != 0 && s.ClaudePID != w.ClaudePID {
			swapped, err := rs.Store.Rebind(ctx, s.ID, s.ClaudePID, w.Session, w.ClaudePID)
			if err != nil {
				return Subject{}, false, err
			}
			if swapped {
				s.ClaudePID = w.ClaudePID
			} else {
				// CAS miss: a concurrent rebind moved the pid under us; the
				// session binding still holds, so re-read and continue.
				if s, err = rs.Store.Get(ctx, s.ID); err != nil {
					return Subject{}, false, err
				}
			}
		}
		return s, true, nil
	}

	if s, ok, err := rs.Store.FindLatestByWindowScope(ctx, w.ClaudePID, scope); err != nil {
		return Subject{}, false, err
	} else if ok {
		swapped, err := rs.Store.Rebind(ctx, s.ID, s.ClaudePID, w.Session, w.ClaudePID)
		if err != nil {
			return Subject{}, false, err
		}
		if swapped {
			s.SessionID = w.Session
			return s, true, nil
		}
	}

	if s, ok, err := rs.Store.FindAdoptableByScope(ctx, scope); err != nil {
		return Subject{}, false, err
	} else if ok && rs.Policy.Active(s) && !rs.Policy.Held(ctx, s) {
		swapped, err := rs.Store.Rebind(ctx, s.ID, s.ClaudePID, w.Session, w.ClaudePID)
		if err != nil {
			return Subject{}, false, err
		}
		if swapped {
			s.SessionID = w.Session
			s.ClaudePID = w.ClaudePID
			return s, true, nil
		}
	}

	return rs.create(ctx, w, scope, slug, lc.Initial)
}

// Rebind follows session rotation at window start: it points the window's own
// active subject — found by the rotated session id, else by the window pid — at
// the new session id. It never adopts a subject owned by another window. Session
// rotation is not a claim on the scope: a review a different session opened is
// left untouched, so two windows sharing a scope can never cross-bind here.
// Adopting a dead window's orphan is the job of an explicit Start alone. An
// empty session id is a no-op.
func (rs Resolver) Rebind(ctx context.Context, w Window, scope string) error {
	if w.Session == "" {
		return nil
	}

	if _, ok, err := rs.Store.FindBySessionScope(ctx, w.Session, scope); err != nil || ok {
		return err
	}

	if s, ok, err := rs.Store.FindLatestByWindowScope(ctx, w.ClaudePID, scope); err != nil {
		return err
	} else if ok && rs.Policy.Active(s) {
		_, err := rs.Store.Rebind(ctx, s.ID, s.ClaudePID, w.Session, w.ClaudePID)
		return err
	}

	return nil
}

func (rs Resolver) create(ctx context.Context, w Window, scope, slug, status string) (Subject, bool, error) {
	s, err := rs.Store.Create(ctx, newID(), slug, w.Session, scope, w.ClaudePID, status)
	return s, false, err
}

// newID returns a random 128-bit identifier as 32 lowercase hex chars.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("read random id: %v", err))
	}
	return hex.EncodeToString(b[:])
}
