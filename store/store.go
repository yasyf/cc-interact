// Package store is cc-interact's exact v1 append-only state layer.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/cc-interact/internal/statepath"
	"github.com/yasyf/daemonkit/paths"

	_ "modernc.org/sqlite"
)

// Store wraps the single-writer sqlite connection.
type Store struct {
	db *sql.DB
}

const schemaVersion = 1

const coreSchema = `
CREATE TABLE cc_interact_schema_v1 (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  fingerprint TEXT NOT NULL
);
CREATE TABLE subjects (
  id         TEXT PRIMARY KEY,
  slug       TEXT NOT NULL DEFAULT '',
  session_id TEXT,
  scope      TEXT NOT NULL,
  claude_pid INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'open',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_subjects_session_scope
  ON subjects(session_id, scope) WHERE session_id IS NOT NULL;
CREATE INDEX idx_subjects_scope ON subjects(scope);
CREATE INDEX idx_subjects_pid_scope ON subjects(claude_pid, scope);
CREATE UNIQUE INDEX idx_subjects_slug ON subjects(slug) WHERE slug <> '';
CREATE TABLE events (
  subject_id TEXT NOT NULL REFERENCES subjects(id),
  seq        INTEGER NOT NULL,
  origin     TEXT NOT NULL,
  type       TEXT NOT NULL,
  payload    TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  dedup_key  TEXT,
  PRIMARY KEY (subject_id, seq)
);
CREATE UNIQUE INDEX idx_events_dedup ON events(subject_id, dedup_key) WHERE dedup_key IS NOT NULL;
CREATE TABLE agents (
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
CREATE TABLE directives (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  subject_id   TEXT NOT NULL REFERENCES subjects(id),
  agent_id     TEXT NOT NULL,
  origin       TEXT NOT NULL,
  text         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  delivered_at INTEGER
);
CREATE INDEX idx_directives_pending
  ON directives(subject_id, agent_id) WHERE delivered_at IS NULL;
`

// Schema is a consumer's exact v1 DDL extension. It is applied only while a
// fresh database is created and participates byte-for-byte in its fingerprint.
type Schema struct {
	DDL string
}

// Compose returns one exact schema extension from ordered component schemas.
func Compose(schemas ...Schema) Schema {
	var ddl strings.Builder
	for _, schema := range schemas {
		if strings.TrimSpace(schema.DDL) == "" {
			continue
		}
		if ddl.Len() != 0 {
			ddl.WriteByte('\n')
		}
		ddl.WriteString(schema.DDL)
	}
	return Schema{DDL: ddl.String()}
}

// Validate rejects compatibility DDL and ownership of the core epoch markers.
func (s Schema) Validate() error {
	normalized := strings.Join(strings.Fields(strings.ToUpper(s.DDL)), " ")
	for _, forbidden := range []string{
		"IF NOT EXISTS", "ALTER TABLE", "PRAGMA USER_VERSION", "CC_INTERACT_SCHEMA_V1",
	} {
		if strings.Contains(normalized, forbidden) {
			return fmt.Errorf("store: exact schema contains forbidden %q", forbidden)
		}
	}
	return nil
}

// Fingerprint returns the exact core-plus-consumer v1 schema fingerprint.
func (s Schema) Fingerprint() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("cc-interact-store-v1\x00" + coreSchema + "\x00" + s.DDL))
	return hex.EncodeToString(sum[:]), nil
}

// Path returns cc-interact's exact v1 store path under the consumer state dir.
func Path(p paths.Paths) string { return statepath.DB(p) }

// Open opens or creates an exact v1 store. Existing databases must match both
// user_version and the compiled core-plus-consumer fingerprint; they are never
// altered, migrated, or interpreted under a different schema.
func Open(ctx context.Context, dbPath string, extension Schema) (*Store, error) {
	fingerprint, err := extension.Fingerprint()
	if err != nil {
		return nil, err
	}
	sqliteSchemaFingerprint, err := expectedSQLiteSchemaFingerprint(ctx, extension, fingerprint)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dbPath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect sqlite: %w", err)
	}
	if exists && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("store: database path %q is not a regular file", dbPath)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if exists {
		if err := verifySchema(ctx, db, fingerprint, sqliteSchemaFingerprint); err != nil {
			_ = db.Close()
			return nil, err
		}
		return &Store{db: db}, nil
	}
	if err := createSchema(ctx, db, extension, fingerprint); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := verifySchema(ctx, db, fingerprint, sqliteSchemaFingerprint); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

func createSchema(ctx context.Context, db *sql.DB, extension Schema, fingerprint string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, coreSchema); err != nil {
		return fmt.Errorf("create core schema: %w", err)
	}
	if strings.TrimSpace(extension.DDL) != "" {
		if _, err := tx.ExecContext(ctx, extension.DDL); err != nil {
			return fmt.Errorf("create consumer schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cc_interact_schema_v1(id, fingerprint) VALUES(1, ?)`, fingerprint); err != nil {
		return fmt.Errorf("record schema fingerprint: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema: %w", err)
	}
	return nil
}

func expectedSQLiteSchemaFingerprint(ctx context.Context, extension Schema, fingerprint string) (string, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return "", fmt.Errorf("open expected sqlite_schema: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if err := createSchema(ctx, db, extension, fingerprint); err != nil {
		return "", fmt.Errorf("create expected sqlite_schema: %w", err)
	}
	value, err := liveSQLiteSchemaFingerprint(ctx, db)
	if err != nil {
		return "", fmt.Errorf("fingerprint expected sqlite_schema: %w", err)
	}
	return value, nil
}

func liveSQLiteSchemaFingerprint(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT type, name, tbl_name, COALESCE(sql, '')
FROM sqlite_schema
WHERE name NOT GLOB 'sqlite_*'
ORDER BY type, name, tbl_name, sql`)
	if err != nil {
		return "", fmt.Errorf("query sqlite_schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	digest := sha256.New()
	for rows.Next() {
		var objectType, name, tableName, definition string
		if err := rows.Scan(&objectType, &name, &tableName, &definition); err != nil {
			return "", fmt.Errorf("scan sqlite_schema: %w", err)
		}
		for _, field := range []string{objectType, name, tableName, definition} {
			var length [8]byte
			binary.BigEndian.PutUint64(length[:], uint64(len(field)))
			_, _ = digest.Write(length[:])
			_, _ = digest.Write([]byte(field))
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate sqlite_schema: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func verifySchema(ctx context.Context, db *sql.DB, wantFingerprint, wantSQLiteSchemaFingerprint string) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version != schemaVersion {
		return fmt.Errorf("store: schema version %d, want exactly %d", version, schemaVersion)
	}
	var fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT fingerprint FROM cc_interact_schema_v1 WHERE id=1`).Scan(&fingerprint); err != nil {
		return fmt.Errorf("read schema fingerprint: %w", err)
	}
	if fingerprint != wantFingerprint {
		return fmt.Errorf("store: schema fingerprint %q, want exactly %q", fingerprint, wantFingerprint)
	}
	sqliteSchemaFingerprint, err := liveSQLiteSchemaFingerprint(ctx, db)
	if err != nil {
		return err
	}
	if sqliteSchemaFingerprint != wantSQLiteSchemaFingerprint {
		return fmt.Errorf("store: sqlite_schema fingerprint %q, want exactly %q", sqliteSchemaFingerprint, wantSQLiteSchemaFingerprint)
	}
	return nil
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
