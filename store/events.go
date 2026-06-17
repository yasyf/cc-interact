package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yasyf/cc-interact/event"
)

const eventCols = `subject_id, seq, origin, type, payload, created_at, dedup_key`

func scanEvent(row interface{ Scan(...any) error }) (event.Event, error) {
	var (
		e       event.Event
		payload string
		dedup   sql.NullString
		created int64
	)
	if err := row.Scan(&e.SubjectID, &e.Seq, &e.Origin, &e.Type, &payload, &created, &dedup); err != nil {
		return event.Event{}, err
	}
	e.Payload = []byte(payload)
	e.DedupKey = dedup.String
	e.CreatedAt = fromUnix(created)
	return e, nil
}

// AppendEvent allocates the next per-subject seq and appends the event,
// returning the assigned seq. The MAX(seq)+1 read and the insert run in one
// transaction on the single writer, so seq is gap-free and monotonic. When
// DedupKey is set and already present, the existing event's seq is returned and
// nothing is inserted.
func (s *Store) AppendEvent(ctx context.Context, e *event.Event) (int64, error) {
	payload := e.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin event tx: %w", err)
	}
	defer tx.Rollback()

	// Dedup inside the tx: it holds the single connection across the check and
	// the insert, so the lookup-then-insert is atomic. Scope by subject so a key
	// reused across subjects can't return another subject's seq.
	if e.DedupKey != "" {
		var seq int64
		err := tx.QueryRowContext(ctx,
			`SELECT seq FROM events WHERE subject_id=? AND dedup_key=?`, e.SubjectID, e.DedupKey).Scan(&seq)
		if err == nil {
			return seq, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("event dedup lookup: %w", err)
		}
	}

	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE subject_id=?`, e.SubjectID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("next event seq: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(subject_id, seq, origin, type, payload, created_at, dedup_key)
		 VALUES(?,?,?,?,?,?,?)`,
		e.SubjectID, seq, e.Origin, e.Type, string(payload), unix(time.Now()), nullString(e.DedupKey)); err != nil {
		return 0, fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit event: %w", err)
	}
	e.Seq = seq
	return seq, nil
}

// EventsSince returns events with seq greater than cursor, oldest first.
// excludeOrigin drops rows of that origin (an agent passes its own origin to kill
// the echo loop); "" sees every origin.
func (s *Store) EventsSince(ctx context.Context, subjectID string, cursor int64, excludeOrigin string) ([]event.Event, error) {
	q := `SELECT ` + eventCols + ` FROM events WHERE subject_id=? AND seq>?`
	args := []any{subjectID, cursor}
	if excludeOrigin != "" {
		q += ` AND origin<>?`
		args = append(args, excludeOrigin)
	}
	q += ` ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
