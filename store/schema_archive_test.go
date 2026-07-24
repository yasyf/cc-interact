package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func seedSubject(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB().Exec(`INSERT INTO subjects(id, scope, created_at, updated_at) VALUES('s1', '/repo', 1, 1)`); err != nil {
		t.Fatalf("seed subject: %v", err)
	}
}

func subjectCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM subjects`).Scan(&n); err != nil {
		t.Fatalf("count subjects: %v", err)
	}
	return n
}

func storeBackups(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".*.bak")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return matches
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestOpenUnsupportedSchemaPolicy(t *testing.T) {
	widgets := Schema{DDL: `CREATE TABLE widgets (id TEXT PRIMARY KEY);`}

	tests := []struct {
		name         string
		seed         *Schema
		open         Schema
		opts         []Option
		wantErr      string
		wantBackups  int
		wantWarnings int
		wantSubjects int
	}{
		{
			name:         "mismatch archived starts fresh",
			seed:         &Schema{},
			open:         widgets,
			opts:         []Option{WithUnsupportedSchema(ArchiveUnsupportedSchema)},
			wantBackups:  1,
			wantWarnings: 1,
			wantSubjects: 0,
		},
		{
			name:        "mismatch fails closed by default",
			seed:        &Schema{},
			open:        widgets,
			wantErr:     "schema fingerprint",
			wantBackups: 0,
		},
		{
			name:         "matching schema left untouched",
			seed:         &widgets,
			open:         widgets,
			opts:         []Option{WithUnsupportedSchema(ArchiveUnsupportedSchema)},
			wantBackups:  0,
			wantWarnings: 0,
			wantSubjects: 1,
		},
		{
			name:         "empty path created fresh",
			open:         widgets,
			opts:         []Option{WithUnsupportedSchema(ArchiveUnsupportedSchema)},
			wantBackups:  0,
			wantWarnings: 0,
			wantSubjects: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "test.db")
			if tt.seed != nil {
				seeded, err := Open(ctx, path, *tt.seed)
				if err != nil {
					t.Fatalf("seed open: %v", err)
				}
				seedSubject(t, seeded)
				if err := seeded.Close(); err != nil {
					t.Fatalf("seed close: %v", err)
				}
			}

			warnings := captureWarnings(t)
			s, err := Open(ctx, path, tt.open, tt.opts...)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("open error = %v, want substring %q", err, tt.wantErr)
				}
				if _, statErr := os.Stat(path); statErr != nil {
					t.Fatalf("fail-closed must preserve the store: %v", statErr)
				}
			} else {
				if err != nil {
					t.Fatalf("open = %v, want nil", err)
				}
				t.Cleanup(func() { _ = s.Close() })
				if got := subjectCount(t, s); got != tt.wantSubjects {
					t.Fatalf("reopened subject count = %d, want %d", got, tt.wantSubjects)
				}
			}

			if bak := storeBackups(t, path); len(bak) != tt.wantBackups {
				t.Fatalf("backup count = %d (%v), want %d", len(bak), bak, tt.wantBackups)
			}
			if got := strings.Count(warnings.String(), "archived unsupported-schema store"); got != tt.wantWarnings {
				t.Fatalf("archive warnings = %d, want %d\nlog: %s", got, tt.wantWarnings, warnings.String())
			}
			if tt.wantWarnings > 0 {
				if bak := storeBackups(t, path); !strings.Contains(warnings.String(), bak[0]) {
					t.Fatalf("warning must name the backup path %q\nlog: %s", bak[0], warnings.String())
				}
			}
		})
	}
}

func TestArchiveUnsupportedStoreMovesSidecars(t *testing.T) {
	tests := []struct {
		name     string
		sidecars map[string]string
	}{
		{
			name:     "with wal and shm sidecars",
			sidecars: map[string]string{"-wal": "wal-bytes", "-shm": "shm-bytes"},
		},
		{
			name:     "no sidecars present",
			sidecars: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.db")
			if err := os.WriteFile(path, []byte("wedged"), 0o600); err != nil {
				t.Fatal(err)
			}
			for suffix, content := range tt.sidecars {
				if err := os.WriteFile(path+suffix, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			backup, err := archiveUnsupportedStore(path)
			if err != nil {
				t.Fatalf("archiveUnsupportedStore = %v", err)
			}

			base := filepath.Base(backup)
			if filepath.Dir(backup) != dir || !strings.HasPrefix(base, "test.db.") || !strings.HasSuffix(base, ".bak") {
				t.Fatalf("backup path = %q, want test.db.<fp>.<ts>.bak in %q", backup, dir)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("original must be renamed away, stat = %v", statErr)
			}
			if data, err := os.ReadFile(backup); err != nil || string(data) != "wedged" {
				t.Fatalf("backup contents = %q, %v; want %q", data, err, "wedged")
			}
			for suffix, content := range tt.sidecars {
				if _, statErr := os.Stat(path + suffix); !os.IsNotExist(statErr) {
					t.Fatalf("sidecar %s must be renamed away, stat = %v", suffix, statErr)
				}
				if data, err := os.ReadFile(backup + suffix); err != nil || string(data) != content {
					t.Fatalf("sidecar %s backup = %q, %v; want %q", suffix, data, err, content)
				}
			}
		})
	}
}

// TestArchivePreservesLockedMatchingStore proves a merely locked but
// schema-matching store is never archived: the SQLITE_BUSY that its verify
// queries hit is transient, not an ErrUnsupportedSchema mismatch, so it
// propagates and the store is left on disk.
func TestArchivePreservesLockedMatchingStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	seeded, err := Open(ctx, path, Schema{})
	if err != nil {
		t.Fatal(err)
	}
	if err := seeded.Close(); err != nil {
		t.Fatal(err)
	}

	locker, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	locker.SetMaxOpenConns(1)
	defer func() { _ = locker.Close() }()
	conn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, stmt := range []string{`PRAGMA journal_mode=DELETE`, `PRAGMA locking_mode=EXCLUSIVE`, `BEGIN EXCLUSIVE`} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("lock setup %q: %v", stmt, err)
		}
	}

	warnings := captureWarnings(t)
	fresh, openErr := Open(ctx, path, Schema{}, WithUnsupportedSchema(ArchiveUnsupportedSchema))
	if fresh != nil {
		_ = fresh.Close()
	}
	_, _ = conn.ExecContext(ctx, `ROLLBACK`)

	if openErr == nil {
		t.Fatal("a locked but schema-matching store must not be opened by archiving it")
	}
	if errors.Is(openErr, ErrUnsupportedSchema) {
		t.Fatalf("a locked store must not be treated as a schema mismatch: %v", openErr)
	}
	if bak := storeBackups(t, path); len(bak) != 0 {
		t.Fatalf("a locked healthy store must never be archived; found %v", bak)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the locked store must be preserved on disk: %v", statErr)
	}
	if got := strings.Count(warnings.String(), "archived unsupported-schema store"); got != 0 {
		t.Fatalf("a locked store must not log an archive warning:\n%s", warnings.String())
	}
}

// TestConcurrentArchiveSingleFlight proves two simultaneous archiving Opens of a
// mismatched store produce exactly one backup and one warning, with both openers
// succeeding on the same fresh store.
func TestConcurrentArchiveSingleFlight(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	seeded, err := Open(ctx, path, Schema{})
	if err != nil {
		t.Fatal(err)
	}
	seedSubject(t, seeded)
	if err := seeded.Close(); err != nil {
		t.Fatal(err)
	}

	widgets := Schema{DDL: `CREATE TABLE widgets (id TEXT PRIMARY KEY);`}
	warnings := captureWarnings(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var storesMu sync.Mutex
	var stores []*Store
	for range 2 {
		go func() {
			<-start
			s, err := Open(ctx, path, widgets, WithUnsupportedSchema(ArchiveUnsupportedSchema))
			if s != nil {
				storesMu.Lock()
				stores = append(stores, s)
				storesMu.Unlock()
			}
			results <- err
		}()
	}
	close(start)
	err1, err2 := <-results, <-results
	t.Cleanup(func() {
		for _, s := range stores {
			_ = s.Close()
		}
	})

	if err1 != nil || err2 != nil {
		t.Fatalf("both concurrent opens must succeed: %v, %v", err1, err2)
	}
	if len(stores) != 2 {
		t.Fatalf("both openers must return a store, got %d", len(stores))
	}
	if bak := storeBackups(t, path); len(bak) != 1 {
		t.Fatalf("single-flight archive must leave exactly one backup, found %v", bak)
	}
	if got := strings.Count(warnings.String(), "archived unsupported-schema store"); got != 1 {
		t.Fatalf("single-flight archive must log exactly one warning, got %d:\n%s", got, warnings.String())
	}
	for i, s := range stores {
		if got := subjectCount(t, s); got != 0 {
			t.Fatalf("store %d must ride the fresh store (0 rows), got %d", i, got)
		}
	}
}

// TestArchiveIsolatesFromLiveHandle proves archiving out from under a still-open
// handle yields a clean, isolated fresh store: writes on the stale handle never
// leak into it. The stale handle's own fate is best-effort and unasserted.
func TestArchiveIsolatesFromLiveHandle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	old, err := Open(ctx, path, Schema{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if _, err := old.DB().Exec(`INSERT INTO subjects(id, scope, created_at, updated_at) VALUES('old-1', '/repo', 1, 1)`); err != nil {
		t.Fatal(err)
	}

	fresh, err := Open(ctx, path, Schema{DDL: `CREATE TABLE widgets (id TEXT PRIMARY KEY);`}, WithUnsupportedSchema(ArchiveUnsupportedSchema))
	if err != nil {
		t.Fatalf("archive open with a live handle: %v", err)
	}
	defer func() { _ = fresh.Close() }()

	if got := subjectCount(t, fresh); got != 0 {
		t.Fatalf("fresh store must be empty, got %d", got)
	}
	if bak := storeBackups(t, path); len(bak) != 1 {
		t.Fatalf("archive must leave exactly one backup, found %v", bak)
	}
	_, _ = old.DB().Exec(`INSERT INTO subjects(id, scope, created_at, updated_at) VALUES('old-2', '/repo', 2, 2)`)
	if got := subjectCount(t, fresh); got != 0 {
		t.Fatalf("a stale live handle must not leak rows into the fresh store, got %d", got)
	}
}
