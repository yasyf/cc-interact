package store

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
