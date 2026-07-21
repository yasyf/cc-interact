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

// EnqueueDirective atomically observes the addressed agent's status and queues a directive.
func (s *Store) EnqueueDirective(
	ctx context.Context,
	subjectID, agentID, origin, text string,
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
		`INSERT INTO directives(subject_id, agent_id, origin, text, created_at) VALUES(?,?,?,?,?)`,
		subjectID, agentID, origin, text, unix(now))
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

// DrainDirectives atomically marks and returns pending directives in FIFO order.
func (s *Store) DrainDirectives(
	ctx context.Context,
	subjectID, agentID string,
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
		 ORDER BY created_at ASC, id ASC`,
		subjectID, agentID)
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
