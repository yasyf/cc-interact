package vcs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TreeRef identifies one turn-boundary working-tree snapshot: a git tree OID
// or a jj commit id, tagged with the backend that produced it.
type TreeRef struct {
	Backend string
	OID     string
}

// TreeDiffer produces a git-format patch between two snapshot OIDs.
type TreeDiffer interface {
	Diff(ctx context.Context, fromOID, toOID string) (string, error)
}

// SnapshotTree captures the working tree at a turn boundary without touching
// the repository: git stages into a scratch index and writes objects into a
// scratch object directory under scratchDir; jj relies on its implicit
// working-copy snapshot. Backend selection matches Capture — jj wins on
// colocated repos.
func SnapshotTree(ctx context.Context, repoRoot, scratchDir string) (TreeRef, error) {
	kind, _, err := detect(repoRoot)
	if err != nil {
		return TreeRef{}, err
	}
	if kind == backendJJ {
		return jjSnapshotTree(ctx, repoRoot)
	}
	return gitSnapshotTree(ctx, repoRoot, scratchDir)
}

func NewTreeDiffer(repoRoot, scratchDir, backend string) TreeDiffer {
	switch backend {
	case "jj":
		return jjTreeDiffer{repoRoot: repoRoot}
	case "git":
		return gitTreeDiffer{repoRoot: repoRoot, scratchDir: scratchDir}
	}
	panic(fmt.Sprintf("unknown vcs backend %q", backend))
}

type gitScratch struct {
	repoRoot string
	index    string
	env      []string
}

func newGitScratch(ctx context.Context, repoRoot, scratchDir string) (gitScratch, error) {
	objects := filepath.Join(scratchDir, "objects")
	if err := os.MkdirAll(objects, 0o755); err != nil {
		return gitScratch{}, fmt.Errorf("create scratch objects dir: %w", err)
	}
	repoObjects, ok := repoObjectsDir(repoRoot)
	if !ok {
		out, err := git(ctx, repoRoot, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects")
		if err != nil {
			return gitScratch{}, fmt.Errorf("locate repo objects: %w", err)
		}
		repoObjects = strings.TrimSpace(out)
	}
	index := filepath.Join(scratchDir, "index")
	return gitScratch{
		repoRoot: repoRoot,
		index:    index,
		env: []string{
			"GIT_INDEX_FILE=" + index,
			"GIT_OBJECT_DIRECTORY=" + objects,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + repoObjects,
		},
	}, nil
}

// repoObjectsDir reads the repository's object store off the filesystem,
// following a worktree or submodule gitfile and then its commondir, so the
// warm snapshot path spends no exec on `rev-parse --git-path objects`. ok is
// false when the layout yields no directory and only git can answer.
func repoObjectsDir(repoRoot string) (string, bool) {
	gitPath := filepath.Join(repoRoot, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return "", false
	}
	gitDir := gitPath
	if !fi.IsDir() {
		data, err := os.ReadFile(gitPath)
		if err != nil {
			return "", false
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !ok {
			return "", false
		}
		gitDir = resolveAgainst(repoRoot, strings.TrimSpace(target))
	}
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		gitDir = resolveAgainst(gitDir, strings.TrimSpace(string(data)))
	}
	objects := filepath.Join(gitDir, "objects")
	if fi, err := os.Stat(objects); err != nil || !fi.IsDir() {
		return "", false
	}
	return objects, true
}

func resolveAgainst(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}

// seedIndex copies the repo's real index into the scratch index so the first
// `add -A` only hashes what differs from HEAD; an existing scratch index is
// kept, and an index-less repo starts from an empty one.
func (s gitScratch) seedIndex(ctx context.Context) error {
	if _, err := os.Stat(s.index); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat scratch index: %w", err)
	}
	out, err := git(ctx, s.repoRoot, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return fmt.Errorf("locate repo index: %w", err)
	}
	data, err := os.ReadFile(strings.TrimSpace(out))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read repo index: %w", err)
	}
	if err := os.WriteFile(s.index, data, 0o644); err != nil {
		return fmt.Errorf("seed scratch index: %w", err)
	}
	return nil
}

func (s gitScratch) writeTree(ctx context.Context) (string, error) {
	if _, err := git(ctx, s.repoRoot, s.env, "add", "-A"); err != nil {
		return "", fmt.Errorf("stage working tree: %w", err)
	}
	out, err := git(ctx, s.repoRoot, s.env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func gitSnapshotTree(ctx context.Context, repoRoot, scratchDir string) (TreeRef, error) {
	scratch, err := newGitScratch(ctx, repoRoot, scratchDir)
	if err != nil {
		return TreeRef{}, err
	}
	if err := scratch.seedIndex(ctx); err != nil {
		return TreeRef{}, err
	}
	oid, err := scratch.writeTree(ctx)
	if err != nil {
		// A corrupt or locked scratch index is recoverable: rebuild it from the
		// repo's real index and retry once.
		os.Remove(scratch.index)
		os.Remove(scratch.index + ".lock")
		if err := scratch.seedIndex(ctx); err != nil {
			return TreeRef{}, err
		}
		if oid, err = scratch.writeTree(ctx); err != nil {
			return TreeRef{}, fmt.Errorf("snapshot tree after index reseed: %w", err)
		}
	}
	return TreeRef{Backend: "git", OID: oid}, nil
}

type gitTreeDiffer struct {
	repoRoot   string
	scratchDir string
}

func (d gitTreeDiffer) Diff(ctx context.Context, fromOID, toOID string) (string, error) {
	scratch, err := newGitScratch(ctx, d.repoRoot, d.scratchDir)
	if err != nil {
		return "", err
	}
	out, err := git(ctx, d.repoRoot, scratch.env, "diff-tree", "-r", "-M", "--no-color", "--patch", fromOID, toOID)
	if err != nil {
		return "", fmt.Errorf("diff trees %s..%s: %w", fromOID, toOID, err)
	}
	return out, nil
}

// jjSnapshotTree runs jj for its side effect — every invocation snapshots the
// working copy into @ — and reads the resulting commit id in the same call.
func jjSnapshotTree(ctx context.Context, repoRoot string) (TreeRef, error) {
	out, err := jj(ctx, repoRoot, "log", "--no-graph", "-r", "@", "-T", "commit_id")
	if err != nil {
		return TreeRef{}, fmt.Errorf("snapshot working copy: %w", err)
	}
	return TreeRef{Backend: "jj", OID: strings.TrimSpace(out)}, nil
}

type jjTreeDiffer struct {
	repoRoot string
}

func (d jjTreeDiffer) Diff(ctx context.Context, fromOID, toOID string) (string, error) {
	out, err := jj(ctx, d.repoRoot, "diff", "--from", fromOID, "--to", toOID, "--git")
	if err != nil {
		return "", fmt.Errorf("diff %s..%s: %w", fromOID, toOID, err)
	}
	return out, nil
}
