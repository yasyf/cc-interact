package vcs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// StackInfo identifies a Graphite stack: its trunk and the checked-out branch.
type StackInfo struct {
	Trunk  string `json:"trunk"`
	Branch string `json:"branch"`
}

// StackSnapshot is a whole Graphite stack captured as ordered per-branch
// sections, trunk-most first, with a trailing pending section for the
// uncommitted working tree when it is dirty.
type StackSnapshot struct {
	RepoRoot string         `json:"repo_root"`
	Trunk    string         `json:"trunk"`
	Branch   string         `json:"branch"` // the checked-out branch
	Sections []StackSection `json:"sections"`
}

// StackSection is one branch's diff against its parent's fork point. The pending
// section carries the uncommitted working tree: ParentBranch and HeadRef are
// empty, BaseRef is the checked-out branch's tip, and Pending is true.
type StackSection struct {
	Branch       string       `json:"branch"`
	ParentBranch string       `json:"parent_branch"`
	BaseRef      string       `json:"base_ref"`
	HeadRef      string       `json:"head_ref"`
	PatchText    string       `json:"patch_text"`
	Files        []FileChange `json:"files"`
	Pending      bool         `json:"pending"`
}

// branchMeta is the only branch_metadata column pair read from the graphite db.
// parent_branch_revision is a stale-able restack checkpoint and is deliberately
// not selected, so it can never be mistaken for a live diff base.
type branchMeta struct {
	ParentBranch string
}

// DetectStack reports whether cwd sits on a Graphite-tracked branch of a plain
// git repository. A jj-colocated repo (jj wins in detect), a missing config or
// metadata db, a detached or trunk checkout, or a branch with no metadata row
// are all "not stacked"; a malformed config or db is an error.
func DetectStack(ctx context.Context, cwd string) (StackInfo, bool, error) {
	kind, _, err := detect(cwd)
	if err != nil {
		return StackInfo{}, false, err
	}
	if kind != backendGit {
		return StackInfo{}, false, nil
	}
	trunk, ok, err := graphiteTrunk(ctx, cwd)
	if err != nil {
		return StackInfo{}, false, err
	}
	if !ok {
		return StackInfo{}, false, nil
	}
	_, branch, err := gitIdentity(ctx, cwd)
	if err != nil {
		return StackInfo{}, false, err
	}
	if branch == "" || branch == trunk {
		return StackInfo{}, false, nil
	}
	meta, present, err := graphiteMeta(ctx, cwd)
	if err != nil {
		return StackInfo{}, false, err
	}
	if !present {
		return StackInfo{}, false, nil
	}
	if _, ok := meta[branch]; !ok {
		return StackInfo{}, false, nil
	}
	return StackInfo{Trunk: trunk, Branch: branch}, true, nil
}

// CaptureStack snapshots the whole Graphite stack at cwd's checked-out branch
// (SCOPE.STACK: downstack to trunk, current, and every upstack descendant),
// each branch diffed against its parent's live fork point, plus a trailing
// pending section for a dirty working tree. Refs are re-resolved from the live
// stack and diffed as shas; an empty stack with a clean tree is ErrNoChanges.
func CaptureStack(ctx context.Context, cwd string) (StackSnapshot, error) {
	info, ok, err := DetectStack(ctx, cwd)
	if err != nil {
		return StackSnapshot{}, err
	}
	if !ok {
		return StackSnapshot{}, fmt.Errorf("capture stack in %s: not a graphite-stacked checkout", cwd)
	}
	repoRoot, err := gitRoot(ctx, cwd)
	if err != nil {
		return StackSnapshot{}, err
	}
	trunk, current := info.Trunk, info.Branch
	meta, _, err := graphiteMeta(ctx, cwd)
	if err != nil {
		return StackSnapshot{}, err
	}
	branches, err := stackBranches(meta, trunk, current)
	if err != nil {
		return StackSnapshot{}, err
	}

	tips := make(map[string]string, len(branches)+1)
	for _, b := range append([]string{trunk}, branches...) {
		out, err := git(ctx, cwd, nil, "rev-parse", "--verify", "refs/heads/"+b)
		if err != nil {
			return StackSnapshot{}, fmt.Errorf("resolve tip of %q: %w", b, err)
		}
		tips[b] = strings.TrimSpace(out)
	}

	sections := make([]StackSection, 0, len(branches)+1)
	anyNonEmpty := false
	for _, b := range branches {
		parent := meta[b].ParentBranch
		base, err := git(ctx, cwd, nil, "merge-base", tips[parent], tips[b])
		if err != nil {
			return StackSnapshot{}, fmt.Errorf("merge-base of %q and %q: %w", parent, b, err)
		}
		baseRef := strings.TrimSpace(base)
		patch, err := git(ctx, cwd, nil, "diff", "--no-color", "--no-ext-diff", baseRef, tips[b])
		if err != nil {
			return StackSnapshot{}, fmt.Errorf("diff %q against %q: %w", b, parent, err)
		}
		files, err := parseFiles(patch)
		if err != nil {
			return StackSnapshot{}, err
		}
		if strings.TrimSpace(patch) != "" {
			anyNonEmpty = true
		}
		sections = append(sections, StackSection{
			Branch:       b,
			ParentBranch: parent,
			BaseRef:      baseRef,
			HeadRef:      tips[b],
			PatchText:    patch,
			Files:        files,
		})
	}

	env, cleanup, err := gitStage(ctx, cwd)
	if err != nil {
		return StackSnapshot{}, err
	}
	defer cleanup()
	pending, err := git(ctx, cwd, env, "diff", "--cached", "--no-color", "--no-ext-diff", tips[current])
	if err != nil {
		return StackSnapshot{}, fmt.Errorf("diff working tree: %w", err)
	}
	if strings.TrimSpace(pending) != "" {
		files, err := parseFiles(pending)
		if err != nil {
			return StackSnapshot{}, err
		}
		sections = append(sections, StackSection{
			Branch:    current,
			BaseRef:   tips[current],
			PatchText: pending,
			Files:     files,
			Pending:   true,
		})
		anyNonEmpty = true
	}

	if !anyNonEmpty {
		return StackSnapshot{}, fmt.Errorf("%w: stack sections and working tree are all empty", ErrNoChanges)
	}
	return StackSnapshot{
		RepoRoot: repoRoot,
		Trunk:    trunk,
		Branch:   current,
		Sections: sections,
	}, nil
}

