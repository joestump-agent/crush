package dispatch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/lock"
)

// releaseLease gives up the provider's hold on a provisioned
// workspace's lease without tearing the workspace down, exactly as the
// owning process dying would: the lock file stays, the lease is free,
// the marker and worktree remain for salvage.
func releaseLease(t *testing.T, p *GitWorktreeProvider, id string) {
	t.Helper()
	p.mu.Lock()
	rel := p.leases[id]
	delete(p.leases, id)
	p.mu.Unlock()
	require.NotNil(t, rel, "workspace %s has no lease to release", id)
	rel()
}

// leaseHelperEnv marks a test binary re-executed as a lease holder,
// and leaseHelperPathEnv names the lock file it holds.
const (
	leaseHelperEnv     = "GO_DISPATCH_LEASE_HELPER"
	leaseHelperPathEnv = "GO_DISPATCH_LEASE_PATH"
)

// TestLeaseHelperProcess is the re-executed child of holdLeaseElsewhere:
// it takes the lease named in the environment and holds it until the
// parent kills it.
func TestLeaseHelperProcess(t *testing.T) {
	if os.Getenv(leaseHelperEnv) != "1" {
		t.Skip("lease-holder child of holdLeaseElsewhere")
	}
	release, err := lock.TryFile(os.Getenv(leaseHelperPathEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lease holder child: %v\n", err)
		os.Exit(3)
	}
	defer release()
	fmt.Println("locked")
	time.Sleep(10 * time.Minute)
}

// holdLeaseElsewhere holds the lease on path from a child process for
// the duration of the test, killing it at cleanup. A lease held in
// this process does not do: on darwin, flock is per-process, so a
// probe from the same process takes the lock without contention and
// reports the owner dead.
func holdLeaseElsewhere(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$")
	cmd.Env = append(os.Environ(), leaseHelperEnv+"=1", leaseHelperPathEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err, "lease holder child did not start: %q", line)
	require.Equal(t, "locked", strings.TrimSpace(line))

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

// scanTestEnv builds a repository and a provider whose worktrees live
// outside it, so scan fixtures never fight the ignore file.
type scanTestEnv struct {
	repo string
	dir  string
	p    *GitWorktreeProvider
}

func newScanTestEnv(t *testing.T) scanTestEnv {
	t.Helper()
	repo := newTestRepo(t)
	dir := filepath.Join(t.TempDir(), "worktrees")
	p, err := NewGitWorktreeProvider(repo, dir, NewAgentRegistry())
	require.NoError(t, err)
	return scanTestEnv{repo: repo, dir: dir, p: p}
}

// ScanWorkspaces reports every dispatch workspace with its liveness,
// identity and changes: a held lease is a live owner, a free one is a
// dead owner, a missing lock file is unknown, and work is commits
// ahead of the recorded base or a dirty tree (#369).
func TestScanWorkspaces(t *testing.T) {
	env := newScanTestEnv(t)
	ctx := t.Context()

	alive := provisionEntry(t, env.p, ProvisionOptions{})
	dead := provisionEntry(t, env.p, ProvisionOptions{})
	deadChanges := provisionEntry(t, env.p, ProvisionOptions{})
	noLock := provisionEntry(t, env.p, ProvisionOptions{})
	noMarker := provisionEntry(t, env.p, ProvisionOptions{})

	// Identity stamps land on the marker and come back out of a scan.
	require.NoError(t, env.p.UpdateOwnerIdentity(dead.ID, "tester", "fix the flaky test"))
	require.NoError(t, env.p.SetDisposition(dead.ID, DispositionDismissed))
	require.NoError(t, env.p.SetDisposition(alive.ID, DispositionApplied))

	releaseLease(t, env.p, alive.ID)
	holdLeaseElsewhere(t, env.p.leasePath(alive.Branch))
	releaseLease(t, env.p, dead.ID)
	releaseLease(t, env.p, deadChanges.ID)
	releaseLease(t, env.p, noLock.ID)
	releaseLease(t, env.p, noMarker.ID)

	// deadChanges holds work: an untracked file and a commit on top of
	// the recorded base.
	write(t, filepath.Join(deadChanges.Path, "work.txt"), "half-finished")
	gitIn(t, deadChanges.Path, "add", "-A")
	commitIn(t, deadChanges.Path, "dispatched work")

	// noLock loses its lease file entirely: ownership unprovable.
	require.NoError(t, os.Remove(env.p.leasePath(noLock.Branch)))
	// noMarker loses its marker: the disposition column has nothing to
	// show and the default prune mode will not touch it.
	require.NoError(t, os.Remove(env.p.ownerPath(noMarker.Branch)))

	infos, err := ScanWorkspaces(ctx, env.dir, env.repo)
	require.NoError(t, err)
	byBranch := make(map[string]WorkspaceInfo, len(infos))
	for _, w := range infos {
		byBranch[w.Branch] = w
	}

	t.Run("live owner", func(t *testing.T) {
		w := byBranch[alive.Branch]
		require.True(t, w.HasLock)
		require.True(t, w.OwnerAlive)
		require.Equal(t, "yes", w.OwnerAliveState())
		require.True(t, w.HasMarker)
		require.False(t, w.HasChanges, "a provisioned workspace is clean")
		require.Equal(t, DispositionApplied, w.Disposition)
		require.Equal(t, alive.ID, w.ID)
		require.Equal(t, alive.Path, w.Path)
	})

	t.Run("dead owner with identity", func(t *testing.T) {
		w := byBranch[dead.Branch]
		require.True(t, w.HasLock)
		require.False(t, w.OwnerAlive)
		require.Equal(t, "no", w.OwnerAliveState())
		require.Equal(t, "tester", w.Handle, "the assigned handle is scanned from the marker")
		require.Equal(t, "fix the flaky test", w.Role)
		require.Equal(t, DispositionDismissed, w.Disposition)
		require.Equal(t, dead.BaseSHA, w.BaseSHA)
		require.False(t, w.HasChanges)
	})

	t.Run("dead owner with changes", func(t *testing.T) {
		w := byBranch[deadChanges.Branch]
		require.False(t, w.OwnerAlive)
		require.True(t, w.HasChanges, "uncommitted work counts as changes")
	})

	t.Run("missing lock file is unknown", func(t *testing.T) {
		w := byBranch[noLock.Branch]
		require.False(t, w.HasLock)
		require.False(t, w.OwnerAlive, "a missing lock file is unknown, not dead")
		require.Equal(t, "unknown", w.OwnerAliveState())
	})

	t.Run("missing marker", func(t *testing.T) {
		w := byBranch[noMarker.Branch]
		require.False(t, w.HasMarker)
		require.Empty(t, w.Handle)
		require.Empty(t, w.Disposition)
		require.True(t, w.HasChanges, "an unknown base counts as changes: the fail-safe keeps it")
	})
}

// A missing worktrees directory scans as empty — a repository with no
// dispatched agents has nothing to list or prune, not an error (#369).
func TestScanWorkspacesMissingDir(t *testing.T) {
	repo := newTestRepo(t)
	infos, err := ScanWorkspaces(t.Context(), filepath.Join(t.TempDir(), "nope"), repo)
	require.NoError(t, err)
	require.Empty(t, infos)
}

// Non-workspace entries in the worktrees directory — lock files,
// markers, gitignore — are not workspaces and do not confuse the scan.
func TestScanWorkspacesIgnoresNonWorkspaceEntries(t *testing.T) {
	env := newScanTestEnv(t)
	entry := provisionEntry(t, env.p, ProvisionOptions{})
	releaseLease(t, env.p, entry.ID)

	infos, err := ScanWorkspaces(t.Context(), env.dir, env.repo)
	require.NoError(t, err)
	require.Len(t, infos, 1, "only the workspace directory is a workspace")
}

// WorkspacePruner.Remove tears a dead-owner workspace completely down
// — worktree, branch, marker — while leaving the lock file in place:
// flock is keyed by inode, so unlinking it could let two processes
// lock different inodes at the same path (#369).
func TestWorkspacePrunerRemovesDeadOwnerWorkspace(t *testing.T) {
	env := newScanTestEnv(t)
	entry := provisionEntry(t, env.p, ProvisionOptions{})
	releaseLease(t, env.p, entry.ID)

	infos, err := ScanWorkspaces(t.Context(), env.dir, env.repo)
	require.NoError(t, err)
	require.Len(t, infos, 1)

	pruner, err := NewWorkspacePruner(env.repo, env.dir)
	require.NoError(t, err)
	require.NoError(t, pruner.Remove(t.Context(), infos[0]))

	_, err = os.Stat(infos[0].Path)
	require.ErrorIs(t, err, os.ErrNotExist, "the worktree directory is gone")
	require.False(t, branchExists(t, env.repo, entry.Branch), "the branch is gone")
	_, err = os.Stat(env.p.ownerPath(entry.Branch))
	require.ErrorIs(t, err, os.ErrNotExist, "the owner marker is gone")
	_, err = os.Stat(env.p.leasePath(entry.Branch))
	require.NoError(t, err, "the lock file stays: flock is keyed by inode")

	// Removal is idempotent, like Release.
	require.NoError(t, pruner.Remove(t.Context(), infos[0]))
}

// A pruner refuses a workspace whose owner is alive and one whose
// ownership cannot be proven — in both cases nothing on disk changes.
func TestWorkspacePrunerRefusesLiveAndUnprovable(t *testing.T) {
	env := newScanTestEnv(t)
	alive := provisionEntry(t, env.p, ProvisionOptions{})
	noLock := provisionEntry(t, env.p, ProvisionOptions{})
	releaseLease(t, env.p, noLock.ID)
	require.NoError(t, os.Remove(env.p.leasePath(noLock.Branch)))
	releaseLease(t, env.p, alive.ID)
	holdLeaseElsewhere(t, env.p.leasePath(alive.Branch))

	pruner, err := NewWorkspacePruner(env.repo, env.dir)
	require.NoError(t, err)

	infos, err := ScanWorkspaces(t.Context(), env.dir, env.repo)
	require.NoError(t, err)
	byBranch := make(map[string]WorkspaceInfo, len(infos))
	for _, w := range infos {
		byBranch[w.Branch] = w
	}

	err = pruner.Remove(t.Context(), byBranch[alive.Branch])
	require.ErrorContains(t, err, "owner is alive")
	err = pruner.Remove(t.Context(), byBranch[noLock.Branch])
	require.ErrorContains(t, err, "ownership cannot be proven")

	_, err = os.Stat(alive.Path)
	require.NoError(t, err, "a live owner's workspace is untouched")
	_, err = os.Stat(noLock.Path)
	require.NoError(t, err, "an unprovable workspace is untouched")
	require.True(t, branchExists(t, env.repo, alive.Branch))
}

// The pruner re-acquires the lease at removal time, so a workspace
// whose owner appeared between scan and prune is refused.
func TestWorkspacePrunerRechecksLeaseAtRemoval(t *testing.T) {
	env := newScanTestEnv(t)
	entry := provisionEntry(t, env.p, ProvisionOptions{})
	releaseLease(t, env.p, entry.ID)

	infos, err := ScanWorkspaces(t.Context(), env.dir, env.repo)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	require.False(t, infos[0].OwnerAlive)

	// The owner comes back: the free lease is taken again, by another
	// process.
	holdLeaseElsewhere(t, env.p.leasePath(entry.Branch))

	pruner, err := NewWorkspacePruner(env.repo, env.dir)
	require.NoError(t, err)
	err = pruner.Remove(t.Context(), infos[0])
	require.ErrorContains(t, err, "owner is alive")

	_, err = os.Stat(entry.Path)
	require.NoError(t, err, "the workspace is untouched")
}

// A pruner against a directory that is not a git repository refuses to
// build, the same clear error the provider construction gives.
func TestWorkspacePrunerRequiresGitRepository(t *testing.T) {
	_, err := NewWorkspacePruner(t.TempDir(), t.TempDir())
	require.ErrorContains(t, err, "is not a git repository")
}

// UpdateOwnerIdentity and SetDisposition are best-effort stamps on the
// marker: an unknown dispatch is an error, and a missing marker is a
// no-op — recreating it would lose the base SHA salvage keys on.
func TestUpdateOwnerMarkerStamps(t *testing.T) {
	env := newScanTestEnv(t)

	err := env.p.UpdateOwnerIdentity("unknown-id", "x", "y")
	require.Error(t, err, "an unknown dispatch cannot be stamped")

	entry := provisionEntry(t, env.p, ProvisionOptions{})
	require.NoError(t, env.p.UpdateOwnerIdentity(entry.ID, "tester", "role"))
	require.NoError(t, env.p.SetDisposition(entry.ID, DispositionApplied))

	data, err := os.ReadFile(env.p.ownerPath(entry.Branch))
	require.NoError(t, err)
	var m ownerMarker
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "tester", m.Handle)
	require.Equal(t, "role", m.Role)
	require.Equal(t, DispositionApplied, m.Disposition)
	require.Equal(t, entry.BaseSHA, m.BaseSHA, "the stamp keeps the recorded base")

	// A missing marker makes the stamp a no-op, not a partial recreate.
	require.NoError(t, os.Remove(env.p.ownerPath(entry.Branch)))
	require.NoError(t, env.p.UpdateOwnerIdentity(entry.ID, "again", "role"))
	_, err = os.Stat(env.p.ownerPath(entry.Branch))
	require.ErrorIs(t, err, os.ErrNotExist, "no partial marker is recreated")
}
