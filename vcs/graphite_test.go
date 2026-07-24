package vcs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// graphiteDDL mirrors the real gt 1.8.6 branch_metadata table, full column set
// included, so the read path is exercised against columns it never selects.
const graphiteDDL = `CREATE TABLE IF NOT EXISTS branch_metadata (
	branch_name TEXT PRIMARY KEY,
	parent_branch_name TEXT,
	parent_branch_revision TEXT,
	last_submitted_version TEXT,
	state TEXT,
	children TEXT,
	branch_revision TEXT,
	validation_result TEXT,
	parent_head_revision TEXT
)`

func writeGraphiteConfig(t *testing.T, dir, trunk string) {
	t.Helper()
	common := strings.TrimSpace(gitInit(t, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	body := fmt.Sprintf(`{"trunk":%q,"trunks":["decoy-should-be-ignored"]}`, trunk)
	if err := os.WriteFile(filepath.Join(common, ".graphite_repo_config"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func metaDBPath(t *testing.T, dir string) string {
	t.Helper()
	common := strings.TrimSpace(gitInit(t, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	return filepath.Join(common, ".graphite_metadata.db")
}

func openMetaDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", metaDBPath(t, dir))
	if err != nil {
		t.Fatalf("open metadata db: %v", err)
	}
	if _, err := db.Exec(graphiteDDL); err != nil {
		_ = db.Close()
		t.Fatalf("create branch_metadata: %v", err)
	}
	return db
}

func setTrunkMeta(t *testing.T, dir, trunk string) {
	t.Helper()
	db := openMetaDB(t, dir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO branch_metadata (branch_name, parent_branch_name, validation_result) VALUES (?, NULL, 'TRUNK')`,
		trunk,
	); err != nil {
		t.Fatalf("insert trunk metadata: %v", err)
	}
}

// setBranchMeta records a stack row the way gt does, with parent_branch_revision
// set to the parent's real tip at track-time — the code must ignore that column.
func setBranchMeta(t *testing.T, dir, branch, parent string) {
	t.Helper()
	rev := strings.TrimSpace(gitInit(t, dir, "rev-parse", "refs/heads/"+parent))
	db := openMetaDB(t, dir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO branch_metadata
		 (branch_name, parent_branch_name, parent_branch_revision, children, validation_result)
		 VALUES (?, ?, ?, '[]', 'VALID')`,
		branch, parent, rev,
	); err != nil {
		t.Fatalf("insert branch metadata: %v", err)
	}
}

func branchCommit(t *testing.T, dir, branch, parent, file, content string) {
	t.Helper()
	gitInit(t, dir, "checkout", "-qb", branch, parent)
	write(t, dir, file, content)
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", branch)
	setBranchMeta(t, dir, branch, parent)
}

func branchOnly(t *testing.T, dir, branch, parent string) {
	t.Helper()
	gitInit(t, dir, "checkout", "-qb", branch, parent)
	setBranchMeta(t, dir, branch, parent)
}

// stackTrunk builds a repo with a committed main, a graphite config naming it
// trunk, and a trunk row in the metadata db; callers stack branches on top.
func stackTrunk(t *testing.T) string {
	t.Helper()
	dir := newRepo(t)
	write(t, dir, "base.txt", "0\n")
	gitInit(t, dir, "add", "-A")
	gitInit(t, dir, "commit", "-qm", "init")
	writeGraphiteConfig(t, dir, "main")
	setTrunkMeta(t, dir, "main")
	return dir
}

func linearStack(t *testing.T) string {
	t.Helper()
	dir := stackTrunk(t)
	branchCommit(t, dir, "a", "main", "a.txt", "a\n")
	branchCommit(t, dir, "b", "a", "b.txt", "b\n")
	branchCommit(t, dir, "c", "b", "c.txt", "c\n")
	return dir
}

func branchOrder(snap StackSnapshot) []string {
	out := make([]string, 0, len(snap.Sections))
	for _, s := range snap.Sections {
		out = append(out, s.Branch)
	}
	return out
}

func equalOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDetectStack(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) string
		wantOK  bool
		wantErr bool
		trunk   string
		branch  string
	}{
		{
			name: "tracked branch is stacked",
			setup: func(t *testing.T) string {
				dir := stackTrunk(t)
				branchCommit(t, dir, "feat", "main", "f.txt", "f\n")
				return dir
			},
			wantOK: true, trunk: "main", branch: "feat",
		},
		{
			name: "no graphite config",
			setup: func(t *testing.T) string {
				dir := newRepo(t)
				write(t, dir, "a.txt", "a\n")
				gitInit(t, dir, "add", "-A")
				gitInit(t, dir, "commit", "-qm", "init")
				return dir
			},
		},
		{
			name:  "on trunk",
			setup: func(t *testing.T) string { return stackTrunk(t) },
		},
		{
			name: "detached head",
			setup: func(t *testing.T) string {
				dir := stackTrunk(t)
				branchCommit(t, dir, "feat", "main", "f.txt", "f\n")
				sha := strings.TrimSpace(gitInit(t, dir, "rev-parse", "HEAD"))
				gitInit(t, dir, "checkout", "-q", sha)
				return dir
			},
		},
		{
			name: "config but no metadata db",
			setup: func(t *testing.T) string {
				dir := newRepo(t)
				write(t, dir, "base.txt", "0\n")
				gitInit(t, dir, "add", "-A")
				gitInit(t, dir, "commit", "-qm", "init")
				writeGraphiteConfig(t, dir, "main")
				gitInit(t, dir, "checkout", "-qb", "feat", "main")
				write(t, dir, "f.txt", "f\n")
				gitInit(t, dir, "add", "-A")
				gitInit(t, dir, "commit", "-qm", "feat")
				return dir
			},
		},
		{
			name: "db present but branch untracked",
			setup: func(t *testing.T) string {
				dir := stackTrunk(t)
				branchCommit(t, dir, "tracked", "main", "t.txt", "t\n")
				gitInit(t, dir, "checkout", "-qb", "untracked", "main")
				write(t, dir, "u.txt", "u\n")
				gitInit(t, dir, "add", "-A")
				gitInit(t, dir, "commit", "-qm", "untracked")
				return dir
			},
		},
		{
			name: "colocated jj wins",
			setup: func(t *testing.T) string {
				dir := stackTrunk(t)
				branchCommit(t, dir, "feat", "main", "f.txt", "f\n")
				if err := os.Mkdir(filepath.Join(dir, ".jj"), 0o755); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			name: "corrupt config",
			setup: func(t *testing.T) string {
				dir := stackTrunk(t)
				branchCommit(t, dir, "feat", "main", "f.txt", "f\n")
				common := strings.TrimSpace(gitInit(t, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"))
				if err := os.WriteFile(filepath.Join(common, ".graphite_repo_config"), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantErr: true,
		},
		{
			name: "garbage metadata db",
			setup: func(t *testing.T) string {
				dir := stackTrunk(t)
				branchCommit(t, dir, "feat", "main", "f.txt", "f\n")
				if err := os.WriteFile(metaDBPath(t, dir), []byte("not a sqlite database"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			info, ok, err := DetectStack(context.Background(), dir)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("detect: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (info.Trunk != tc.trunk || info.Branch != tc.branch) {
				t.Fatalf("info = %+v, want trunk=%q branch=%q", info, tc.trunk, tc.branch)
			}
		})
	}
}

func TestCaptureStackOrder(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) string
		want  []string
	}{
		{
			name:  "linear stack trunk-most first",
			setup: linearStack,
			want:  []string{"a", "b", "c"},
		},
		{
			name: "mid-stack checkout spans full stack",
			setup: func(t *testing.T) string {
				dir := linearStack(t)
				gitInit(t, dir, "checkout", "-q", "b")
				return dir
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "upstack fork DFS preorder",
			setup: func(t *testing.T) string {
				dir := stackTrunk(t)
				branchCommit(t, dir, "a", "main", "a.txt", "a\n")
				branchCommit(t, dir, "b1", "a", "b1.txt", "b1\n")
				branchCommit(t, dir, "c", "b1", "c.txt", "c\n")
				branchCommit(t, dir, "b2", "a", "b2.txt", "b2\n")
				gitInit(t, dir, "checkout", "-q", "a")
				return dir
			},
			want: []string{"a", "b1", "c", "b2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			snap, err := CaptureStack(context.Background(), dir)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if got := branchOrder(snap); !equalOrder(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
			for _, s := range snap.Sections {
				if s.Pending {
					t.Fatalf("unexpected pending section on a clean tree: %+v", s)
				}
			}
		})
	}
}

func TestCaptureStackSections(t *testing.T) {
	dir := linearStack(t)
	snap, err := CaptureStack(context.Background(), dir)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.Sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(snap.Sections))
	}
	if snap.Trunk != "main" || snap.Branch != "c" {
		t.Fatalf("snapshot trunk/branch = %q/%q, want main/c", snap.Trunk, snap.Branch)
	}
	tip := func(ref string) string {
		return strings.TrimSpace(gitInit(t, dir, "rev-parse", "refs/heads/"+ref))
	}
	byBranch := map[string]StackSection{}
	for _, s := range snap.Sections {
		byBranch[s.Branch] = s
	}
	// BaseRef is the live merge-base (parent tip here), proving the stored
	// parent_branch_revision is ignored.
	wants := []struct {
		branch, parent, base, head, wantFile, notFile string
	}{
		{"a", "main", tip("main"), tip("a"), "a.txt", "b.txt"},
		{"b", "a", tip("a"), tip("b"), "b.txt", "a.txt"},
		{"c", "b", tip("b"), tip("c"), "c.txt", "b.txt"},
	}
	for _, w := range wants {
		s := byBranch[w.branch]
		if s.ParentBranch != w.parent || s.BaseRef != w.base || s.HeadRef != w.head {
			t.Fatalf("section %s = {parent %q base %q head %q}, want {%q %q %q}",
				w.branch, s.ParentBranch, s.BaseRef, s.HeadRef, w.parent, w.base, w.head)
		}
		if s.Pending {
			t.Fatalf("section %s should not be pending", w.branch)
		}
		if !strings.Contains(s.PatchText, w.wantFile) || strings.Contains(s.PatchText, w.notFile) {
			t.Fatalf("section %s patch wrong (want %q, not %q):\n%s", w.branch, w.wantFile, w.notFile, s.PatchText)
		}
		if len(s.Files) != 1 || s.Files[0].Path != w.wantFile || s.Files[0].Status != "A" {
			t.Fatalf("section %s files = %+v, want one added %s", w.branch, s.Files, w.wantFile)
		}
	}
}

func TestCaptureStackDirtyTree(t *testing.T) {
	dir := linearStack(t)
	write(t, dir, "c.txt", "c\nmore\n")
	write(t, dir, "extra.txt", "brand new\n")

	snap, err := CaptureStack(context.Background(), dir)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(snap.Sections) != 4 {
		t.Fatalf("sections = %d, want 3 stack + 1 pending", len(snap.Sections))
	}
	last := snap.Sections[len(snap.Sections)-1]
	if !last.Pending || last.ParentBranch != "" || last.HeadRef != "" {
		t.Fatalf("pending section = %+v, want pending with empty parent/head", last)
	}
	if last.Branch != "c" {
		t.Fatalf("pending branch = %q, want the checked-out branch c", last.Branch)
	}
	if cTip := strings.TrimSpace(gitInit(t, dir, "rev-parse", "refs/heads/c")); last.BaseRef != cTip {
		t.Fatalf("pending base = %q, want current tip %q", last.BaseRef, cTip)
	}
	for _, want := range []string{"extra.txt", "more"} {
		if !strings.Contains(last.PatchText, want) {
			t.Fatalf("pending patch missing %q:\n%s", want, last.PatchText)
		}
	}
	// The real index is a throwaway; extra.txt stays untracked.
	if status := gitInit(t, dir, "status", "--porcelain"); !strings.Contains(status, "?? extra.txt") {
		t.Fatalf("extra.txt should still be untracked:\n%s", status)
	}
}

func TestCaptureStackNoChanges(t *testing.T) {
	dir := stackTrunk(t)
	branchOnly(t, dir, "a", "main")
	if _, err := CaptureStack(context.Background(), dir); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges on an empty stack with a clean tree", err)
	}
}

func TestCaptureStackEmptyMidSection(t *testing.T) {
	dir := stackTrunk(t)
	branchCommit(t, dir, "a", "main", "a.txt", "a\n")
	branchOnly(t, dir, "b", "a") // b == a: an empty mid-stack section
	branchCommit(t, dir, "c", "b", "c.txt", "c\n")

	snap, err := CaptureStack(context.Background(), dir)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if got := branchOrder(snap); !equalOrder(got, []string{"a", "b", "c"}) {
		t.Fatalf("order = %v, want a b c", got)
	}
	var b StackSection
	for _, s := range snap.Sections {
		if s.Branch == "b" {
			b = s
		}
	}
	if b.PatchText != "" {
		t.Fatalf("empty section b should have no patch:\n%s", b.PatchText)
	}
	if b.Files == nil || len(b.Files) != 0 {
		t.Fatalf("empty section files = %+v, want a non-nil empty slice", b.Files)
	}
}

func TestCaptureStackIgnoresStaleParentRevision(t *testing.T) {
	dir := stackTrunk(t)
	branchCommit(t, dir, "a", "main", "a.txt", "a\n")
	// A bogus checkpoint must not become the diff base — capture recomputes it.
	db := openMetaDB(t, dir)
	if _, err := db.Exec(`UPDATE branch_metadata SET parent_branch_revision='deadbeef' WHERE branch_name='a'`); err != nil {
		t.Fatalf("update: %v", err)
	}
	_ = db.Close()

	snap, err := CaptureStack(context.Background(), dir)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	mainTip := strings.TrimSpace(gitInit(t, dir, "rev-parse", "refs/heads/main"))
	if snap.Sections[0].BaseRef != mainTip {
		t.Fatalf("base = %q, want live merge-base %q; stale parent_branch_revision must be ignored", snap.Sections[0].BaseRef, mainTip)
	}
}

func TestCaptureStackDanglingParent(t *testing.T) {
	dir := stackTrunk(t)
	branchCommit(t, dir, "a", "main", "a.txt", "a\n")
	// Repoint a's parent at a branch with no metadata row and no ref.
	db := openMetaDB(t, dir)
	if _, err := db.Exec(`UPDATE branch_metadata SET parent_branch_name='ghost' WHERE branch_name='a'`); err != nil {
		t.Fatalf("update: %v", err)
	}
	_ = db.Close()

	_, err := CaptureStack(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want it to name the dangling parent ghost", err)
	}
}

func TestCaptureStackDeletedChildRef(t *testing.T) {
	dir := stackTrunk(t)
	branchCommit(t, dir, "a", "main", "a.txt", "a\n")
	branchCommit(t, dir, "b", "a", "b.txt", "b\n")
	gitInit(t, dir, "checkout", "-q", "a")
	gitInit(t, dir, "branch", "-qD", "b") // metadata row stays, branch ref gone

	_, err := CaptureStack(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), `resolve tip of "b"`) {
		t.Fatalf("err = %v, want a tip-resolution error naming b", err)
	}
}

func TestCaptureStackPatchStableAcrossReword(t *testing.T) {
	dir := linearStack(t)
	before, err := CaptureStack(context.Background(), dir)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	cTipBefore := strings.TrimSpace(gitInit(t, dir, "rev-parse", "refs/heads/c"))
	gitInit(t, dir, "commit", "--amend", "-qm", "reworded c") // new sha, identical tree
	cTipAfter := strings.TrimSpace(gitInit(t, dir, "rev-parse", "refs/heads/c"))
	if cTipBefore == cTipAfter {
		t.Fatal("amend did not change c's sha")
	}

	after, err := CaptureStack(context.Background(), dir)
	if err != nil {
		t.Fatalf("recapture: %v", err)
	}
	if len(before.Sections) != len(after.Sections) {
		t.Fatalf("section count changed: %d vs %d", len(before.Sections), len(after.Sections))
	}
	for i := range before.Sections {
		if before.Sections[i].PatchText != after.Sections[i].PatchText {
			t.Fatalf("section %d (%s) patch drifted across a reword-only amend:\n--- before\n%s\n--- after\n%s",
				i, before.Sections[i].Branch, before.Sections[i].PatchText, after.Sections[i].PatchText)
		}
	}
	if head := after.Sections[len(after.Sections)-1].HeadRef; head != cTipAfter {
		t.Fatalf("section c head = %q, want the reworded sha %q", head, cTipAfter)
	}
}

func TestCaptureStackOnTrunkErrors(t *testing.T) {
	dir := stackTrunk(t)
	branchCommit(t, dir, "a", "main", "a.txt", "a\n")
	gitInit(t, dir, "checkout", "-q", "main")

	_, err := CaptureStack(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "not a graphite-stacked checkout") {
		t.Fatalf("err = %v, want a not-a-graphite-stacked-checkout error on trunk", err)
	}
}

func TestCaptureStackSlashBranch(t *testing.T) {
	dir := stackTrunk(t)
	branchCommit(t, dir, "feature/a", "main", "a.txt", "a\n")
	branchCommit(t, dir, "feature/b", "feature/a", "b.txt", "b\n")

	snap, err := CaptureStack(context.Background(), dir)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if got := branchOrder(snap); !equalOrder(got, []string{"feature/a", "feature/b"}) {
		t.Fatalf("order = %v, want feature/a feature/b", got)
	}
	byBranch := map[string]StackSection{}
	for _, s := range snap.Sections {
		byBranch[s.Branch] = s
	}
	fa, fb := byBranch["feature/a"], byBranch["feature/b"]
	aTip := strings.TrimSpace(gitInit(t, dir, "rev-parse", "refs/heads/feature/a"))
	if fa.ParentBranch != "main" || fa.HeadRef != aTip {
		t.Fatalf("section feature/a = %+v, want parent main head %s", fa, aTip)
	}
	if !strings.Contains(fa.PatchText, "a.txt") {
		t.Fatalf("section feature/a patch missing a.txt:\n%s", fa.PatchText)
	}
	if fb.ParentBranch != "feature/a" {
		t.Fatalf("section feature/b parent = %q, want feature/a", fb.ParentBranch)
	}
	if !strings.Contains(fb.PatchText, "b.txt") {
		t.Fatalf("section feature/b patch missing b.txt:\n%s", fb.PatchText)
	}
}

func TestReadBranchMetadataWaitsForWriteLock(t *testing.T) {
	dir := stackTrunk(t)
	branchCommit(t, dir, "a", "main", "a.txt", "a\n")
	dbPath := metaDBPath(t, dir)

	writer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = writer.Close() }()
	ctx := context.Background()
	conn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatalf("writer conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	// An EXCLUSIVE lock blocks the read-only reader until it is released.
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		_, _ = conn.ExecContext(ctx, "COMMIT")
	}()

	// Without busy_timeout on the read DSN this returns "database is locked"
	// instantly; with it, the read waits out the lock and succeeds.
	meta, err := readBranchMetadata(ctx, dbPath)
	if err != nil {
		t.Fatalf("read under a held write lock: %v", err)
	}
	if _, ok := meta["a"]; !ok {
		t.Fatalf("meta = %+v, want branch a", meta)
	}
}

func TestStackBranches(t *testing.T) {
	m := func(pairs ...string) map[string]branchMeta {
		out := map[string]branchMeta{}
		for i := 0; i < len(pairs); i += 2 {
			out[pairs[i]] = branchMeta{ParentBranch: pairs[i+1]}
		}
		return out
	}
	cases := []struct {
		name    string
		meta    map[string]branchMeta
		trunk   string
		current string
		want    []string
		wantErr string
	}{
		{name: "single branch off trunk", meta: m("a", "main"), trunk: "main", current: "a", want: []string{"a"}},
		{name: "linear", meta: m("a", "main", "b", "a", "c", "b"), trunk: "main", current: "c", want: []string{"a", "b", "c"}},
		{name: "mid-stack full span", meta: m("a", "main", "b", "a", "c", "b"), trunk: "main", current: "b", want: []string{"a", "b", "c"}},
		{name: "fork preorder", meta: m("a", "main", "b1", "a", "b2", "a", "c", "b1"), trunk: "main", current: "a", want: []string{"a", "b1", "c", "b2"}},
		{name: "cycle", meta: m("a", "b", "b", "a"), trunk: "main", current: "a", wantErr: "cycle"},
		{name: "dangling parent", meta: m("a", "ghost"), trunk: "main", current: "a", wantErr: "not tracked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stackBranches(tc.meta, tc.trunk, tc.current)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("stackBranches: %v", err)
			}
			if !equalOrder(got, tc.want) {
				t.Fatalf("branches = %v, want %v", got, tc.want)
			}
		})
	}
}
