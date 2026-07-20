package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// branchPrefix is prepended to a dispatch id to name the branch a git
// worktree is created on.
const branchPrefix = "crush-dispatch-"

// GitBackend provisions workspaces as git worktrees of a repository.
// Each workspace is a real worktree on its own branch, sharing the
// repo's object database, so it is cheap to create and its commits are
// visible to the parent repo for review or merge.
type GitBackend struct {
	// repoDir is the git repository worktrees are added to.
	repoDir string
}

// NewGitBackend returns a [Backend] that adds worktrees to the git
// repository at repoDir.
func NewGitBackend(repoDir string) *GitBackend {
	return &GitBackend{repoDir: repoDir}
}

var _ Backend = (*GitBackend)(nil)

// Create adds a git worktree at dest on a new branch crush-dispatch-{id},
// forked from base (empty means the repo's current HEAD). It resolves
// base to a concrete commit first so the returned fork point stays valid
// even after the dispatched agent commits on top of it.
func (b *GitBackend) Create(ctx context.Context, dest, id, base string) (Provisioned, error) {
	if base == "" {
		base = "HEAD"
	}
	commit, err := b.resolve(ctx, base)
	if err != nil {
		return Provisioned{}, fmt.Errorf("resolve base %q: %w", base, err)
	}

	branch := branchPrefix + id
	if out, err := b.git(ctx, b.repoDir, "worktree", "add", "-b", branch, dest, commit); err != nil {
		return Provisioned{}, fmt.Errorf("git worktree add: %w: %s", err, out)
	}

	return Provisioned{Path: dest, Branch: branch, Base: commit}, nil
}

// Diff returns the worktree's full work product as one unified diff:
// everything from base to the current working tree — committed, staged,
// unstaged, and untracked. The entire worktree is staged into a
// throwaway temporary index so the worktree's real index is never
// touched.
func (b *GitBackend) Diff(ctx context.Context, path, base string) (string, error) {
	if base == "" {
		base = "HEAD"
	}

	tmp, err := os.CreateTemp("", "crush-dispatch-index-")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	env := append(os.Environ(), "GIT_INDEX_FILE="+tmp.Name())
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", path}, args...)...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
		}
		return stdout.Bytes(), nil
	}

	// Seed the temp index from the fork point, stage the whole worktree
	// into it (honoring .gitignore, so untracked files come along), and
	// diff that against the fork point.
	if _, err := run("read-tree", base); err != nil {
		return "", err
	}
	if _, err := run("add", "-A"); err != nil {
		return "", err
	}
	out, err := run("diff", "--cached", base)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Remove deletes the worktree at path and its branch. It is idempotent:
// a worktree or branch that is already gone is not an error. A stale
// administrative entry left by an out-of-band directory deletion is
// pruned so the branch can then be removed.
func (b *GitBackend) Remove(ctx context.Context, path, branch string) error {
	if _, err := b.git(ctx, b.repoDir, "worktree", "remove", "--force", path); err != nil {
		// The worktree directory may already be gone; prune the
		// administrative record and carry on to branch cleanup rather
		// than failing an idempotent teardown.
		_, _ = b.git(ctx, b.repoDir, "worktree", "prune")
	}
	if branch != "" {
		if _, err := b.git(ctx, b.repoDir, "branch", "-D", branch); err != nil {
			// A missing branch is fine; anything else is worth
			// surfacing so a real failure is not silently swallowed.
			if !strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("delete branch %q: %w", branch, err)
			}
		}
	}
	return nil
}

// resolve turns a ref into the concrete commit SHA it points at.
func (b *GitBackend) resolve(ctx context.Context, ref string) (string, error) {
	out, err := b.git(ctx, b.repoDir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// git runs a git subcommand in dir, returning combined output. On
// failure the error carries git's stderr so callers can surface it.
func (b *GitBackend) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
