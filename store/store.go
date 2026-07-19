// Package store is cc-interact's append-only state layer: a modernc.org/sqlite
// (pure-Go) database holding subjects and the single per-subject event log that
// drives the realtime fan-out. The core schema is domain-agnostic; a consumer
// supplies its own tables through the migrate callback at Open. Rows are never
// deleted; status flags carry state.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the single-writer sqlite connection.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS subjects (
  id         TEXT PRIMARY KEY,
  slug       TEXT NOT NULL DEFAULT '',
  session_id TEXT,
  scope      TEXT NOT NULL,
  claude_pid INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'open',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_subjects_session_scope
  ON subjects(session_id, scope) WHERE session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subjects_scope ON subjects(scope);
CREATE INDEX IF NOT EXISTS idx_subjects_pid_scope ON subjects(claude_pid, scope);
CREATE UNIQUE INDEX IF NOT EXISTS idx_subjects_slug ON subjects(slug) WHERE slug <> '';
CREATE TABLE IF NOT EXISTS events (
  subject_id TEXT NOT NULL REFERENCES subjects(id),
  seq        INTEGER NOT NULL,
  origin     TEXT NOT NULL,
  type       TEXT NOT NULL,
  payload    TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  dedup_key  TEXT,
  PRIMARY KEY (subject_id, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_dedup ON events(subject_id, dedup_key) WHERE dedup_key IS NOT NULL;
CREATE TABLE IF NOT EXISTS agents (
  subject_id      TEXT NOT NULL REFERENCES subjects(id),
  agent_id        TEXT NOT NULL,
  parent_agent_id TEXT NOT NULL,
  agent_type      TEXT NOT NULL,
  session_id      TEXT NOT NULL,
  transcript_path TEXT NOT NULL,
  status          TEXT NOT NULL,
  started_at      INTEGER NOT NULL,
  ended_at        INTEGER,
  PRIMARY KEY (subject_id, agent_id)
);
CREATE TABLE IF NOT EXISTS directives (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  subject_id   TEXT NOT NULL REFERENCES subjects(id),
  agent_id     TEXT NOT NULL,
  origin       TEXT NOT NULL,
  text         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  delivered_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_directives_pending
  ON directives(subject_id, agent_id) WHERE delivered_at IS NULL;
`

// Open opens (creating if needed) the database at dbPath, applies the core
// schema, then runs migrate so the domain can add its own tables (idempotent
// CREATE TABLE IF NOT EXISTS). A single serialized writer (SetMaxOpenConns(1))
// with WAL avoids "database is locked" across the fan-out, REST, and the event
// bus. There are no migrations beyond migrate: on a core schema change, wipe the
// local state dir.
func Open(dbPath string, migrate func(ctx context.Context, db *sql.DB) error) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if migrate != nil {
		if err := migrate(ctx, db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying connection so the domain can query its own tables.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(n int64) time.Time { return time.Unix(n, 0) }

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
