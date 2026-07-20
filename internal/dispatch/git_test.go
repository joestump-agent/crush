package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// initRepo creates a git repository in a fresh temp dir with one commit
// and returns its path. Author identity is set locally so worktree
// commits succeed without a global git config.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@crush.test")
	runGit(t, dir, "config", "user.name", "Crush Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	return string(out)
}

func TestGitBackend_CreateForksBranchAndDir(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	backend := NewGitBackend(repo)
	dest := filepath.Join(t.TempDir(), "ws")

	prov, err := backend.Create(context.Background(), dest, "abc123", "")
	require.NoError(t, err)

	require.Equal(t, dest, prov.Path)
	require.Equal(t, branchPrefix+"abc123", prov.Branch)
	require.Len(t, prov.Base, 40, "base should be a resolved commit SHA")

	require.DirExists(t, dest)
	require.FileExists(t, filepath.Join(dest, "README.md"))

	// The branch exists in the parent repo.
	branches := runGit(t, repo, "branch", "--list", prov.Branch)
	require.Contains(t, branches, prov.Branch)
}

func TestGitBackend_CreateBadBaseErrors(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	backend := NewGitBackend(repo)
	dest := filepath.Join(t.TempDir(), "ws")

	_, err := backend.Create(context.Background(), dest, "abc123", "no-such-ref")
	require.Error(t, err)
	require.NoDirExists(t, dest)
}

func TestGitBackend_DiffCapturesCommittedAndUncommitted(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	backend := NewGitBackend(repo)
	dest := filepath.Join(t.TempDir(), "ws")

	prov, err := backend.Create(context.Background(), dest, "abc123", "")
	require.NoError(t, err)

	// A committed change on the dispatch branch.
	require.NoError(t, os.WriteFile(filepath.Join(dest, "committed.txt"), []byte("committed\n"), 0o644))
	runGit(t, dest, "add", "-A")
	runGit(t, dest, "commit", "-m", "work")

	// An unstaged change to a tracked file, and an untracked file.
	require.NoError(t, os.WriteFile(filepath.Join(dest, "README.md"), []byte("changed\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "untracked.txt"), []byte("new\n"), 0o644))

	diff, err := backend.Diff(context.Background(), prov.Path, prov.Base)
	require.NoError(t, err)

	require.Contains(t, diff, "committed.txt", "committed work missing from diff")
	require.Contains(t, diff, "untracked.txt", "untracked file missing from diff")
	require.Contains(t, diff, "changed", "unstaged edit missing from diff")

	// The real index is untouched by the diff's temp-index staging.
	status := runGit(t, dest, "status", "--porcelain")
	require.Contains(t, status, "untracked.txt")
	require.Contains(t, status, "README.md")
}

func TestGitBackend_RemoveIsIdempotent(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	backend := NewGitBackend(repo)
	dest := filepath.Join(t.TempDir(), "ws")

	prov, err := backend.Create(context.Background(), dest, "abc123", "")
	require.NoError(t, err)

	require.NoError(t, backend.Remove(context.Background(), prov.Path, prov.Branch))
	require.NoDirExists(t, dest)
	require.NotContains(t, runGit(t, repo, "branch", "--list", prov.Branch), prov.Branch)

	// A second removal of the same, now-gone workspace is a no-op.
	require.NoError(t, backend.Remove(context.Background(), prov.Path, prov.Branch))
}

func TestGitBackend_RemoveAfterDirDeletedPrunesAndDropsBranch(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	backend := NewGitBackend(repo)
	dest := filepath.Join(t.TempDir(), "ws")

	prov, err := backend.Create(context.Background(), dest, "abc123", "")
	require.NoError(t, err)

	// Simulate an out-of-band deletion of the worktree directory.
	require.NoError(t, os.RemoveAll(dest))

	require.NoError(t, backend.Remove(context.Background(), prov.Path, prov.Branch))
	require.NotContains(t, runGit(t, repo, "branch", "--list", prov.Branch), prov.Branch)
}
