package dispatch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestRepo creates a git repository with one commit on a file, with
// a stable identity so commits never depend on global git config.
func newTestRepo(t *testing.T) string {
	t.Helper()

	repo := filepath.Join(t.TempDir(), "repo")
	run := func(args ...string) string {
		t.Helper()
		full := append([]string{"-C", repo}, args...)
		cmd := exec.CommandContext(t.Context(), "git", full...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
		return string(out)
	}

	require.NoError(t, os.MkdirAll(repo, 0o755))
	run("init", "-q")
	run("config", "user.email", "dispatch-test@example.com")
	run("config", "user.name", "dispatch test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one"), 0o644))
	run("add", "-A")
	run("commit", "-qm", "initial")
	return repo
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// branchExists reports whether the repo currently has the branch.
func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	out := gitIn(t, repo, "branch", "--list", branch)
	return strings.TrimSpace(out) != ""
}

// The full lifecycle: provision a workspace (branch + directory +
// registry entry), record the dispatched agent's session/handle/status,
// capture a diff that includes committed, uncommitted, and untracked
// work, and clean up idempotently.
func TestWorkspaceLifecycle(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := NewWorkspace(repo)
	require.NoError(t, err)

	entry, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, entry.ID)
	require.Equal(t, BranchPrefix+entry.ID, entry.Branch)
	require.Equal(t, filepath.Join(ws.worktreesDir, entry.Branch), entry.Path)
	require.Equal(t, StatusProvisioned, entry.Status)
	require.NotEmpty(t, entry.BaseSHA)

	// The worktree exists and is on its dispatch branch; the branch
	// exists in the parent repo.
	dirInfo, err := os.Stat(entry.Path)
	require.NoError(t, err)
	require.True(t, dirInfo.IsDir())
	require.True(t, branchExists(t, repo, entry.Branch))

	// The registry tracks the entry and answers the later-phase
	// queries: by handle, by session, and the flat list.
	got, ok := ws.Get(entry.ID)
	require.True(t, ok)
	require.Equal(t, entry, got)

	require.True(t, ws.SetSession(entry.ID, "session-1"))
	require.True(t, ws.SetHandle(entry.ID, "coder-a"))
	require.True(t, ws.SetStatus(entry.ID, StatusRunning))
	require.True(t, ws.SetEndpoint(entry.ID, "http://127.0.0.1:9999", "card"))

	byHandle, ok := ws.ByHandle("coder-a")
	require.True(t, ok)
	require.Equal(t, entry.ID, byHandle.ID)
	require.Equal(t, "session-1", byHandle.SessionID)
	require.Equal(t, StatusRunning, byHandle.Status)
	require.Equal(t, "http://127.0.0.1:9999", byHandle.Endpoint)
	require.Equal(t, "card", byHandle.AgentCard)

	bySession, ok := ws.BySession("session-1")
	require.True(t, ok)
	require.Equal(t, entry.ID, bySession.ID)
	require.Len(t, ws.List(), 1)

	// The dispatched agent's work product: a committed change, an
	// uncommitted edit, and an untracked file. All three must appear in
	// the diff — committed work is not lost.
	write(t, filepath.Join(entry.Path, "committed.txt"), "committed")
	gitIn(t, entry.Path, "add", "-A")
	gitIn(t, entry.Path, "commit", "-qm", "dispatched work")
	write(t, filepath.Join(entry.Path, "f.txt"), "one edited")
	write(t, filepath.Join(entry.Path, "untracked.txt"), "untracked")

	diff, err := ws.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "committed.txt")
	require.Contains(t, diff, "untracked.txt")
	require.Contains(t, diff, "-one")
	require.Contains(t, diff, "+one edited")

	// The temp-index approach must not stage anything into the
	// workspace's real index (the unstaged f.txt edit is expected; a
	// staged entry would mean Diff dirtied the real index).
	require.Empty(t, strings.TrimSpace(gitIn(t, entry.Path, "diff", "--cached", "--name-only")), "Diff staged changes into the real index")

	// Cleanup removes directory, branch, and registry entry — and is
	// idempotent.
	require.NoError(t, ws.Remove(ctx, entry.ID))
	_, err = os.Stat(entry.Path)
	require.True(t, os.IsNotExist(err), "worktree directory survived Remove")
	require.False(t, branchExists(t, repo, entry.Branch))
	_, ok = ws.Get(entry.ID)
	require.False(t, ok)
	require.NoError(t, ws.Remove(ctx, entry.ID))
}

// Sweep is the session-end backstop: it removes every tracked
// workspace, even abandoned ones, plus orphaned crush-dispatch-*
// directories whose registry entry was lost, and leaves no dangling
// branches.
func TestWorkspaceSweepRemovesTrackedAndOrphans(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := NewWorkspace(repo)
	require.NoError(t, err)

	tracked, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	abandoned, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	require.True(t, ws.SetStatus(abandoned.ID, StatusRunning))

	// An orphan: a worktree this process did not register, as a crashed
	// run would leave behind.
	orphanBranch := BranchPrefix + "orphaned"
	orphanPath := filepath.Join(ws.worktreesDir, orphanBranch)
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "add", "-b", orphanBranch, orphanPath).CombinedOutput()
	require.NoError(t, err, string(out))

	require.NoError(t, ws.Sweep(ctx))

	for _, path := range []string{tracked.Path, abandoned.Path, orphanPath} {
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err), "sweep left %s behind", path)
	}
	for _, branch := range []string{tracked.Branch, abandoned.Branch, orphanBranch} {
		require.False(t, branchExists(t, repo, branch), "sweep left branch %s behind", branch)
	}
	require.Empty(t, ws.List())
}

