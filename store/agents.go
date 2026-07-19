package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yasyf/cc-interact/agent"
)

const agentCols = `subject_id, agent_id, parent_agent_id, agent_type, session_id, transcript_path, status, started_at, ended_at`

func scanAgent(row interface{ Scan(...any) error }) (agent.Info, error) {
	var (
		info    agent.Info
		started int64
		ended   sql.NullInt64
	)
	if err := row.Scan(
		&info.SubjectID,
		&info.AgentID,
		&info.ParentAgentID,
		&info.AgentType,
		&info.SessionID,
		&info.TranscriptPath,
		&info.Status,
		&started,
		&ended,
	); err != nil {
		return agent.Info{}, err
	}
	info.StartedAt = fromUnix(started)
	if ended.Valid {
		info.EndedAt = fromUnix(ended.Int64)
	}
	return info, nil
}

// RegisterAgent inserts or refreshes an agent registration without changing its
// start time, reporting whether the row was newly created. created is decided by
// the insert itself (an insert-or-nothing on the primary key), so a first-
// registration caller needs no racy pre-read; a conflict refreshes the mutable
// fields in the same transaction.
func (s *Store) RegisterAgent(ctx context.Context, info agent.Info) (bool, error) {
	var endedAt any
	if !info.EndedAt.IsZero() {
		endedAt = unix(info.EndedAt)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin register agent tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO agents(
			subject_id, agent_id, parent_agent_id, agent_type, session_id, transcript_path, status, started_at, ended_at
		 ) VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(subject_id, agent_id) DO NOTHING`,
		info.SubjectID,
		info.AgentID,
		info.ParentAgentID,
		info.AgentType,
		info.SessionID,
		info.TranscriptPath,
		info.Status,
		unix(info.StartedAt),
		endedAt,
	)
	if err != nil {
		return false, fmt.Errorf("register agent insert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("register agent rows: %w", err)
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE agents SET
				parent_agent_id=?, agent_type=?, session_id=?, transcript_path=?, status=?, ended_at=?
			 WHERE subject_id=? AND agent_id=?`,
			info.ParentAgentID, info.AgentType, info.SessionID, info.TranscriptPath, info.Status, endedAt,
			info.SubjectID, info.AgentID,
		); err != nil {
			return false, fmt.Errorf("register agent update: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit register agent: %w", err)
	}
	return n == 1, nil
}

// CloseAgent marks a running agent done at endedAt, reporting whether this call
// made the transition. The running→done update is guarded, so a concurrent or
// repeat close moves the row exactly once (closed=false for the losers and for an
// already-done agent); a missing row is ErrNotFound.
func (s *Store) CloseAgent(ctx context.Context, subjectID, agentID string, endedAt time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin close agent tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE agents SET status=?, ended_at=? WHERE subject_id=? AND agent_id=? AND status<>?`,
		agent.StatusDone, unix(endedAt), subjectID, agentID, agent.StatusDone)
	if err != nil {
		return false, fmt.Errorf("close agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("close agent: %w", err)
	}
	if n == 1 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit close agent: %w", err)
		}
		return true, nil
	}
	// No transition: the agent is already done, or its row is missing.
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM agents WHERE subject_id=? AND agent_id=?`, subjectID, agentID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("close agent lookup: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit close agent: %w", err)
	}
	return false, nil
}

// GetAgent returns the identified agent or ErrNotFound when it does not exist.
func (s *Store) GetAgent(ctx context.Context, subjectID, agentID string) (agent.Info, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE subject_id=? AND agent_id=?`,
		subjectID, agentID)
	info, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Info{}, ErrNotFound
	}
	if err != nil {
		return agent.Info{}, fmt.Errorf("get agent: %w", err)
	}
	return info, nil
}

// ListPendingDirectiveAgents returns every done agent that still holds at least
// one undelivered directive, across all subjects, ordered by subject then agent.
// It is a non-destructive peek — delivered_at is never touched — so a reconcile
// sweep can re-announce stranded directives without consuming them.
func (s *Store) ListPendingDirectiveAgents(ctx context.Context) ([]agent.Info, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentCols+` FROM agents a
		 WHERE a.status=? AND EXISTS (
		   SELECT 1 FROM directives d
		   WHERE d.subject_id=a.subject_id AND d.agent_id=a.agent_id AND d.delivered_at IS NULL
		 )
		 ORDER BY a.subject_id ASC, a.agent_id ASC`,
		agent.StatusDone)
	if err != nil {
		return nil, fmt.Errorf("list pending directive agents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var agents []agent.Info
	for rows.Next() {
		info, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending directive agents: %w", err)
		}
		agents = append(agents, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending directive agents: %w", err)
	}
	return agents, nil
}

// ListAgents returns a subject's agents ordered by start time.
func (s *Store) ListAgents(ctx context.Context, subjectID string) ([]agent.Info, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE subject_id=? ORDER BY started_at ASC, agent_id ASC`,
		subjectID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var agents []agent.Info
	for rows.Next() {
		info, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		agents = append(agents, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	return agents, nil
}
