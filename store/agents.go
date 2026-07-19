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

// RegisterAgent inserts or refreshes an agent registration without changing its start time.
func (s *Store) RegisterAgent(ctx context.Context, info agent.Info) error {
	var endedAt any
	if !info.EndedAt.IsZero() {
		endedAt = unix(info.EndedAt)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents(
			subject_id, agent_id, parent_agent_id, agent_type, session_id, transcript_path, status, started_at, ended_at
		 ) VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(subject_id, agent_id) DO UPDATE SET
			parent_agent_id=excluded.parent_agent_id,
			agent_type=excluded.agent_type,
			session_id=excluded.session_id,
			transcript_path=excluded.transcript_path,
			status=excluded.status,
			ended_at=excluded.ended_at`,
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
		return fmt.Errorf("register agent: %w", err)
	}
	return nil
}

// CloseAgent marks an agent done at endedAt.
func (s *Store) CloseAgent(ctx context.Context, subjectID, agentID string, endedAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET status=?, ended_at=? WHERE subject_id=? AND agent_id=?`,
		agent.StatusDone, unix(endedAt), subjectID, agentID)
	if err != nil {
		return fmt.Errorf("close agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("close agent: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("close agent: %w", ErrNotFound)
	}
	return nil
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
