package vcs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireJJ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not installed")
	}
	t.Setenv("JJ_USER", "t")
	t.Setenv("JJ_EMAIL", "t@t")
}

func jjRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", append([]string{"--color=never", "--no-pager"}, args...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("jj %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func newJJRepo(t *testing.T, colocate bool) string {
	dir := t.TempDir()
	args := []string{"git", "init"}
	if colocate {
		args = append(args, "--colocate")
	}
	jjRun(t, dir, append(args, ".")...)
	return dir
}

func TestJJCaptureColocatedWithBookmark(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, true)
	write(t, dir, "a.txt", "hello\n")
	jjRun(t, dir, "bookmark", "create", "somebranch", "-r", "@")

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if snap.Branch != "somebranch" {
		t.Fatalf("branch = %q, want somebranch", snap.Branch)
	}
	if snap.RepoRoot != dir {
		t.Fatalf("repo root = %q, want %q", snap.RepoRoot, dir)
	}
	for _, want := range []string{"a.txt", "hello"} {
		if !strings.Contains(snap.PatchText, want) {
			t.Fatalf("patch missing %q:\n%s", want, snap.PatchText)
		}
	}
	if len(snap.Files) != 1 || snap.Files[0].Path != "a.txt" || snap.Files[0].Status != "A" {
		t.Fatalf("files = %+v, want a.txt added", snap.Files)
	}
}

func TestJJCaptureNoBookmarkUsesChangeID(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, true)
	write(t, dir, "a.txt", "hello\n")

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.Branch) != 8 {
		t.Fatalf("branch = %q, want an 8-char change-id prefix", snap.Branch)
	}
	want := strings.TrimSpace(jjRun(t, dir, "log", "--no-graph", "-r", "@", "-T", "change_id.shortest(8)"))
	if snap.Branch != want {
		t.Fatalf("branch = %q, want change id %q", snap.Branch, want)
	}
}

func TestJJCapturePureRepo(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	write(t, dir, "a.txt", "hello\n")

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if snap.RepoRoot != dir {
		t.Fatalf("repo root = %q, want %q", snap.RepoRoot, dir)
	}
	if len(snap.BaseRef) != 12 {
		t.Fatalf("base = %q, want a 12-char commit id", snap.BaseRef)
	}
	want := strings.TrimSpace(jjRun(t, dir, "log", "--no-graph", "-r", "@-", "-T", "commit_id.shortest(12)"))
	if snap.BaseRef != want {
		t.Fatalf("base = %q, want parent commit %q", snap.BaseRef, want)
	}
	if !strings.Contains(snap.PatchText, "hello") {
		t.Fatalf("patch missing the new file:\n%s", snap.PatchText)
	}
	if len(snap.Files) != 1 || snap.Files[0].Path != "a.txt" || snap.Files[0].Status != "A" {
		t.Fatalf("files = %+v, want a.txt added", snap.Files)
	}
}

func TestJJCaptureFingerprints(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	write(t, dir, "a.go", "package a\n")
	write(t, dir, "b.go", "package b\n")
	jjRun(t, dir, "commit", "-m", "init")
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
}

func TestJJCaptureFileStatuses(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	write(t, dir, "kept.go", "package a\n")
	write(t, dir, "gone.go", "package a\nvar X int\n")
	jjRun(t, dir, "commit", "-m", "init")

	// fresh.go must not resemble gone.go, or jj's rename detection pairs them
	// into an R instead of A + D.
	write(t, dir, "kept.go", "package a\nfunc New() {}\n")
	write(t, dir, "fresh.go", "while the moon spins backwards\nthe lighthouse hums in C minor\n")
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	byPath := map[string]string{}
	for _, f := range snap.Files {
		byPath[f.Path] = f.Status
	}
	if byPath["fresh.go"] != "A" || byPath["kept.go"] != "M" || byPath["gone.go"] != "D" {
		t.Fatalf("file statuses = %+v, want fresh=A kept=M gone=D", byPath)
	}
}

// newJJRepoWithTrunk builds a repo whose trunk() resolves to a real remote
// bookmark: commit A on main pushed to a local bare origin, commit B on a
// feature bookmark, a later trunk commit T past the fork point, and the empty
// working copy back on the feature.
func newJJRepoWithTrunk(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, origin, "init", "-q", "--bare")
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	jjRun(t, repo, "git", "init", ".")
	jjRun(t, repo, "git", "remote", "add", "origin", origin)
	write(t, repo, "trunk.txt", "t\n")
	jjRun(t, repo, "commit", "-m", "A")
	jjRun(t, repo, "bookmark", "create", "main", "-r", "@-")
	jjRun(t, repo, "git", "push", "-b", "main")
	write(t, repo, "branch.txt", "b\n")
	jjRun(t, repo, "commit", "-m", "B")
	jjRun(t, repo, "bookmark", "create", "feat", "-r", "@-")
	jjRun(t, repo, "new", "main")
	write(t, repo, "advance.txt", "x\n")
	jjRun(t, repo, "commit", "-m", "T")
	jjRun(t, repo, "bookmark", "set", "main", "-r", "@-")
	jjRun(t, repo, "git", "push", "-b", "main")
	jjRun(t, repo, "new", "feat")
	return repo
}

