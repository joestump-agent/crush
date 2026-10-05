package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/lock"
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
	run("-c", "commit.gpgsign=false", "commit", "-qm", "initial")
	return repo
}

// newWorkspace creates a workspace whose worktrees live under the repo
// itself (the default before #383 moved them), nested so the ignore
// file lands beside the per-repo key directory.
func newWorkspace(t *testing.T, repo string) (*Workspace, error) {
	t.Helper()
	return NewWorkspace(repo, filepath.Join(repo, "worktrees", "repo-key"))
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

// commitIn stages nothing and commits the index in dir, with gpg
// signing disabled so fixtures never depend on the machine's
// commit.gpgsign setting.
func commitIn(t *testing.T, dir, msg string) {
	t.Helper()
	full := []string{"-C", dir, "-c", "commit.gpgsign=false", "commit", "-qm", msg}
	cmd := exec.CommandContext(t.Context(), "git", full...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git commit %q: %s", msg, out)
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

	ws, err := newWorkspace(t, repo)
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

	// Every provisioned workspace holds a lease and carries an owner
	// marker, both next to the worktree so they never appear in a
	// diff.
	require.FileExists(t, ws.leasePath(entry.Branch))
	require.FileExists(t, ws.ownerPath(entry.Branch))

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
	require.NotContains(t, diff, ".lock")
	require.NotContains(t, diff, ".owner.json")

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

	// The owner marker goes with the workspace. The lock file stays on
	// disk — flock is keyed by inode, so it is never unlinked — but
	// nobody holds it.
	require.NoFileExists(t, ws.ownerPath(entry.Branch))
	rel, err := lock.TryFile(ws.leasePath(entry.Branch))
	require.NoError(t, err)
	rel()
}

// A locked worktree fails its own removal but must not stop the sweep:
// the other workspaces are still removed, the failed entry stays
// registered for retry, and the orphan pass and final prune still run.
func TestSweepContinuesPastLockedWorktree(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	one, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	locked, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	three, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)

	// Lock the worktree, as a stale run or a Windows hold would.
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "lock", locked.Path).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() {
		gitIn(t, repo, "worktree", "unlock", locked.Path)
		gitIn(t, repo, "worktree", "prune")
	})

	err = ws.Sweep(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), locked.ID, "sweep error must name the locked entry")

	// The other two workspaces are fully gone: directory, branch, and
	// registry entry.
	for _, gone := range []Entry{one, three} {
		_, statErr := os.Stat(gone.Path)
		require.True(t, os.IsNotExist(statErr), "sweep left %s behind", gone.Path)
		require.False(t, branchExists(t, repo, gone.Branch), "sweep left branch %s behind", gone.Branch)
		_, ok := ws.Get(gone.ID)
		require.False(t, ok, "removed entry %s still registered", gone.ID)
	}

	// The locked entry is still registered so a later sweep can retry.
	_, ok := ws.Get(locked.ID)
	require.True(t, ok)
	require.Len(t, ws.List(), 1)

	// Unlock and sweep again: the retry succeeds and nothing is left.
	gitIn(t, repo, "worktree", "unlock", locked.Path)
	require.NoError(t, ws.Sweep(ctx))
	require.Empty(t, ws.List())
	_, statErr := os.Stat(locked.Path)
	require.True(t, os.IsNotExist(statErr), "retry sweep left %s behind", locked.Path)
	require.False(t, branchExists(t, repo, locked.Branch))
}

// Removing an entry whose worktree and branch were deleted behind the
// registry's back succeeds: absence is detected by exit code, without
// reading git's (localized) message text.
func TestRemoveToleratesMissingBranch(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	entry, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)

	// Delete the worktree and the branch out from under the registry.
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "remove", "--force", entry.Path).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.CommandContext(ctx, "git", "-C", repo, "branch", "-D", entry.Branch).CombinedOutput()
	require.NoError(t, err, string(out))

	require.NoError(t, ws.Sweep(ctx))
	require.Empty(t, ws.List())
	require.NoError(t, ws.Remove(ctx, entry.ID))
}

// hasBranch decides by git's exit code, never by its message text.
func TestHasBranch(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	out, err := exec.CommandContext(ctx, "git", "-C", repo, "branch", "has-branch-probe").CombinedOutput()
	require.NoError(t, err, string(out))
	require.True(t, hasBranch(ctx, repo, "has-branch-probe"))
	require.False(t, hasBranch(ctx, repo, BranchPrefix+"missing"))
}