// Provisioning from an explicit base cuts the workspace at that
// revision, and the diff falls back to the recorded base SHA when the
// base branch is later deleted.
func TestWorkspaceExplicitBaseAndFallback(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	// A feature branch one commit ahead of the initial state.
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "checkout", "-q", "-b", "feature-base").CombinedOutput()
	require.NoError(t, err, string(out))
	write(t, filepath.Join(repo, "base.txt"), "base work")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "base work")
	baseSHA := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	out, err = exec.CommandContext(t.Context(), "git", "-C", repo, "checkout", "-q", "-").CombinedOutput()
	require.NoError(t, err, string(out))

	ws, err := NewWorkspace(repo)
	require.NoError(t, err)

	entry, err := ws.Provision(ctx, ProvisionOptions{Base: "feature-base"})
	require.NoError(t, err)
	require.Equal(t, "feature-base", entry.Base)
	require.Equal(t, baseSHA, entry.BaseSHA)
	require.FileExists(t, filepath.Join(entry.Path, "base.txt"))

	// Dispatched work, then the base branch is deleted out from under
	// the entry.
	write(t, filepath.Join(entry.Path, "work.txt"), "dispatched")
	gitIn(t, entry.Path, "add", "-A")
	gitIn(t, entry.Path, "commit", "-qm", "dispatched")
	out, err = exec.CommandContext(t.Context(), "git", "-C", repo, "branch", "-D", "feature-base").CombinedOutput()
	require.NoError(t, err, string(out))

	diff, err := ws.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "work.txt")

	require.NoError(t, ws.Remove(ctx, entry.ID))
}

func TestWorkspaceDiffUnknownID(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := NewWorkspace(repo)
	require.NoError(t, err)

	_, err = ws.Diff(t.Context(), "no-such-dispatch")
	require.Error(t, err)
}

func TestWorkspaceRequiresGitRepo(t *testing.T) {
	_, err := NewWorkspace(t.TempDir())
	require.Error(t, err)
}

// Registry mutations on unknown entries report false rather than
// panicking or silently succeeding.
func TestWorkspaceUpdateUnknownEntry(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := NewWorkspace(repo)
	require.NoError(t, err)

	require.False(t, ws.SetStatus("nope", StatusRunning))
	require.False(t, ws.SetSession("nope", "s"))
	require.False(t, ws.SetHandle("nope", "h"))
	require.False(t, ws.SetEndpoint("nope", "e", nil))
	require.False(t, ws.Update("nope", func(e *Entry) {}))
	require.False(t, ws.Update("", nil))

	_, ok := ws.ByHandle("")
	require.False(t, ok)
	_, ok = ws.BySession("")
	require.False(t, ok)
}

// HandleSlug normalizes candidates into handle form and leaves
// unhandle-able ones empty so the caller can fall through.
func TestHandleSlug(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"tester", "tester"},
		{"@Tester", "tester"},
		{"  @Team Lead  ", "team-lead"},
		{"Docs_Writer", "docs-writer"},
		{"---weird__name---", "weird-name"},
		{"🤖 robot", "robot"},
		{"@__", ""},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, HandleSlug(tc.in), "HandleSlug(%q)", tc.in)
	}
}

// AssignHandle assigns the requested handle, derives one from the role,
// falls back to "agent", and suffixes collisions numerically — including
// against finished dispatches, which keep their handles until their
// registry entries are removed.
func TestAssignHandle(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := NewWorkspace(repo)
	require.NoError(t, err)
	ctx := t.Context()

	a, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	b, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	c, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)

	// Explicit handle, with the leading @ tolerated.
	handle, ok := ws.AssignHandle(a.ID, "@Tester", "writes tests")
	require.True(t, ok)
	require.Equal(t, "tester", handle)

	// Derived from the role when no handle was requested.
	handle, ok = ws.AssignHandle(b.ID, "", "Docs Writer")
	require.True(t, ok)
	require.Equal(t, "docs-writer", handle)

	// Default when neither handle nor role was given.
	handle, ok = ws.AssignHandle(c.ID, "", "")
	require.True(t, ok)
	require.Equal(t, "agent", handle)

	// The entry carries handle and role, and ByHandle resolves it.
	entry, ok := ws.Get(a.ID)
	require.True(t, ok)
	require.Equal(t, "tester", entry.Handle)
	require.Equal(t, "writes tests", entry.Role)
	got, ok := ws.ByHandle("tester")
	require.True(t, ok)
	require.Equal(t, a.ID, got.ID)

	// A fourth dispatch asking for an in-use handle gets a suffix.
	d, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	handle, ok = ws.AssignHandle(d.ID, "tester", "")
	require.True(t, ok)
	require.Equal(t, "tester-2", handle)

	// Finished dispatches keep their handles: the registry is the
	// namespace until Remove.
	ws.SetStatus(a.ID, StatusCompleted)
	e, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	handle, ok = ws.AssignHandle(e.ID, "tester", "")
	require.True(t, ok)
	require.Equal(t, "tester-3", handle)

	// A removed entry releases its handle for reuse.
	require.NoError(t, ws.Remove(ctx, b.ID))
	handle, ok = ws.AssignHandle(e.ID, "", "docs writer")
	require.True(t, ok)
	require.Equal(t, "docs-writer", handle)

	// Unknown entry: not assigned.
	_, ok = ws.AssignHandle("no-such-id", "x", "")
	require.False(t, ok)
}

// IsTerminal marks exactly the terminal statuses.
func TestStatusIsTerminal(t *testing.T) {
	t.Parallel()

	for _, s := range []Status{StatusCompleted, StatusFailed, StatusKilled} {
		require.True(t, s.IsTerminal(), "%s must be terminal", s)
	}
	for _, s := range []Status{StatusProvisioned, StatusRunning} {
		require.False(t, s.IsTerminal(), "%s must not be terminal", s)
	}
}
