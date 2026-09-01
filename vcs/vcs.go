// Package vcs snapshots a repository's pending changes as a single git-format
// patch, detecting whether the working copy is managed by git or jj. When a
// repository is colocated (both .jj and .git), jj wins.
//
// vcs is an optional layer of cc-interact: a consumer that wants working-tree
// snapshots imports it, and one that wants the per-prompt turn ledger composes
// TurnsSchema into its daemon StoreSchema.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoChanges reports that capture found nothing: the diff against the chosen
// base (after the trunk fallback, when applicable) is empty.
var ErrNoChanges = errors.New("no changes to capture")

type backend int

const (
	backendGit backend = iota
	backendJJ
)

// FileChange is one file's status within a snapshot. Fingerprint identifies the
// file's diff content (see fingerprint) and is stable across no-op recaptures.
type FileChange struct {
	Path        string `json:"path"`
	OldPath     string `json:"old_path,omitempty"`
	Status      string `json:"status"` // A | M | D | R | C
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Snapshot is the result of snapshotting a working copy's pending changes.
type Snapshot struct {
	RepoRoot  string
	Branch    string // git: branch name, empty on detached HEAD; jj: nearest bookmark or change-id prefix
	BaseRef   string // resolved diff base: git full sha (or the empty-tree hash); jj 12-char commit id
	PatchText string
	Files     []FileChange
}

// Root resolves the repository root containing cwd, without taking a full
// snapshot and without spawning a subprocess. For a git working tree the
// result equals `git rev-parse --show-toplevel`, physical path and all; it
// diverges only where no working tree contains cwd — a bare repository, or a
// GIT_DIR/GIT_WORK_TREE override.
func Root(ctx context.Context, cwd string) (string, error) {
	kind, dir, err := detect(cwd)
	if err != nil {
		return "", err
	}
	if kind == backendJJ {
		return dir, nil
	}
	root, err := gitWorkTree(cwd)
	if err != nil {
		return "", err
	}
	if _, err := gitDir(root); err != nil {
		return "", err
	}
	return root, nil
}

// Backend names the version-control system managing cwd's repository, "git"
// or "jj", matching the tag SnapshotTree stamps on a TreeRef.
func Backend(cwd string) (string, error) {
	kind, _, err := detect(cwd)
	if err != nil {
		return "", err
	}
	if kind == backendJJ {
		return "jj", nil
	}
	if _, err := gitWorkTree(cwd); err != nil {
		return "", err
	}
	return "git", nil
}

// gitWorkTree walks up from the physically resolved cwd, the path git itself
// sees through getcwd, so a symlink escaping one repository into another
// answers with the repository holding the link's target.
func gitWorkTree(cwd string) (string, error) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", cwd, err)
	}
	_, dir, err := detect(resolved)
	if err != nil {
		return "", err
	}
	return dir, nil
}

// Capture snapshots cwd's pending changes. With base == "" the diff is
// session-scoped (git: working tree vs HEAD; jj: @ vs @-), falling back to a
// full-branch diff against the trunk's fork point when the session diff is
// empty. A non-empty base diffs the working copy against the fork point of base
// and the working copy instead. Snapshot.BaseRef is always a resolved commit
// id; an empty final diff is ErrNoChanges.
func Capture(ctx context.Context, cwd, base string) (Snapshot, error) {
	kind, dir, err := detect(cwd)
	if err != nil {
		return Snapshot{}, err
	}
	if kind == backendJJ {
		return jjCapture(ctx, cwd, dir, base)
	}
	return gitCapture(ctx, cwd, base)
}

// CaptureAt snapshots cwd's pending changes against a pinned base commit,
// verbatim — no fork-point re-resolution (which would silently move the base
// after a rebase) and no trunk fallback. An empty diff is ErrNoChanges.
func CaptureAt(ctx context.Context, cwd, base string) (Snapshot, error) {
	kind, dir, err := detect(cwd)
	if err != nil {
		return Snapshot{}, err
	}
	if kind == backendJJ {
		return jjCaptureAt(ctx, cwd, dir, base)
	}
	return gitCaptureAt(ctx, cwd, base)
}

// detect walks upward from cwd without spawning a subprocess: Root sits on a
// hot poll path. A .git entry may be a file (worktrees), but .jj is only ever a
// directory.
func detect(cwd string) (backend, string, error) {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return 0, "", fmt.Errorf("resolve %s: %w", cwd, err)
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".jj")); err == nil && fi.IsDir() {
			return backendJJ, dir, nil
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return backendGit, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, "", errors.New("not inside a git or jj repository")
		}
		dir = parent
	}
}
