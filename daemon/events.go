package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/yasyf/cc-interact/event"
)

// Append is the single chokepoint through which every event enters the log, from
// any origin: it persists the event, then publishes the wakeup. Persisting
// before publishing guarantees a woken consumer can read the row.
func (s *Server) Append(ctx context.Context, e *event.Event) (int64, error) {
	seq, err := s.store.AppendEvent(ctx, e)
	if err != nil {
		return 0, fmt.Errorf("append %s event: %w", e.Type, err)
	}
	s.bus.Publish(e.SubjectID)
	return seq, nil
}

// ResolveSubject maps a ref — a slug (browser) or a canonical id (agent-side
// consumer) — to the canonical subject id keying the Bus and the events table.
// Satisfies sse.Backend.
func (s *Server) ResolveSubject(ctx context.Context, ref string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM subjects WHERE id=? OR slug=?`, ref, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve subject %q: %w", ref, err)
	}
	return id, true, nil
}

// EventsSince returns events past cursor, oldest first. Satisfies sse.Backend.
func (s *Server) EventsSince(ctx context.Context, subjectID string, cursor int64, excludeOrigin string) ([]event.Event, error) {
	return s.store.EventsSince(ctx, subjectID, cursor, excludeOrigin)
}

// Subscribe exposes the bus to the SSE plane. Satisfies sse.Backend.
func (s *Server) Subscribe(subjectID string) (<-chan struct{}, func()) {
	return s.bus.Subscribe(subjectID)
}

// Attach registers a named SSE stream consumer for a subject and returns its
// detach. Satisfies sse.Backend.
func (s *Server) Attach(subjectID, consumer string, pid int) func() {
	return s.activity.Attach(subjectID, consumer, pid)
}

// ConsumerConnected reports whether any named stream consumer is attached to the
// subject, with a grace window that papers over reconnect blips. Satisfies
// sse.Backend.
func (s *Server) ConsumerConnected(subjectID string) bool {
	return s.activity.AttachedWithin(subjectID, attachGrace)
}