// Sweep is the session-end backstop: it removes every tracked
// workspace, even abandoned ones, plus orphaned crush-dispatch-*
// directories whose registry entry was lost, and leaves no dangling
// branches.
func TestWorkspaceSweepRemovesTrackedAndOrphans(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	tracked, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	abandoned, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	require.True(t, ws.SetStatus(abandoned.ID, StatusRunning))

	// An orphan: a worktree this process did not register, as a crashed
	// run would leave behind. Its lease file survives the crash — the
	// kernel releases the lock, not the file — so the orphan here gets
	// a lock file nobody holds, and is still reclaimed.
	orphanBranch := BranchPrefix + "orphaned"
	orphanPath := filepath.Join(ws.worktreesDir, orphanBranch)
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "add", "-b", orphanBranch, orphanPath).CombinedOutput()
	require.NoError(t, err, string(out))
	orphanRelease, err := lock.TryFile(ws.leasePath(orphanBranch))
	require.NoError(t, err)
	orphanRelease()

	// A worktree with no lease file at all: ownership cannot be
	// proven, so Sweep must leave it alone.
	unmarkedBranch := BranchPrefix + "unmarked"
	unmarkedPath := filepath.Join(ws.worktreesDir, unmarkedBranch)
	out, err = exec.CommandContext(t.Context(), "git", "-C", repo, "worktree", "add", "-b", unmarkedBranch, unmarkedPath).CombinedOutput()
	require.NoError(t, err, string(out))

	require.NoError(t, ws.Sweep(ctx))

	for _, path := range []string{tracked.Path, abandoned.Path, orphanPath} {
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err), "sweep left %s behind", path)
	}
	for _, branch := range []string{tracked.Branch, abandoned.Branch, orphanBranch} {
		require.False(t, branchExists(t, repo, branch), "sweep left branch %s behind", branch)
	}
	_, err = os.Stat(unmarkedPath)
	require.NoError(t, err, "sweep removed a worktree with no lease file")
	require.True(t, branchExists(t, repo, unmarkedBranch))
	require.Empty(t, ws.List())
}

// dropLeases simulates process exit: it releases every lease the
// workspace holds without removing any workspace, leaving the lock
// files on disk unheld — exactly what a crashed run leaves behind.
func dropLeases(ws *Workspace) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, release := range ws.leases {
		release()
	}
	ws.leases = make(map[string]func())
}

// A second Workspace on the same repository is a live peer: its
// provisioned workspaces — committed and uncommitted work included —
// survive another instance's Sweep, and keep diffing.
func TestSweepLeavesOtherInstancesLiveWorkspaces(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	a, err := newWorkspace(t, repo)
	require.NoError(t, err)
	b, err := newWorkspace(t, repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Sweep(context.Background()) })

	entry, err := b.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	write(t, filepath.Join(entry.Path, "committed.txt"), "committed")
	gitIn(t, entry.Path, "add", "-A")
	gitIn(t, entry.Path, "-c", "commit.gpgsign=false", "commit", "-qm", "dispatched work")
	write(t, filepath.Join(entry.Path, "f.txt"), "one edited")

	require.NoError(t, a.Sweep(ctx))

	_, err = os.Stat(entry.Path)
	require.NoError(t, err, "sweep removed another instance's live workspace")
	require.True(t, branchExists(t, repo, entry.Branch))
	require.FileExists(t, filepath.Join(entry.Path, "committed.txt"))
	require.FileExists(t, filepath.Join(entry.Path, "f.txt"))

	diff, err := b.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "committed.txt")
	require.Contains(t, diff, "+one edited")
	require.NotContains(t, diff, ".lock")
	require.NotContains(t, diff, ".owner.json")

	_, ok := b.Get(entry.ID)
	require.True(t, ok)
}

