package vcs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func snapshotTree(t *testing.T, repoRoot, scratchDir string) TreeRef {
	t.Helper()
	ref, err := SnapshotTree(context.Background(), repoRoot, scratchDir)
	if err != nil {
		t.Fatalf("snapshot tree: %v", err)
	}
	if ref.OID == "" {
		t.Fatal("snapshot returned an empty OID")
	}
	return ref
}

func countScratchObjects(t *testing.T, scratchDir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(scratchDir, "objects"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGitTreeSnapshotAndDiff(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, ".gitignore", "ignored.txt\n")
	write(t, dir, "tracked.go", "package a\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "init")
	scratch := t.TempDir()

	refA := snapshotTree(t, dir, scratch)
	if refA.Backend != "git" {
		t.Fatalf("backend = %q, want git", refA.Backend)
	}

	write(t, dir, "tracked.go", "package a\nfunc Edited() {}\n")
	write(t, dir, "untracked.go", "package a\nvar Untracked int\n")
	write(t, dir, "ignored.txt", "secret\n")

	refB := snapshotTree(t, dir, scratch)
	if refB.OID == refA.OID {
		t.Fatal("tree OID did not change after edits")
	}

	objectsBefore := countScratchObjects(t, scratch)
	refB2 := snapshotTree(t, dir, scratch)
	if refB2.OID != refB.OID {
		t.Fatalf("unchanged tree OID = %q, want %q", refB2.OID, refB.OID)
	}
	if after := countScratchObjects(t, scratch); after != objectsBefore {
		t.Fatalf("unchanged snapshot wrote objects: %d -> %d", objectsBefore, after)
	}

	patch, err := NewTreeDiffer(dir, scratch, refA.Backend).Diff(context.Background(), refA.OID, refB.OID)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, want := range []string{"+func Edited() {}", "+var Untracked int"} {
		if !strings.Contains(patch, want) {
			t.Fatalf("patch missing %q:\n%s", want, patch)
		}
	}
	for _, not := range []string{"ignored.txt", "secret"} {
		if strings.Contains(patch, not) {
			t.Fatalf("patch contains ignored content %q:\n%s", not, patch)
		}
	}

	status := gitInit(t, dir, "status", "--porcelain")
	if !strings.Contains(status, "?? untracked.go") {
		t.Fatalf("untracked.go should still be untracked in the real index:\n%s", status)
	}
}

func TestGitTreeSnapshotReseedsCorruptIndex(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "init")
	scratch := t.TempDir()

	refA := snapshotTree(t, dir, scratch)

	if err := os.WriteFile(filepath.Join(scratch, "index"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	refB := snapshotTree(t, dir, scratch)
	if refB.OID != refA.OID {
		t.Fatalf("OID after reseed = %q, want %q", refB.OID, refA.OID)
	}
}

func TestJJTreeSnapshotAndDiff(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	scratch := t.TempDir()
	write(t, dir, "a.txt", "one\n")

	refA := snapshotTree(t, dir, scratch)
	if refA.Backend != "jj" {
		t.Fatalf("backend = %q, want jj", refA.Backend)
	}

	write(t, dir, "a.txt", "one\ntwo\n")
	refB := snapshotTree(t, dir, scratch)
	if refB.OID == refA.OID {
		t.Fatal("commit id did not change after edit")
	}

	patch, err := NewTreeDiffer(dir, scratch, refB.Backend).Diff(context.Background(), refA.OID, refB.OID)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(patch, "+two") {
		t.Fatalf("patch missing edit:\n%s", patch)
	}
}

func resolved(t *testing.T, dir string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gitObjectsPath(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitInit(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "objects"))
}

func TestRepoObjectsDir(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (root, wantObjects string)
	}{
		{
			name: "plain repository",
			setup: func(t *testing.T) (string, string) {
				root := resolved(t, newRepo(t))
				return root, gitObjectsPath(t, root)
			},
		},
		{
			name: "linked worktree resolves through commondir",
			setup: func(t *testing.T) (string, string) {
				repo := newRepo(t)
				write(t, repo, "a.txt", "1\n")
				gitInit(t, repo, "add", "-A")
				gitInit(t, repo, "commit", "-qm", "c1")
				wt := filepath.Join(t.TempDir(), "wt")
				gitInit(t, repo, "worktree", "add", "-q", wt)
				root := resolved(t, wt)
				return root, gitObjectsPath(t, root)
			},
		},
		{
			name: "submodule gitfile without a commondir",
			setup: func(t *testing.T) (string, string) {
				super := t.TempDir()
				gitDir := filepath.Join(super, ".git", "modules", "sub")
				sub := filepath.Join(super, "sub")
				for _, dir := range []string{filepath.Join(gitDir, "objects"), sub} {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				write(t, sub, ".git", "gitdir: ../.git/modules/sub\n")
				return sub, filepath.Join(gitDir, "objects")
			},
		},
		{
			name: "gitfile pointing nowhere falls back",
			setup: func(t *testing.T) (string, string) {
				repo := filepath.Join(t.TempDir(), "repo")
				if err := os.MkdirAll(repo, 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, repo, ".git", "gitdir: ../nowhere\n")
				return repo, ""
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, wantObjects := tt.setup(t)
			got, ok := repoObjectsDir(root)
			if ok != (wantObjects != "") {
				t.Fatalf("ok = %v (%q), want %v", ok, got, wantObjects != "")
			}
			if got != wantObjects {
				t.Fatalf("objects dir = %q, want %q", got, wantObjects)
			}
		})
	}
}

func TestSnapshotTreeWarmRunsTwoGitCommands(t *testing.T) {
	dir := resolved(t, newRepo(t))
	write(t, dir, "a.go", "package a\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "init")
	scratch := t.TempDir()
	snapshotTree(t, dir, scratch)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	stub := t.TempDir()
	log := filepath.Join(stub, "git.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexec %q \"$@\"\n", log, realGit)
	if err := os.WriteFile(filepath.Join(stub, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	write(t, dir, "a.go", "package a\nvar X int\n")
	snapshotTree(t, dir, scratch)

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read git log: %v", err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		got = append(got, strings.TrimPrefix(line, "-C "+dir+" "))
	}
	want := []string{"add -A", "write-tree"}
	if !slices.Equal(got, want) {
		t.Fatalf("warm snapshot ran git %q, want %q", got, want)
	}
}