func TestJJCaptureFallsBackToTrunkForkPoint(t *testing.T) {
	requireJJ(t)
	dir := newJJRepoWithTrunk(t)

	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	wantBase := strings.TrimSpace(jjRun(t, dir, "log", "--no-graph",
		"-r", "latest(heads(::trunk() & ::@))", "-T", "commit_id.shortest(12)"))
	if snap.BaseRef != wantBase {
		t.Fatalf("base = %q, want fork point %q", snap.BaseRef, wantBase)
	}
	if !strings.Contains(snap.PatchText, "branch.txt") {
		t.Fatalf("patch missing the branch's committed work:\n%s", snap.PatchText)
	}
	// Diffing from the fork point, not the trunk tip: no reverse diff of T.
	if strings.Contains(snap.PatchText, "advance.txt") {
		t.Fatalf("patch reverse-includes a post-fork trunk commit:\n%s", snap.PatchText)
	}
}

func TestJJCaptureFallsBackToLocalBookmark(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	write(t, dir, "trunk.txt", "t\n")
	jjRun(t, dir, "commit", "-m", "A")
	jjRun(t, dir, "bookmark", "create", "main", "-r", "@-")
	write(t, dir, "branch.txt", "b\n")
	jjRun(t, dir, "commit", "-m", "B")

	// No remote, so trunk() is root(); a local main bookmark still names the trunk.
	snap, err := Capture(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	wantBase := strings.TrimSpace(jjRun(t, dir, "log", "--no-graph", "-r", `bookmarks(exact:"main")`, "-T", "commit_id.shortest(12)"))
	if snap.BaseRef != wantBase {
		t.Fatalf("base = %q, want local main %q", snap.BaseRef, wantBase)
	}
	if !strings.Contains(snap.PatchText, "branch.txt") || strings.Contains(snap.PatchText, "trunk.txt") {
		t.Fatalf("patch must contain branch.txt only:\n%s", snap.PatchText)
	}
}

func TestJJCaptureNoTrunkErrors(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	write(t, dir, "a.txt", "hello\n")
	jjRun(t, dir, "commit", "-m", "one")

	// Working copy clean and trunk() resolves to root(): refuse to diff the
	// whole history.
	_, err := Capture(context.Background(), dir, "")
	if err == nil || !strings.Contains(err.Error(), "pass --base") {
		t.Fatalf("err = %v, want no-trunk guidance pointing at --base", err)
	}
	if errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, must not be ErrNoChanges", err)
	}
}

func TestJJCaptureExplicitBase(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	write(t, dir, "a.txt", "a\n")
	jjRun(t, dir, "commit", "-m", "one")
	write(t, dir, "b.txt", "b\n")
	jjRun(t, dir, "commit", "-m", "two")
	write(t, dir, "c.txt", "c\n")

	wantBase := strings.TrimSpace(jjRun(t, dir, "log", "--no-graph", "-r", "@--", "-T", "commit_id.shortest(12)"))
	snap, err := Capture(context.Background(), dir, "@--")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if snap.BaseRef != wantBase {
		t.Fatalf("base = %q, want %q", snap.BaseRef, wantBase)
	}
	for _, want := range []string{"b.txt", "c.txt"} {
		if !strings.Contains(snap.PatchText, want) {
			t.Fatalf("patch missing %q:\n%s", want, snap.PatchText)
		}
	}
	if strings.Contains(snap.PatchText, "a.txt") {
		t.Fatalf("patch includes the base's own file:\n%s", snap.PatchText)
	}
}

func TestJJCaptureAt(t *testing.T) {
	requireJJ(t)
	dir := newJJRepo(t, false)
	write(t, dir, "a.txt", "a\n")
	jjRun(t, dir, "commit", "-m", "one")
	pin := strings.TrimSpace(jjRun(t, dir, "log", "--no-graph", "-r", "@-", "-T", "commit_id.shortest(12)"))
	write(t, dir, "b.txt", "b\n")
	jjRun(t, dir, "commit", "-m", "two")
	write(t, dir, "c.txt", "c\n")

	snap, err := CaptureAt(context.Background(), dir, pin)
	if err != nil {
		t.Fatalf("capture at %s: %v", pin, err)
	}
	if snap.BaseRef != pin {
		t.Fatalf("base = %q, want pinned %q", snap.BaseRef, pin)
	}
	for _, want := range []string{"b.txt", "c.txt"} {
		if !strings.Contains(snap.PatchText, want) {
			t.Fatalf("patch missing %q:\n%s", want, snap.PatchText)
		}
	}

	at := strings.TrimSpace(jjRun(t, dir, "log", "--no-graph", "-r", "@", "-T", "commit_id.shortest(12)"))
	if _, err := CaptureAt(context.Background(), dir, at); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges when the working copy matches the pin", err)
	}
}