// A workspace whose owner died — the lock file remains but nobody
// holds the lease — is an orphan like any other, and Sweep reclaims
// it: directory, branch, and ownership artifacts.
func TestSweepReclaimsDeadOwnersWorkspace(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	b, err := newWorkspace(t, repo)
	require.NoError(t, err)
	entry, err := b.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)

	a, err := newWorkspace(t, repo)
	require.NoError(t, err)
	dropLeases(b)

	require.NoError(t, a.Sweep(ctx))

	_, err = os.Stat(entry.Path)
	require.True(t, os.IsNotExist(err), "sweep left a dead owner's workspace behind")
	require.False(t, branchExists(t, repo, entry.Branch))
	require.FileExists(t, b.leasePath(entry.Branch))
	rel, err := lock.TryFile(b.leasePath(entry.Branch))
	require.NoError(t, err)
	rel()
	require.NoFileExists(t, b.ownerPath(entry.Branch))
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

	ws, err := newWorkspace(t, repo)
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

// A detached parent (a CI checkout, a rebase or bisect in progress, any
// jj-colocated repo) records Base as "HEAD"; the diff must still show
// the agent's committed work, not only the uncommitted leftovers (#380).
func TestDiffDetachedParent(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	gitIn(t, repo, "checkout", "--detach")

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	entry, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	require.Equal(t, "HEAD", entry.Base)
	require.NotEmpty(t, entry.BaseSHA)

	// The dispatched agent commits one file and leaves another
	// uncommitted. Both must appear in the diff.
	write(t, filepath.Join(entry.Path, "committed.txt"), "committed")
	gitIn(t, entry.Path, "add", "-A")
	commitIn(t, entry.Path, "committed work")
	write(t, filepath.Join(entry.Path, "uncommitted.txt"), "uncommitted")

	diff, err := ws.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "b/committed.txt")
	require.Contains(t, diff, "b/uncommitted.txt")

	require.NoError(t, ws.Remove(ctx, entry.ID))
}

// A model-supplied relative base ("HEAD~1") resolves at provision time;
// the diff must include every agent commit, not just the last (#380).
func TestDiffRelativeBase(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	// A second commit so HEAD~1 is not the only commit.
	write(t, filepath.Join(repo, "second.txt"), "second")
	gitIn(t, repo, "add", "-A")
	commitIn(t, repo, "second")

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	entry, err := ws.Provision(ctx, ProvisionOptions{Base: "HEAD~1"})
	require.NoError(t, err)
	require.Equal(t, "HEAD~1", entry.Base)

	// Two agent commits on top of the relative base.
	write(t, filepath.Join(entry.Path, "a.txt"), "a")
	gitIn(t, entry.Path, "add", "-A")
	commitIn(t, entry.Path, "first agent commit")
	write(t, filepath.Join(entry.Path, "b.txt"), "b")
	gitIn(t, entry.Path, "add", "-A")
	commitIn(t, entry.Path, "second agent commit")

	diff, err := ws.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "a.txt")
	require.Contains(t, diff, "b.txt")

	require.NoError(t, ws.Remove(ctx, entry.ID))
}

// A base branch that moves forward after provision does not leak into
// the diff: the diff is against the recorded base SHA, not the branch's
// new tip (#380).
func TestDiffIgnoresAdvancedBase(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	entry, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)

	// The dispatch works; meanwhile the base branch advances.
	write(t, filepath.Join(entry.Path, "work.txt"), "dispatched")
	gitIn(t, entry.Path, "add", "-A")
	commitIn(t, entry.Path, "dispatched work")
	write(t, filepath.Join(repo, "advanced.txt"), "base branch work")
	gitIn(t, repo, "add", "-A")
	commitIn(t, repo, "base advanced")

	diff, err := ws.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "work.txt")
	require.NotContains(t, diff, "advanced.txt")

	require.NoError(t, ws.Remove(ctx, entry.ID))
}

// An entry with no recorded base SHA is an error, never a fallback to
// some other revision (#380).
func TestDiffMissingBaseSHA(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	// A directory standing in for a workspace whose registry entry lost
	// its base SHA.
	path := filepath.Join(ws.worktreesDir, BranchPrefix+"no-base")
	require.NoError(t, os.MkdirAll(path, 0o755))
	t.Cleanup(func() {
		ws.mu.Lock()
		delete(ws.entries, "no-base")
		ws.mu.Unlock()
		os.RemoveAll(path)
	})
	ws.entries["no-base"] = Entry{ID: "no-base", Path: path, Base: "HEAD"}

	_, err = ws.Diff(t.Context(), "no-base")
	require.Error(t, err)
}