// graphiteCommonDir resolves the git common dir (shared across worktrees, unlike
// --git-path), where gt keeps .graphite_repo_config and .graphite_metadata.db.
func graphiteCommonDir(ctx context.Context, cwd string) (string, error) {
	out, err := git(ctx, cwd, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("locate git common dir: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// graphiteTrunk reads the trunk field of .graphite_repo_config. ok is false when
// the repo has no config or an empty trunk; a malformed config is an error. The
// gt 1.8.6 config also carries a trunks array — only trunk is read.
func graphiteTrunk(ctx context.Context, cwd string) (trunk string, ok bool, err error) {
	common, err := graphiteCommonDir(ctx, cwd)
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(common, ".graphite_repo_config")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg struct {
		Trunk string `json:"trunk"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg.Trunk, cfg.Trunk != "", nil
}

// graphiteMeta reads branch -> parent from .graphite_metadata.db. present is
// false when the db does not exist (not gt-tracked); a db that exists but can't
// be opened or queried is an error.
func graphiteMeta(ctx context.Context, cwd string) (meta map[string]branchMeta, present bool, err error) {
	common, err := graphiteCommonDir(ctx, cwd)
	if err != nil {
		return nil, false, err
	}
	dbPath := filepath.Join(common, ".graphite_metadata.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("stat %s: %w", dbPath, err)
	}
	m, err := readBranchMetadata(ctx, dbPath)
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
}

// readBranchMetadata opens the metadata db read-only and reads only
// branch_name/parent_branch_name, resilient to Kysely migrations adding columns.
// A busy_timeout rides out a transient write lock from a concurrent gt process;
// the trunk row (NULL parent) is skipped so it never enters the stack walk.
func readBranchMetadata(ctx context.Context, dbPath string) (map[string]branchMeta, error) {
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "SELECT branch_name, parent_branch_name FROM branch_metadata")
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", dbPath, err)
	}
	defer func() { _ = rows.Close() }()
	meta := map[string]branchMeta{}
	for rows.Next() {
		var branch string
		var parent sql.NullString
		if err := rows.Scan(&branch, &parent); err != nil {
			return nil, fmt.Errorf("scan %s: %w", dbPath, err)
		}
		if !parent.Valid {
			continue
		}
		meta[branch] = branchMeta{ParentBranch: parent.String}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", dbPath, err)
	}
	return meta, nil
}

// stackBranches orders the SCOPE.STACK branches trunk-most first: the downstack
// chain from current up to trunk, then current's upstack subtree in DFS
// preorder with children in branch-name order. Only the walked subtree is
// validated, so a stale ref elsewhere cannot brick capture; a cycle, an
// untracked branch, or a dangling parent fails loudly with the branch named.
func stackBranches(meta map[string]branchMeta, trunk, current string) ([]string, error) {
	visited := map[string]bool{}
	var chain []string
	for b := current; b != trunk; {
		if visited[b] {
			return nil, fmt.Errorf("branch metadata cycle at %q", b)
		}
		visited[b] = true
		m, ok := meta[b]
		if !ok {
			return nil, fmt.Errorf("branch %q is not tracked by graphite", b)
		}
		if m.ParentBranch == "" {
			return nil, fmt.Errorf("branch %q has no parent in its metadata", b)
		}
		chain = append(chain, b)
		b = m.ParentBranch
	}
	ordered := make([]string, 0, len(meta))
	for i := len(chain) - 1; i >= 0; i-- {
		ordered = append(ordered, chain[i])
	}
	desc, err := upstackBranches(meta, current, visited)
	if err != nil {
		return nil, err
	}
	return append(ordered, desc...), nil
}

// upstackBranches returns node's descendants in DFS preorder, children in
// branch-name order, sharing visited with the downstack walk so a cycle
// spanning the walked subtree is caught.
func upstackBranches(meta map[string]branchMeta, node string, visited map[string]bool) ([]string, error) {
	var children []string
	for b, m := range meta {
		if m.ParentBranch == node {
			children = append(children, b)
		}
	}
	sort.Strings(children)
	var out []string
	for _, c := range children {
		if visited[c] {
			return nil, fmt.Errorf("branch metadata cycle at %q", c)
		}
		visited[c] = true
		out = append(out, c)
		rest, err := upstackBranches(meta, c, visited)
		if err != nil {
			return nil, err
		}
		out = append(out, rest...)
	}
	return out, nil
}
