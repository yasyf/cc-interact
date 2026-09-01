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

func TestRootMatchesGitToplevel(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{
			name:  "plain repository",
			setup: func(t *testing.T) string { return newRepo(t) },
		},
		{
			name: "nested subdirectory",
			setup: func(t *testing.T) string {
				repo := newRepo(t)
				sub := filepath.Join(repo, "a", "b")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				return sub
			},
		},
		{
			name: "linked worktree",
			setup: func(t *testing.T) string {
				repo := newRepo(t)
				write(t, repo, "a.txt", "1\n")
				gitInit(t, repo, "add", "-A")
				gitInit(t, repo, "commit", "-qm", "c1")
				wt := filepath.Join(t.TempDir(), "wt")
				gitInit(t, repo, "worktree", "add", "-q", wt)
				return wt
			},
		},
		{
			name: "symlinked path",
			setup: func(t *testing.T) string {
				repo := newRepo(t)
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(repo, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := tt.setup(t)
			want := strings.TrimSpace(gitInit(t, cwd, "rev-parse", "--show-toplevel"))
			root, err := Root(context.Background(), cwd)
			if err != nil {
				t.Fatalf("root: %v", err)
			}
			if root != want {
				t.Fatalf("root = %q, want git toplevel %q", root, want)
			}
		})
	}
}

func TestRootTakesNoSubprocess(t *testing.T) {
	repo := newRepo(t)
	want := strings.TrimSpace(gitInit(t, repo, "rev-parse", "--show-toplevel"))

	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "git"), []byte("#!/bin/sh\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stub)

	root, err := Root(context.Background(), repo)
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if root != want {
		t.Fatalf("root = %q, want %q", root, want)
	}
}

func TestBackend(t *testing.T) {
	markers := func(names ...string) func(t *testing.T) string {
		return func(t *testing.T) string {
			dir := t.TempDir()
			for _, name := range names {
				if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			return dir
		}
	}
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		want    string
		wantErr string
	}{
		{name: "git repository", setup: func(t *testing.T) string { return newRepo(t) }, want: "git"},
		{name: "jj repository", setup: markers(".jj"), want: "jj"},
		{name: "colocated prefers jj", setup: markers(".jj", ".git"), want: "jj"},
		{
			name:    "outside any repository",
			setup:   func(t *testing.T) string { return t.TempDir() },
			wantErr: "not inside a git or jj repository",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Backend(tt.setup(t))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("backend: %v", err)
			}
			if got != tt.want {
				t.Fatalf("backend = %q, want %q", got, tt.want)
			}
		})
	}
}