// Bases a model could supply that git would otherwise read as options,
// plus a ref that does not exist: Provision must fail without leaving a
// worktree, branch, or registry entry behind.
func TestWorkspaceProvisionRejectsBadBases(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Sweep(context.Background()) })

	worktreesBefore := gitIn(t, repo, "worktree", "list", "--porcelain")
	branchesBefore := gitIn(t, repo, "branch", "--list", BranchPrefix+"*")

	tests := []struct {
		base string
		err  string
	}{
		{"--lock", `invalid base "--lock"`},
		{"--no-checkout", `invalid base "--no-checkout"`},
		{"-b", `invalid base "-b"`},
		{"no-such-ref", "unknown revision"},
	}
	for _, tt := range tests {
		t.Run(tt.base, func(t *testing.T) {
			_, err := ws.Provision(ctx, ProvisionOptions{Base: tt.base})
			require.ErrorContains(t, err, tt.err)
		})
	}

	require.Equal(t, worktreesBefore, gitIn(t, repo, "worktree", "list", "--porcelain"))
	require.Equal(t, branchesBefore, gitIn(t, repo, "branch", "--list", BranchPrefix+"*"))
	require.Empty(t, ws.List())
}

// Branch, tag, full SHA, and HEAD~1 bases still work: BaseSHA is the
// commit git says the ref points at, and the worktree is cut from it.
func TestWorkspaceProvisionValidBases(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	gitIn(t, repo, "tag", "v1")
	write(t, filepath.Join(repo, "f.txt"), "two")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "second")
	headSHA := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	gitIn(t, repo, "branch", "stable", "v1")
	baseSHA := strings.TrimSpace(gitIn(t, repo, "rev-parse", "v1^{commit}"))

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	tests := []struct {
		name string
		base string
		want string
	}{
		{"branch", "stable", baseSHA},
		{"tag", "v1", baseSHA},
		{"full sha", headSHA, headSHA},
		{"head tilde one", "HEAD~1", baseSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := ws.Provision(ctx, ProvisionOptions{Base: tt.base})
			require.NoError(t, err)
			require.Equal(t, tt.base, entry.Base)
			require.Equal(t, tt.want, entry.BaseSHA)
			require.Equal(t, tt.want, strings.TrimSpace(gitIn(t, entry.Path, "rev-parse", "HEAD")))
			require.NoError(t, ws.Remove(ctx, entry.ID))
		})
	}
}

func TestWorkspaceDiffUnknownID(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	_, err = ws.Diff(t.Context(), "no-such-dispatch")
	require.Error(t, err)
}

func TestWorkspaceRequiresGitRepo(t *testing.T) {
	_, err := NewWorkspace(t.TempDir(), filepath.Join(t.TempDir(), "worktrees"))
	require.Error(t, err)
}

// Registry mutations on unknown entries report false rather than
// panicking or silently succeeding.
func TestWorkspaceUpdateUnknownEntry(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := newWorkspace(t, repo)
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
// falls back to "agent", and suffixes collisions numerically against the
// running agents only (#399): a finished dispatch releases its handle for
// reuse. Reserved names are suffixed like collisions, and the 32-byte
// cap holds even with a suffix.
func TestAssignHandle(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Sweep(context.Background()) })
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

	// A finished dispatch releases its handle: the next dispatch asking
	// for it gets the bare handle again, even though the finished entry
	// still carries it (#399).
	ws.SetStatus(a.ID, StatusCompleted)
	e, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	handle, ok = ws.AssignHandle(e.ID, "tester", "")
	require.True(t, ok)
	require.Equal(t, "tester", handle)

	// A reserved name is never assigned outright: it is suffixed like a
	// collision (#399).
	f, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	handle, ok = ws.AssignHandle(f.ID, "task", "")
	require.True(t, ok)
	require.Equal(t, "task-2", handle)

	// A 60-character role yields a handle of at most 32 bytes (#399).
	g, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	long := strings.Repeat("x", 60)
	handle, ok = ws.AssignHandle(g.ID, long, "")
	require.True(t, ok)
	require.Equal(t, strings.Repeat("x", MaxHandleLength), handle)

	// A collision on an already-capped handle suffixes with the base
	// truncated so "-2" fits: the result stays within the cap.
	h, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	handle, ok = ws.AssignHandle(h.ID, long, "")
	require.True(t, ok)
	require.Equal(t, strings.Repeat("x", MaxHandleLength-2)+"-2", handle)
	require.LessOrEqual(t, len(handle), MaxHandleLength)

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

