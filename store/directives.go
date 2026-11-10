package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

const directiveCols = `id, subject_id, agent_id, origin, text, created_at, delivered_at`

func scanDirective(row interface{ Scan(...any) error }) (agent.Directive, error) {
	var (
		directive agent.Directive
		created   int64
		delivered sql.NullInt64
	)
	if err := row.Scan(
		&directive.ID,
		&directive.SubjectID,
		&directive.AgentID,
		&directive.Origin,
		&directive.Text,
		&created,
		&delivered,
	); err != nil {
		return agent.Directive{}, err
	}
	directive.CreatedAt = fromUnix(created)
	if delivered.Valid {
		directive.DeliveredAt = fromUnix(delivered.Int64)
	}
	return directive, nil
}

// ChannelPushed is the span of event seqs, After exclusive through Through
// inclusive, that a window's proven channel stream has pushed. The zero value is
// empty.
type ChannelPushed struct {
	After, Through int64
}

// EnqueueDirective atomically observes the addressed agent's status and queues a
// directive. A valid eventSeq links the directive to the logged event it carries.
func (s *Store) EnqueueDirective(
	ctx context.Context,
	subjectID, agentID, origin, text string,
	eventSeq sql.NullInt64,
	now time.Time,
) (agent.Directive, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return agent.Directive{}, "", fmt.Errorf("begin enqueue directive tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	status := agent.StatusRunning
	if agentID != agent.TopLevel {
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM agents WHERE subject_id=? AND agent_id=?`,
			subjectID, agentID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return agent.Directive{}, "", fmt.Errorf("enqueue directive agent: %w", ErrNotFound)
		}
		if err != nil {
			return agent.Directive{}, "", fmt.Errorf("enqueue directive agent: %w", err)
		}
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO directives(subject_id, agent_id, origin, text, event_seq, created_at) VALUES(?,?,?,?,?,?)`,
		subjectID, agentID, origin, text, eventSeq, unix(now))
	if err != nil {
		return agent.Directive{}, "", fmt.Errorf("insert directive: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return agent.Directive{}, "", fmt.Errorf("insert directive id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return agent.Directive{}, "", fmt.Errorf("commit enqueue directive: %w", err)
	}
	return agent.Directive{
		ID:        id,
		SubjectID: subjectID,
		AgentID:   agentID,
		Origin:    origin,
		Text:      text,
		CreatedAt: now,
	}, status, nil
}

// HasPendingDirectives reports whether the agent holds at least one undelivered
// directive, without delivering any — a non-destructive EXISTS peek. excludeOrigin
// drops directives of that origin from the count; "" counts every origin.
func (s *Store) HasPendingDirectives(ctx context.Context, subjectID, agentID, excludeOrigin string) (bool, error) {
	q := `SELECT EXISTS(SELECT 1 FROM directives WHERE subject_id=? AND agent_id=? AND delivered_at IS NULL`
	args := []any{subjectID, agentID}
	if excludeOrigin != "" {
		q += ` AND origin<>?`
		args = append(args, excludeOrigin)
	}
	q += `)`
	var pending int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&pending); err != nil {
		return false, fmt.Errorf("has pending directives: %w", err)
	}
	return pending == 1, nil
}

// DeliveredEventSeqs returns the seqs from through to of the subject's events whose
// linked top-level directive is delivered.
func (s *Store) DeliveredEventSeqs(ctx context.Context, subjectID string, from, to int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_seq FROM directives
		 WHERE subject_id=? AND event_seq BETWEEN ? AND ? AND agent_id=? AND delivered_at IS NOT NULL`,
		subjectID, from, to, agent.TopLevel)
	if err != nil {
		return nil, fmt.Errorf("select delivered event seqs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, fmt.Errorf("scan delivered event seq: %w", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select delivered event seqs: %w", err)
	}
	return seqs, nil
}

// DrainDirectives atomically marks every pending directive delivered and returns
// them in FIFO order, except a directive whose linked event lies in pushed: the
// channel already delivered it, so it is marked and not returned.
func (s *Store) DrainDirectives(
	ctx context.Context,
	subjectID, agentID string,
	pushed ChannelPushed,
	now time.Time,
) ([]agent.Directive, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin drain directives tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT `+directiveCols+`
		 FROM directives
		 WHERE subject_id=? AND agent_id=? AND delivered_at IS NULL
		   AND (event_seq IS NULL OR event_seq<=? OR event_seq>?)
		 ORDER BY created_at ASC, id ASC`,
		subjectID, agentID, pushed.After, pushed.Through)
	if err != nil {
		return nil, fmt.Errorf("select pending directives: %w", err)
	}
	directives := []agent.Directive{}
	for rows.Next() {
		directive, err := scanDirective(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan pending directive: %w", err)
		}
		directives = append(directives, directive)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("select pending directives: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close pending directives: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE directives SET delivered_at=?
		 WHERE subject_id=? AND agent_id=? AND delivered_at IS NULL`,
		unix(now), subjectID, agentID); err != nil {
		return nil, fmt.Errorf("mark directives delivered: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit drain directives: %w", err)
	}
	for i := range directives {
		directives[i].DeliveredAt = now
	}
	return directives, nil
}
