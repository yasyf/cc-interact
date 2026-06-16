package vcs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootDetection(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) (cwd, wantRoot string)
		wantErr string
	}{
		{
			name: "jj wins over git when colocated",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				for _, marker := range []string{".jj", ".git"} {
					if err := os.Mkdir(filepath.Join(dir, marker), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				sub := filepath.Join(dir, "sub")
				if err := os.Mkdir(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				return sub, dir
			},
		},
		{
			name: "git file marks a worktree",
			setup: func(t *testing.T) (string, string) {
				repo := newRepo(t)
				write(t, repo, "a.txt", "1\n")
				gitInit(t, repo, "add", "-A")
				gitInit(t, repo, "commit", "-qm", "c1")
				wt := filepath.Join(t.TempDir(), "wt")
				gitInit(t, repo, "worktree", "add", "-q", wt)
				resolved, err := filepath.EvalSymlinks(wt)
				if err != nil {
					t.Fatal(err)
				}
				return wt, resolved
			},
		},
		{
			name: "no repository",
			setup: func(t *testing.T) (string, string) {
				return t.TempDir(), ""
			},
			wantErr: "not inside a git or jj repository",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd, wantRoot := tt.setup(t)
			root, err := Root(context.Background(), cwd)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("root: %v", err)
			}
			if root != wantRoot {
				t.Fatalf("root = %q, want %q", root, wantRoot)
			}
		})
	}
}