// HandleSlug caps a slug at MaxHandleLength bytes (#399), trimming a
// trailing dash the cap leaves.
func TestHandleSlugLengthCap(t *testing.T) {
	t.Parallel()

	capped := HandleSlug(strings.Repeat("a", 60))
	require.Len(t, capped, MaxHandleLength)
	require.Equal(t, strings.Repeat("a", MaxHandleLength), capped)

	// The cap lands after a dash run: the trailing dash is trimmed.
	dashed := HandleSlug(strings.Repeat("a", 30) + "-" + strings.Repeat("b", 30))
	require.LessOrEqual(t, len(dashed), MaxHandleLength)
	require.False(t, strings.HasSuffix(dashed, "-"), "a capped slug never ends with a dash")
	require.Equal(t, strings.Repeat("a", 30)+"-b", dashed)
}

// ByHandle prefers the live entry carrying the handle; when only
// finished entries carry it, the most recently finished one answers
// (#399), so a mention of a finished @handle still renders its card
// until the handle is reused.
func TestByHandlePrefersLiveEntry(t *testing.T) {
	repo := newTestRepo(t)
	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)
	// Sweep releases this process's leases so Windows can unlink the
	// lock files during TempDir cleanup.
	t.Cleanup(func() { _ = ws.Sweep(context.Background()) })
	ctx := t.Context()

	// Release the entries' ownership leases on the way out: the open
	// lock files keep t.TempDir's RemoveAll from cleaning up on
	// Windows.
	t.Cleanup(func() { _ = ws.Sweep(context.Background()) })

	a, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	_, ok := ws.AssignHandle(a.ID, "tester", "")
	require.True(t, ok)

	// Finish a; its handle still resolves, as the finished fallback.
	require.True(t, ws.SetStatus(a.ID, StatusCompleted))
	require.True(t, ws.Update(a.ID, func(e *Entry) { e.FinishedAt = time.Now().Add(-time.Minute) }))
	got, ok := ws.ByHandle("tester")
	require.True(t, ok, "a finished handle still resolves")
	require.Equal(t, a.ID, got.ID)

	// A newer finished entry wins the fallback over an older one.
	b, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	_, ok = ws.AssignHandle(b.ID, "tester", "")
	require.True(t, ok)
	require.True(t, ws.SetStatus(b.ID, StatusKilled))
	require.True(t, ws.Update(b.ID, func(e *Entry) { e.FinishedAt = time.Now() }))
	got, ok = ws.ByHandle("tester")
	require.True(t, ok)
	require.Equal(t, b.ID, got.ID, "the most recently finished entry answers")

	// A new dispatch reuses the released handle; the live entry now wins.
	c, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)
	handle, ok := ws.AssignHandle(c.ID, "tester", "")
	require.True(t, ok)
	require.Equal(t, "tester", handle, "the handle was free again")
	got, ok = ws.ByHandle("tester")
	require.True(t, ok)
	require.Equal(t, c.ID, got.ID, "the live entry answers over finished ones")
}

// newTestRepoWithRemote extends a test repo with a bare "origin" and
// an origin/main ref, the shape a dispatch fan-out provisions against.
func newTestRepoWithRemote(t *testing.T) string {
	t.Helper()

	repo := newTestRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	require.NoError(t, os.MkdirAll(remote, 0o755))
	run := func(dir string, args ...string) {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	run(remote, "init", "-q", "--bare")
	run(repo, "remote", "add", "origin", remote)
	run(repo, "push", "-q", "origin", "HEAD:main")
	run(repo, "fetch", "-q", "origin")
	return repo
}

// worktreeCount counts the worktrees git reports for the repo, by its
// porcelain form's leading "worktree" line per entry.
func worktreeCount(t *testing.T, repo string) int {
	t.Helper()

	out := strings.TrimSpace(gitIn(t, repo, "worktree", "list", "--porcelain"))
	if out == "" {
		return 0
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			count++
		}
	}
	return count
}

