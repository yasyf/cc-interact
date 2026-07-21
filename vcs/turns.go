package vcs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yasyf/cc-interact/store"
)

// ErrTurnNotFound is returned when a turn lookup by id finds no row.
var ErrTurnNotFound = errors.New("turn not found")

const turnsSchema = `
CREATE TABLE turns (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_root         TEXT NOT NULL,
  backend           TEXT NOT NULL DEFAULT 'git',
  session_id        TEXT NOT NULL DEFAULT '',
  claude_pid        INTEGER NOT NULL DEFAULT 0,
  prompt_excerpt    TEXT NOT NULL DEFAULT '',
  tree_start        TEXT NOT NULL,
  tree_end          TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT 'open',
  started_at        INTEGER NOT NULL,
  ended_at          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_turns_repo ON turns(repo_root, id);
CREATE INDEX idx_turns_repo_open ON turns(repo_root, claude_pid) WHERE status='open';
`

const turnCols = `id, repo_root, backend, session_id, claude_pid, prompt_excerpt, tree_start, tree_end, status, started_at, ended_at`

// Turn is one Claude prompt→stop window in a repo, bracketed by working-tree
// snapshots. Timestamps are unix milliseconds.
type Turn struct {
	ID            int64
	RepoRoot      string
	Backend       string // git | jj
	SessionID     string
	ClaudePID     int
	PromptExcerpt string
	TreeStart     string
	TreeEnd       string // empty until closed
	Status        string // open | closed | interrupted
	StartedAt     int64
	EndedAt       int64 // 0 while open
}

// TurnStore is the turn ledger over a single-writer sqlite connection.
type TurnStore struct {
	db *sql.DB
}

// NewTurnStore wraps db for turn CRUD. The turns table must already be part of
// the daemon's exact StoreSchema.
func NewTurnStore(db *sql.DB) *TurnStore { return &TurnStore{db: db} }

// TurnsSchema returns the exact v1 turn-ledger schema extension.
func TurnsSchema() store.Schema { return store.Schema{DDL: turnsSchema} }

func scanTurn(row interface{ Scan(...any) error }) (Turn, error) {
	var t Turn
	if err := row.Scan(&t.ID, &t.RepoRoot, &t.Backend, &t.SessionID, &t.ClaudePID, &t.PromptExcerpt,
		&t.TreeStart, &t.TreeEnd, &t.Status, &t.StartedAt, &t.EndedAt); err != nil {
		return Turn{}, err
	}
	return t, nil
}

// CreateTurn inserts a new open turn, stamping Status and StartedAt, and
// returns it with the allocated id.
func (s *TurnStore) CreateTurn(ctx context.Context, t Turn) (Turn, error) {
	t.Status = "open"
	t.StartedAt = time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO turns(repo_root, backend, session_id, claude_pid, prompt_excerpt, tree_start, tree_end, status, started_at, ended_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		t.RepoRoot, t.Backend, t.SessionID, t.ClaudePID, t.PromptExcerpt,
		t.TreeStart, t.TreeEnd, t.Status, t.StartedAt, t.EndedAt)
	if err != nil {
		return Turn{}, fmt.Errorf("create turn: %w", err)
	}
	t.ID, err = res.LastInsertId()
	if err != nil {
		return Turn{}, fmt.Errorf("create turn: %w", err)
	}
	return t, nil
}

// CloseTurn ends a turn with its closing tree snapshot and final status,
// stamping ended_at.
func (s *TurnStore) CloseTurn(ctx context.Context, id int64, treeEnd, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE turns SET tree_end=?, status=?, ended_at=? WHERE id=?`,
		treeEnd, status, time.Now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("close turn: %w", err)
	}
	return nil
}

// CloseOpenTurnsForWindow marks every open turn of a Claude window (repo +
// pid) interrupted; tree_end stays empty because no closing snapshot exists.
func (s *TurnStore) CloseOpenTurnsForWindow(ctx context.Context, repoRoot string, claudePID int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE turns SET status='interrupted', ended_at=? WHERE repo_root=? AND claude_pid=? AND status='open'`,
		time.Now().UnixMilli(), repoRoot, claudePID)
	if err != nil {
		return fmt.Errorf("close open turns: %w", err)
	}
	return nil
}

// LatestOpenTurn returns the newest open turn of a Claude window (repo + pid).
// ok is false when none is open.
func (s *TurnStore) LatestOpenTurn(ctx context.Context, repoRoot string, claudePID int) (Turn, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+turnCols+` FROM turns WHERE repo_root=? AND claude_pid=? AND status='open' ORDER BY id DESC LIMIT 1`,
		repoRoot, claudePID)
	t, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, false, nil
	}
	return t, err == nil, err
}

// GetTurn returns the turn by id, or ErrTurnNotFound.
func (s *TurnStore) GetTurn(ctx context.Context, id int64) (Turn, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+turnCols+` FROM turns WHERE id=?`, id)
	t, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	return t, err
}

// ListTurnsBySession returns a session's turns in ledger order.
func (s *TurnStore) ListTurnsBySession(ctx context.Context, sessionID string) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+turnCols+` FROM turns WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list turns by session: %w", err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAttributableTurns returns a repo's turns started at or after sinceMs,
// oldest first, capped at 1000.
func (s *TurnStore) ListAttributableTurns(ctx context.Context, repoRoot string, sinceMs int64) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+turnCols+` FROM turns WHERE repo_root=? AND started_at>=? ORDER BY id LIMIT 1000`,
		repoRoot, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("list attributable turns: %w", err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTurnsByIDs returns the turns with the given ids, ordered by id.
func (s *TurnStore) ListTurnsByIDs(ctx context.Context, ids []int64) ([]Turn, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+turnCols+` FROM turns WHERE id IN (`+placeholders+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list turns by ids: %w", err)
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
