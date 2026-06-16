package vcs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitInit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRepo(t *testing.T) string {
	dir := t.TempDir()
	gitInit(t, dir, "init", "-q", "-b", "main")
	return dir
}

func TestCaptureTrackedAndUntracked(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "kept.go", "package a\n")
	write(t, dir, "gone.go", "package a\nvar X int\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "init")

	// Uncommitted: modify a tracked file, add an untracked file, delete a tracked file.
	write(t, dir, "kept.go", "package a\nfunc New() {}\n")
	write(t, dir, "fresh.go", "package a\n// brand new\n")
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if head := strings.TrimSpace(gitInit(t, dir, "rev-parse", "HEAD")); snap.BaseRef != head {
		t.Fatalf("base = %q, want HEAD sha %q", snap.BaseRef, head)
	}
	if snap.Branch != "main" {
		t.Fatalf("branch = %q, want main", snap.Branch)
	}
	for _, want := range []string{"fresh.go", "func New()", "gone.go"} {
		if !strings.Contains(snap.PatchText, want) {
			t.Fatalf("patch missing %q:\n%s", want, snap.PatchText)
		}
	}

	byPath := map[string]string{}
	for _, f := range snap.Files {
		byPath[f.Path] = f.Status
	}
	if byPath["fresh.go"] != "A" || byPath["kept.go"] != "M" || byPath["gone.go"] != "D" {
		t.Fatalf("file statuses = %+v, want fresh=A kept=M gone=D", byPath)
	}

	// The real index must be untouched: gone.go still tracked, fresh.go untracked.
	status := gitInit(t, dir, "status", "--porcelain")
	if !strings.Contains(status, "?? fresh.go") {
		t.Fatalf("fresh.go should still be untracked in the real index:\n%s", status)
	}
}

func TestCaptureCommitlessUsesEmptyTree(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "hello\n")

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if snap.BaseRef != emptyTree {
		t.Fatalf("base = %q, want empty-tree %q", snap.BaseRef, emptyTree)
	}
	if !strings.Contains(snap.PatchText, "a.txt") || !strings.Contains(snap.PatchText, "hello") {
		t.Fatalf("commitless patch missing the new file:\n%s", snap.PatchText)
	}
	if len(snap.Files) != 1 || snap.Files[0].Status != "A" {
		t.Fatalf("files = %+v, want one added file", snap.Files)
	}
}

func TestCaptureDetachedHeadHasNoBranch(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "1\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "c1")
	sha := strings.TrimSpace(gitInit(t, dir, "rev-parse", "HEAD"))
	gitInit(t, dir, "checkout", "-q", sha) // detach

	write(t, dir, "a.txt", "1\n2\n")
	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if snap.Branch != "" {
		t.Fatalf("branch = %q, want empty on detached HEAD", snap.Branch)
	}
	if snap.BaseRef != sha {
		t.Fatalf("base = %q, want HEAD sha %q", snap.BaseRef, sha)
	}
}