// dispatchBranches lists the crush-dispatch-* branches in the repo.
func dispatchBranches(t *testing.T, repo string) []string {
	t.Helper()

	out := strings.TrimSpace(gitIn(t, repo, "branch", "--list", BranchPrefix+"*"))
	if out == "" {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}

// A fan-out of concurrent provisions against a remote-tracking base —
// what fantasy's parallel tool calls do — must all succeed: unserialized
// worktree add calls race on .git/config and fail, leaving orphaned
// branches behind.
func TestProvisionConcurrent(t *testing.T) {
	repo := newTestRepoWithRemote(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Sweep(context.Background()) })

	const n = 12
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = ws.Provision(ctx, ProvisionOptions{Base: "origin/main"})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent provision %d", i)
	}

	// Every provision is registered, its directory is on disk, and the
	// branch count matches the registry.
	entries := ws.List()
	require.Len(t, entries, n)
	for _, entry := range entries {
		require.DirExists(t, entry.Path, "worktree directory missing for %s", entry.ID)
	}
	require.Len(t, dispatchBranches(t, repo), n)

	// --no-track keeps .git/config free of upstream entries for the
	// dispatch branches.
	require.Empty(t, strings.TrimSpace(gitIn(t, repo, "config", "--get-regexp", `^branch\.crush-dispatch-`)))
}

// A failed provision leaves no branch, no directory, and no worktree
// admin entry: cleanup runs in the locked region right after the add.
func TestProvisionFailureCleansUp(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Sweep(context.Background()) })

	var failedBranch string
	// A non-empty target directory makes worktree add fail after the
	// branch is created — the exact residue the fix must remove.
	ws.provisionHook = func(branch, path string) {
		failedBranch = branch
		require.NoError(t, os.MkdirAll(path, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(path, "stuck.txt"), []byte("x"), 0o644))
	}

	_, err = ws.Provision(ctx, ProvisionOptions{})
	require.Error(t, err)

	require.False(t, branchExists(t, repo, failedBranch), "failed provision left its branch")
	_, statErr := os.Stat(filepath.Join(ws.worktreesDir, failedBranch))
	require.True(t, os.IsNotExist(statErr), "failed provision left its directory")
	require.Empty(t, dispatchBranches(t, repo))

	// No stale worktree admin entries either: only the main worktree
	// remains.
	require.NoError(t, runGit(ctx, repo, nil, "worktree", "prune"))
	require.Equal(t, 1, worktreeCount(t, repo))
}

// cleanupFailedProvision removes each residue class a failed worktree
// add can leave behind: a created branch, a partial directory, and a
// stale admin entry — including when the add never created a branch.
func TestCleanupFailedProvision(t *testing.T) {
	repo := newTestRepo(t)
	ctx := t.Context()

	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)

	// Residue from a crashed provision: the branch exists, its
	// directory is gone, and a stale admin entry remains.
	branch := BranchPrefix + "crashed"
	path := filepath.Join(ws.worktreesDir, branch)
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "-b", branch, path).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.RemoveAll(path))

	ws.cleanupFailedProvision(ctx, branch, path)

	require.False(t, branchExists(t, repo, branch))
	require.Empty(t, dispatchBranches(t, repo))
	require.NoError(t, runGit(ctx, repo, nil, "worktree", "prune"))
	require.Equal(t, 1, worktreeCount(t, repo))

	// A partial directory is removed even without a branch.
	partial := BranchPrefix + "partial"
	partialPath := filepath.Join(ws.worktreesDir, partial)
	require.NoError(t, os.MkdirAll(partialPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(partialPath, "leftover"), []byte("x"), 0o644))

	ws.cleanupFailedProvision(ctx, partial, partialPath)

	_, statErr := os.Stat(partialPath)
	require.True(t, os.IsNotExist(statErr))
}

func TestWorktreesDirStablePerRepo(t *testing.T) {
	repo := newTestRepo(t)
	other := newTestRepo(t)
	dataDir := t.TempDir()

	first, err := WorktreesDir(dataDir, repo)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dataDir, "worktrees"), filepath.Dir(first))

	again, err := WorktreesDir(dataDir, repo)
	require.NoError(t, err)
	require.Equal(t, first, again)

	// A linked worktree of the same repository shares the key: the
	// common dir, not the working directory, is hashed.
	link := filepath.Join(t.TempDir(), "link")
	gitIn(t, repo, "worktree", "add", "-b", "crush-test-link", "--", link, "HEAD")
	require.DirExists(t, link)
	fromLink, err := WorktreesDir(dataDir, link)
	require.NoError(t, err)
	require.Equal(t, first, fromLink)

	// A different repository sharing the same data directory lands in a
	// different key under the same worktrees root.
	fromOther, err := WorktreesDir(dataDir, other)
	require.NoError(t, err)
	require.Equal(t, filepath.Dir(first), filepath.Dir(fromOther))
	require.NotEqual(t, first, fromOther)
}

