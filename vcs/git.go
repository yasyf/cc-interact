package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// emptyTree is git's well-known hash of the empty tree, used as the diff base
// when the repository has no commits.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func gitRoot(ctx context.Context, cwd string) (string, error) {
	out, err := git(ctx, cwd, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve repo root: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// gitCapture snapshots the working tree (tracked + staged + untracked, minus
// ignored). baseRef == "" diffs against HEAD — or the empty tree when there is
// no commit yet — falling back to the trunk fork point when that diff is empty;
// a non-empty baseRef diffs against merge-base(baseRef, HEAD).
func gitCapture(ctx context.Context, cwd, baseRef string) (Snapshot, error) {
	repoRoot, branch, err := gitIdentity(ctx, cwd)
	if err != nil {
		return Snapshot{}, err
	}

	hasHEAD := false
	if _, err := git(ctx, cwd, nil, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		hasHEAD = true
	}
	base := emptyTree
	switch {
	case baseRef != "":
		if base, err = gitMergeBase(ctx, cwd, baseRef); err != nil {
			return Snapshot{}, err
		}
	case hasHEAD:
		out, err := git(ctx, cwd, nil, "rev-parse", "HEAD")
		if err != nil {
			return Snapshot{}, fmt.Errorf("resolve HEAD: %w", err)
		}
		base = strings.TrimSpace(out)
	}

	env, cleanup, err := gitStage(ctx, cwd)
	if err != nil {
		return Snapshot{}, err
	}
	defer cleanup()

	patch, err := git(ctx, cwd, env, "diff", "--cached", "--no-color", "--no-ext-diff", base)
	if err != nil {
		return Snapshot{}, fmt.Errorf("diff working tree: %w", err)
	}
	if strings.TrimSpace(patch) == "" {
		switch {
		case baseRef != "":
			return Snapshot{}, fmt.Errorf("%w: working tree matches the fork point of %s", ErrNoChanges, baseRef)
		case !hasHEAD:
			return Snapshot{}, fmt.Errorf("%w: empty repository", ErrNoChanges)
		}
		trunk, ref, err := gitTrunk(ctx, cwd)
		if err != nil {
			return Snapshot{}, err
		}
		if base, err = gitMergeBase(ctx, cwd, ref); err != nil {
			return Snapshot{}, err
		}
		// The throwaway index is already staged; only the base changes.
		if patch, err = git(ctx, cwd, env, "diff", "--cached", "--no-color", "--no-ext-diff", base); err != nil {
			return Snapshot{}, fmt.Errorf("diff against trunk %s: %w", trunk, err)
		}
		if strings.TrimSpace(patch) == "" {
			return Snapshot{}, fmt.Errorf("%w: working tree matches HEAD and trunk %s", ErrNoChanges, trunk)
		}
	}

	return gitSnapshot(repoRoot, branch, base, patch)
}

// gitCaptureAt diffs the working tree against a pinned base commit, verbatim.
func gitCaptureAt(ctx context.Context, cwd, base string) (Snapshot, error) {
	repoRoot, branch, err := gitIdentity(ctx, cwd)
	if err != nil {
		return Snapshot{}, err
	}
	env, cleanup, err := gitStage(ctx, cwd)
	if err != nil {
		return Snapshot{}, err
	}
	defer cleanup()
	patch, err := git(ctx, cwd, env, "diff", "--cached", "--no-color", "--no-ext-diff", base)
	if err != nil {
		return Snapshot{}, fmt.Errorf("diff against base %s: %w", base, err)
	}
	if strings.TrimSpace(patch) == "" {
		return Snapshot{}, fmt.Errorf("%w: working tree matches base %s", ErrNoChanges, base)
	}
	return gitSnapshot(repoRoot, branch, base, patch)
}

func gitIdentity(ctx context.Context, cwd string) (repoRoot, branch string, err error) {
	if repoRoot, err = gitRoot(ctx, cwd); err != nil {
		return "", "", err
	}
	// symbolic-ref -q exits non-zero with empty output on a detached HEAD; that
	// is a valid state, so a non-empty error here just means "no branch".
	branch, _ = git(ctx, cwd, nil, "symbolic-ref", "--short", "-q", "HEAD")
	return repoRoot, strings.TrimSpace(branch), nil
}

// gitStage stages the entire working tree (tracked changes, deletions, and
// untracked non-ignored files) into a throwaway index via GIT_INDEX_FILE,
// never mutating the caller's real index.
func gitStage(ctx context.Context, cwd string) (env []string, cleanup func(), err error) {
	tmpIndex, err := os.CreateTemp("", "cc-interact-index-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp index: %w", err)
	}
	tmpPath := tmpIndex.Name()
	tmpIndex.Close()
	os.Remove(tmpPath) // git wants to create it itself; we just reserved the name
	// git writes <index>.lock during `add`; clean both so a cancelled git leaves nothing.
	cleanup = func() {
		os.Remove(tmpPath)
		os.Remove(tmpPath + ".lock")
	}

	absIndex, err := filepath.Abs(tmpPath)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	env = []string{"GIT_INDEX_FILE=" + absIndex}
	if _, err := git(ctx, cwd, env, "add", "-A"); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("stage working tree: %w", err)
	}
	return env, cleanup, nil
}

func gitMergeBase(ctx context.Context, cwd, ref string) (string, error) {
	out, err := git(ctx, cwd, nil, "merge-base", ref, "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve merge-base of %s and HEAD: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

// gitTrunk returns the first existing trunk candidate as (display name, full
// ref); full refs dodge ref/path ambiguity.
func gitTrunk(ctx context.Context, cwd string) (string, string, error) {
	for _, t := range []struct{ name, ref string }{
		{"main", "refs/heads/main"},
		{"master", "refs/heads/master"},
		{"origin/main", "refs/remotes/origin/main"},
		{"origin/master", "refs/remotes/origin/master"},
	} {
		if _, err := git(ctx, cwd, nil, "rev-parse", "--verify", "-q", t.ref); err == nil {
			return t.name, t.ref, nil
		}
	}
	return "", "", fmt.Errorf("no uncommitted changes and no trunk branch found (tried main, master, origin/main, origin/master); pass --base <ref>")
}

func gitSnapshot(repoRoot, branch, base, patch string) (Snapshot, error) {
	files, err := parseFiles(patch)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		RepoRoot:  repoRoot,
		Branch:    branch,
		BaseRef:   base,
		PatchText: patch,
		Files:     files,
	}, nil
}

func git(ctx context.Context, cwd string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	if extraEnv != nil {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