func TestCaptureFingerprints(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.go", "package a\n")
	write(t, dir, "b.go", "package b\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "init")
	write(t, dir, "a.go", "package a\nfunc A() {}\n")
	write(t, dir, "b.go", "package b\nfunc B() {}\n")

	first := captureFingerprints(t, dir)
	if first["a.go"] == "" || first["b.go"] == "" {
		t.Fatalf("fingerprints missing: %+v", first)
	}

	// A no-op recapture yields identical fingerprints.
	second := captureFingerprints(t, dir)
	if first["a.go"] != second["a.go"] || first["b.go"] != second["b.go"] {
		t.Fatalf("fingerprints drifted across no-op recapture:\n%+v\n%+v", first, second)
	}

	// Changing one file's content changes its fingerprint only.
	write(t, dir, "a.go", "package a\nfunc A() {}\nfunc A2() {}\n")
	third := captureFingerprints(t, dir)
	if third["a.go"] == first["a.go"] {
		t.Fatal("a.go content changed but its fingerprint did not")
	}
	if third["b.go"] != first["b.go"] {
		t.Fatal("b.go did not change but its fingerprint did")
	}

	// A mode-only flip (chmod +x) changes the fingerprint too.
	if err := os.Chmod(filepath.Join(dir, "a.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	fourth := captureFingerprints(t, dir)
	if fourth["a.go"] == third["a.go"] {
		t.Fatal("a.go mode changed but its fingerprint did not")
	}
	if fourth["b.go"] != third["b.go"] {
		t.Fatal("b.go did not change but its fingerprint did")
	}
}

func captureFingerprints(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	out := map[string]string{}
	for _, f := range snap.Files {
		out[f.Path] = f.Fingerprint
	}
	return out
}

func TestCaptureRename(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "old.go", "package a\n\nfunc One() {}\nfunc Two() {}\nfunc Three() {}\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "init")

	if err := os.Rename(filepath.Join(dir, "old.go"), filepath.Join(dir, "new.go")); err != nil {
		t.Fatal(err)
	}

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.Files) != 1 {
		t.Fatalf("files = %+v, want exactly one rename", snap.Files)
	}
	f := snap.Files[0]
	if f.Status != "R" || f.Path != "new.go" || f.OldPath != "old.go" {
		t.Fatalf("file = %+v, want R old.go -> new.go", f)
	}
}

// diffBaseRepo builds commit A on main (trunk.go), then commit B (branch.go)
// on a feature branch, leaving the worktree clean on feature.
func diffBaseRepo(t *testing.T) (dir, shaA string) {
	t.Helper()
	dir = newRepo(t)
	write(t, dir, "trunk.go", "package a\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "A")
	shaA = strings.TrimSpace(gitInit(t, dir, "rev-parse", "HEAD"))
	gitInit(t, dir, "checkout", "-qb", "feature")
	write(t, dir, "branch.go", "package a\nvar B int\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "B")
	return dir, shaA
}

func TestCaptureDiffBase(t *testing.T) {
	cases := []struct {
		name            string
		setup           func(t *testing.T) (dir, wantBase string)
		base            string
		wantInPatch     []string
		wantNotInPatch  []string
		wantNoChanges   bool
		wantErrContains string
	}{
		{
			name: "dirty worktree uses session diff",
			setup: func(t *testing.T) (string, string) {
				dir, _ := diffBaseRepo(t)
				write(t, dir, "dirty.go", "package a\nvar D int\n")
				return dir, strings.TrimSpace(gitInit(t, dir, "rev-parse", "HEAD"))
			},
			wantInPatch:    []string{"dirty.go"},
			wantNotInPatch: []string{"branch.go"},
		},
		{
			name: "clean feature branch falls back to trunk fork point",
			setup: func(t *testing.T) (string, string) {
				dir, shaA := diffBaseRepo(t)
				return dir, shaA
			},
			wantInPatch:    []string{"branch.go"},
			wantNotInPatch: []string{"trunk.go"},
		},
		{
			name: "fallback finds origin/main when local main is gone",
			setup: func(t *testing.T) (string, string) {
				dir, shaA := diffBaseRepo(t)
				gitInit(t, dir, "update-ref", "refs/remotes/origin/main", shaA)
				gitInit(t, dir, "branch", "-qD", "main")
				return dir, shaA
			},
			wantInPatch: []string{"branch.go"},
		},
		{
			name: "clean tree on trunk is no changes",
			setup: func(t *testing.T) (string, string) {
				dir, _ := diffBaseRepo(t)
				gitInit(t, dir, "checkout", "-q", "main")
				return dir, ""
			},
			wantNoChanges: true,
		},
		{
			name: "clean tree with no trunk candidates points at --base",
			setup: func(t *testing.T) (string, string) {
				dir, _ := diffBaseRepo(t)
				gitInit(t, dir, "branch", "-qm", "main", "work")
				return dir, ""
			},
			wantErrContains: "pass --base",
		},
		{
			name: "explicit base diffs branch and dirty files from the fork point",
			setup: func(t *testing.T) (string, string) {
				dir, shaA := diffBaseRepo(t)
				write(t, dir, "dirty.go", "package a\nvar D int\n")
				return dir, shaA
			},
			base:        "main",
			wantInPatch: []string{"branch.go", "dirty.go"},
		},
		{
			name: "explicit base with no diff is no changes",
			setup: func(t *testing.T) (string, string) {
				dir, _ := diffBaseRepo(t)
				gitInit(t, dir, "checkout", "-q", "main")
				return dir, ""
			},
			base:          "main",
			wantNoChanges: true,
		},
		{
			name: "unknown explicit base fails resolving the fork point",
			setup: func(t *testing.T) (string, string) {
				dir, _ := diffBaseRepo(t)
				return dir, ""
			},
			base:            "nope",
			wantErrContains: "merge-base",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, wantBase := tc.setup(t)
			snap, err := Capture(context.Background(), dir, tc.base)
			if tc.wantNoChanges {
				if !errors.Is(err, ErrNoChanges) {
					t.Fatalf("err = %v, want ErrNoChanges", err)
				}
				return
			}
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErrContains)
				}
				if errors.Is(err, ErrNoChanges) {
					t.Fatalf("err = %v, must not be ErrNoChanges", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if wantBase != "" && snap.BaseRef != wantBase {
				t.Fatalf("base = %q, want %q", snap.BaseRef, wantBase)
			}
			for _, want := range tc.wantInPatch {
				if !strings.Contains(snap.PatchText, want) {
					t.Fatalf("patch missing %q:\n%s", want, snap.PatchText)
				}
			}
			for _, not := range tc.wantNotInPatch {
				if strings.Contains(snap.PatchText, not) {
					t.Fatalf("patch unexpectedly contains %q:\n%s", not, snap.PatchText)
				}
			}
			if len(snap.Files) == 0 {
				t.Fatal("files summary is empty")
			}
		})
	}
}

func TestCaptureAt(t *testing.T) {
	dir, shaA := diffBaseRepo(t)
	// A commit on top moves HEAD; the pinned base keeps the diff cumulative.
	write(t, dir, "later.go", "package a\nvar L int\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "C")

	snap, err := CaptureAt(context.Background(), dir, shaA)
	if err != nil {
		t.Fatalf("capture at %s: %v", shaA, err)
	}
	if snap.BaseRef != shaA {
		t.Fatalf("base = %q, want pinned %q", snap.BaseRef, shaA)
	}
	for _, want := range []string{"branch.go", "later.go"} {
		if !strings.Contains(snap.PatchText, want) {
			t.Fatalf("patch missing %q:\n%s", want, snap.PatchText)
		}
	}

	gitInit(t, dir, "checkout", "-q", "main")
	if _, err := CaptureAt(context.Background(), dir, shaA); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges when the worktree matches the pin", err)
	}
}