// WorktreesDir refuses a directory that is not inside a git repository.
func TestWorktreesDirRequiresGitRepo(t *testing.T) {
	_, err := WorktreesDir(t.TempDir(), t.TempDir())
	require.Error(t, err)
}

// NewWorkspace keeps the worktrees location out of git: a "*" .gitignore
// is written beside the per-repo worktrees directory — one level up, so
// a single file covers every key sharing the root — and a workspace
// created over the same location again leaves the file alone (#383).
func TestNewWorkspaceWritesGitignore(t *testing.T) {
	repo := newTestRepo(t)
	wtDir := filepath.Join(repo, "worktrees", "key")

	_, err := NewWorkspace(repo, wtDir)
	require.NoError(t, err)

	ignorePath := filepath.Join(filepath.Dir(wtDir), ".gitignore")
	b, err := os.ReadFile(ignorePath)
	require.NoError(t, err)
	require.Equal(t, "*\n", string(b))

	before, err := os.Stat(ignorePath)
	require.NoError(t, err)
	_, err = NewWorkspace(repo, wtDir)
	require.NoError(t, err)
	after, err := os.Stat(ignorePath)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime())
}

// TestDiffIgnoresUserDiffConfig verifies Workspace.Diff returns a plain
// unified diff even when the user's global git config forces an external
// diff driver and always-on color: the parsed diff keeps its a/ b/
// headers and carries no external-tool output or escape sequences.
func TestDiffIgnoresUserDiffConfig(t *testing.T) {
	// A shell script only runs on Unix; on Windows git would fail to
	// invoke it, so the external-diff half of the test cannot apply.
	if runtime.GOOS == "windows" {
		t.Skip("diff.external script requires a Unix shell")
	}

	repo := newTestRepo(t)

	// A global config that forces an external diff driver (a script
	// that prints EXTERNAL) and always-on color.
	dir := t.TempDir()
	script := filepath.Join(dir, "ext-diff.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho EXTERNAL\n"), 0o755))
	global := filepath.Join(dir, "gitconfig")
	require.NoError(t, os.WriteFile(global,
		[]byte("[diff]\n\texternal = "+script+"\n[color]\n\tui = always\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	ctx := t.Context()
	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)
	entry, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)

	// A committed change and an uncommitted edit: both must appear.
	write(t, filepath.Join(entry.Path, "committed.txt"), "committed")
	gitIn(t, entry.Path, "add", "-A")
	gitIn(t, entry.Path, "commit", "-qm", "dispatched work")
	write(t, filepath.Join(entry.Path, "f.txt"), "one edited")

	diff, err := ws.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "+++ b/")
	require.Contains(t, diff, "committed.txt")
	require.NotContains(t, diff, "EXTERNAL")
	require.NotContains(t, diff, "\x1b")

	require.NoError(t, ws.Remove(ctx, entry.ID))
}

// TestGitEnvScrubbed verifies the workspace lifecycle ignores git
// variables inherited from the environment (here GIT_DIR, GIT_WORK_TREE
// and GIT_INDEX_FILE all pointing at a different repository) and acts
// only on the repository the workspace was given, leaving the other one
// untouched.
func TestGitEnvScrubbed(t *testing.T) {
	decoy := newTestRepo(t)
	repo := newTestRepo(t)

	// Point every inherited git variable at the decoy repository. A
	// command that honoured them would act on the decoy, not repo.
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))

	ctx := t.Context()
	ws, err := newWorkspace(t, repo)
	require.NoError(t, err)
	entry, err := ws.Provision(ctx, ProvisionOptions{})
	require.NoError(t, err)

	// An uncommitted change in the dispatch worktree must appear in the
	// diff, proving the diff acted on the dispatch repository.
	write(t, filepath.Join(entry.Path, "dispatched.txt"), "dispatched")

	diff, err := ws.Diff(ctx, entry.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "dispatched.txt")
	require.Contains(t, diff, "+dispatched")

	require.NoError(t, ws.Remove(ctx, entry.ID))

	// The decoy was never touched: no dispatch branch and a clean tree.
	require.False(t, branchExists(t, decoy, entry.Branch))
	require.Empty(t, strings.TrimSpace(gitIn(t, decoy, "status", "--porcelain")),
		"decoy repository was modified")
}
