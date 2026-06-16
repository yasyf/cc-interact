package vcs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// jjCapture diffs the working-copy commit against its parents; the implicit
// working-copy snapshot that jj takes on every invocation is exactly the
// "uncommitted changes" we want, so no --ignore-working-copy. A non-empty
// baseRef diffs from the fork point of baseRef and @ instead; an empty session
// diff falls back to the trunk fork point.
func jjCapture(ctx context.Context, cwd, repoRoot, baseRef string) (Snapshot, error) {
	branch, err := jjBranch(ctx, cwd)
	if err != nil {
		return Snapshot{}, err
	}
	if baseRef != "" {
		return jjCaptureFrom(ctx, cwd, repoRoot, branch, jjForkPoint(baseRef), "the fork point of "+baseRef)
	}

	// A merge working copy has multiple parents; the first is the base. The
	// pinned base must reproduce this exact diff on resume, so the diff runs
	// from the resolved id, not the @- revset.
	baseOut, err := jj(ctx, cwd, "log", "--no-graph", "-r", "@-", "-T", `commit_id.shortest(12) ++ "\n"`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve base commit: %w", err)
	}
	base, _, _ := strings.Cut(baseOut, "\n")

	patch, err := jj(ctx, cwd, "diff", "--from", base, "--to", "@", "--git")
	if err != nil {
		return Snapshot{}, fmt.Errorf("diff working copy: %w", err)
	}
	if strings.TrimSpace(patch) == "" {
		trunk, err := jjTrunk(ctx, cwd)
		if err != nil {
			return Snapshot{}, err
		}
		return jjCaptureFrom(ctx, cwd, repoRoot, branch, jjForkPoint(trunk), "trunk "+trunk)
	}

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

// jjTrunk returns the revset naming the trunk: trunk() when it resolves to a
// real commit, else a local main/master bookmark. jj's builtin trunk()
// silently resolves to root() when no remote trunk bookmark exists, and
// falling back to root() would diff the entire history.
func jjTrunk(ctx context.Context, cwd string) (string, error) {
	atRoot, err := jj(ctx, cwd, "log", "--no-graph", "-r", "trunk() & root()", "-T", `"x"`)
	if err != nil {
		return "", fmt.Errorf("resolve trunk: %w", err)
	}
	if strings.TrimSpace(atRoot) == "" {
		return "trunk()", nil
	}
	for _, name := range []string{"main", "master"} {
		rev := fmt.Sprintf("bookmarks(exact:%q)", name)
		out, err := jj(ctx, cwd, "log", "--no-graph", "-r", rev, "-T", `"x"`)
		if err != nil {
			return "", fmt.Errorf("resolve bookmark %s: %w", name, err)
		}
		if strings.TrimSpace(out) != "" {
			return rev, nil
		}
	}
	return "", fmt.Errorf("no uncommitted changes and no trunk bookmark found (tried trunk(), main, master); pass --base <ref>")
}

// jjCaptureAt diffs the working copy against a pinned base commit, verbatim.
func jjCaptureAt(ctx context.Context, cwd, repoRoot, base string) (Snapshot, error) {
	branch, err := jjBranch(ctx, cwd)
	if err != nil {
		return Snapshot{}, err
	}
	return jjCaptureFrom(ctx, cwd, repoRoot, branch, base, "base "+base)
}

// jjForkPoint is the merge-base revset of rev and the working copy: diffing
// from rev's tip would reverse-include commits made on it after the branch
// point.
func jjForkPoint(rev string) string {
	return fmt.Sprintf("latest(heads(::(%s) & ::@))", rev)
}

// jjCaptureFrom diffs the working copy against the from revset; desc names
// the base in errors. The revset is resolved to a commit id once and the diff
// runs from that id, so the recorded BaseRef always matches the patch even if
// a concurrent jj op moves the revset.
func jjCaptureFrom(ctx context.Context, cwd, repoRoot, branch, from, desc string) (Snapshot, error) {
	baseOut, err := jj(ctx, cwd, "log", "--no-graph", "-r", from, "-T", `commit_id.shortest(12) ++ "\n"`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve %s: %w", desc, err)
	}
	base, _, _ := strings.Cut(baseOut, "\n")
	patch, err := jj(ctx, cwd, "diff", "--from", base, "--to", "@", "--git")
	if err != nil {
		return Snapshot{}, fmt.Errorf("diff from %s: %w", desc, err)
	}
	if strings.TrimSpace(patch) == "" {
		return Snapshot{}, fmt.Errorf("%w: working copy matches %s", ErrNoChanges, desc)
	}
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

// jjBranch resolves the nearest ancestor bookmark, falling back to the
// working-copy change id — the jj-idiomatic identity — when no bookmark is
// in the ancestry.
func jjBranch(ctx context.Context, cwd string) (string, error) {
	out, err := jj(ctx, cwd, "log", "--no-graph",
		"-r", "latest(::@ & bookmarks())",
		"-T", `local_bookmarks.map(|b| b.name()).join(" ")`)
	if err != nil {
		return "", fmt.Errorf("resolve bookmark: %w", err)
	}
	// A revset with no matching revisions exits 0 with empty output, so empty
	// means "no bookmark in the ancestry", not failure.
	if names := strings.Fields(out); len(names) > 0 {
		return names[0], nil
	}
	out, err = jj(ctx, cwd, "log", "--no-graph", "-r", "@", "-T", "change_id.shortest(8)")
	if err != nil {
		return "", fmt.Errorf("resolve change id: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func jj(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "jj", append([]string{"--color=never", "--no-pager"}, args...)...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("jj %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
