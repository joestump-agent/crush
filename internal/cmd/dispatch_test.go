package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/lock"
)

// newDispatchCLIRepo builds a git repository with one commit, hermetic
// identity, for dispatch command tests.
func newDispatchCLIRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", repo}, args...)
		out, err := exec.CommandContext(t.Context(), "git", full...).CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	run("init", "-q")
	run("config", "user.email", "dispatch-cli-test@example.com")
	run("config", "user.name", "dispatch cli test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one"), 0o644))
	run("add", "-A")
	run("-c", "commit.gpgsign=false", "commit", "-qm", "initial")
	return repo
}

// addDispatchFixture carves a real worktree on a dispatch branch and
// writes its owner marker, so the scan's git probes see a genuine
// workspace. It returns the scanned WorkspaceInfo's branch.
func addDispatchFixture(t *testing.T, repo, wtDir, name, disposition, handle string) string {
	t.Helper()
	branch := dispatch.BranchPrefix + name
	path := filepath.Join(wtDir, branch)
	out, err := exec.CommandContext(t.Context(), "git", "-C", repo,
		"worktree", "add", "--no-track", "-b", branch, "--", path, "HEAD").CombinedOutput()
	require.NoError(t, err, "git worktree add: %s", out)

	head, err := exec.CommandContext(t.Context(), "git", "-C", repo, "rev-parse", "HEAD").Output()
	require.NoError(t, err)

	marker := map[string]any{
		"instance_id": "test-instance",
		"pid":         4242,
		"created_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"base_sha":    strings.TrimSpace(string(head)),
	}
	if handle != "" {
		marker["handle"] = handle
		marker["role"] = "Fixer"
	}
	if disposition != "" {
		marker["disposition"] = disposition
	}
	data, err := json.Marshal(marker)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path+".owner.json", data, 0o644))
	require.NoError(t, os.WriteFile(path+".lock", nil, 0o600))
	return branch
}

// runDispatchList runs `crush dispatch list` with the given flag
// values, resetting the --json flag afterwards.
func runDispatchList(t *testing.T, repo, dataDir string, json bool) string {
	t.Helper()
	dispatchListCmd.SetContext(context.Background())
	require.NoError(t, dispatchListCmd.Flags().Set("json", map[bool]string{true: "true", false: "false"}[json]))
	require.NoError(t, dispatchListCmd.ParseFlags([]string{"--cwd", repo, "--data-dir", dataDir}))
	t.Cleanup(func() {
		_ = dispatchListCmd.Flags().Set("json", "false")
		_ = dispatchListCmd.ParseFlags([]string{"--cwd", "", "--data-dir", ""})
	})

	var b strings.Builder
	dispatchListCmd.SetOut(&b)
	dispatchListCmd.SetErr(&b)
	t.Cleanup(func() {
		dispatchListCmd.SetOut(nil)
		dispatchListCmd.SetErr(nil)
	})
	require.NoError(t, dispatchListCmd.RunE(dispatchListCmd, nil))
	return b.String()
}

// runDispatchPrune runs `crush dispatch prune` with the given flags.
func runDispatchPrune(t *testing.T, repo, dataDir string, allDead, force, dryRun bool) string {
	t.Helper()
	dispatchPruneCmd.SetContext(context.Background())
	set := func(name string, v bool) {
		require.NoError(t, dispatchPruneCmd.Flags().Set(name, map[bool]string{true: "true", false: "false"}[v]))
	}
	set("all-dead", allDead)
	set("force", force)
	set("dry-run", dryRun)
	require.NoError(t, dispatchPruneCmd.ParseFlags([]string{"--cwd", repo, "--data-dir", dataDir}))
	t.Cleanup(func() {
		set("all-dead", false)
		set("force", false)
		set("dry-run", false)
		_ = dispatchPruneCmd.ParseFlags([]string{"--cwd", "", "--data-dir", ""})
	})

	var b strings.Builder
	dispatchPruneCmd.SetOut(&b)
	dispatchPruneCmd.SetErr(&b)
	t.Cleanup(func() {
		dispatchPruneCmd.SetOut(nil)
		dispatchPruneCmd.SetErr(nil)
	})
	require.NoError(t, dispatchPruneCmd.RunE(dispatchPruneCmd, nil))
	return b.String()
}

// List reports every workspace with all six columns, and --json emits
// the same data (#369).
func TestDispatchListCommand(t *testing.T) {
	repo := newDispatchCLIRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	wtDir, err := dispatch.WorktreesDir(dataDir, repo)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(wtDir, 0o755))

	aliveBranch := addDispatchFixture(t, repo, wtDir, "alive", dispatch.DispositionApplied, "tester")
	deadBranch := addDispatchFixture(t, repo, wtDir, "dead", "", "")

	// The live owner holds its lease; the dead one's is free.
	held, err := lock.TryFile(filepath.Join(wtDir, aliveBranch+".lock"))
	require.NoError(t, err)
	t.Cleanup(held)

	out := runDispatchList(t, repo, dataDir, false)
	for _, want := range []string{"alive", "dead", "tester", aliveBranch, deadBranch, "yes", "no"} {
		require.Contains(t, out, want, "list output must carry the six columns' data")
	}

	jsonOut := runDispatchList(t, repo, dataDir, true)
	var infos []dispatch.WorkspaceInfo
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(jsonOut)), &infos))
	require.Len(t, infos, 2, "--json emits the same data as the table")
	byBranch := make(map[string]dispatch.WorkspaceInfo, len(infos))
	for _, w := range infos {
		byBranch[w.Branch] = w
	}
	require.True(t, byBranch[aliveBranch].OwnerAlive)
	require.Equal(t, "tester", byBranch[aliveBranch].Handle)
	require.Equal(t, dispatch.DispositionApplied, byBranch[aliveBranch].Disposition)
	require.False(t, byBranch[deadBranch].OwnerAlive)
	require.False(t, byBranch[deadBranch].HasChanges)
}

// Prune removes dead-owner applied/dismissed entries, never a live
// owner's, and prints every removal and skip (#369).
func TestDispatchPruneDefault(t *testing.T) {
	repo := newDispatchCLIRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	wtDir, err := dispatch.WorktreesDir(dataDir, repo)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(wtDir, 0o755))

	dismissed := addDispatchFixture(t, repo, wtDir, "dismissed", dispatch.DispositionDismissed, "d1")
	applied := addDispatchFixture(t, repo, wtDir, "applied", dispatch.DispositionApplied, "d2")
	undecided := addDispatchFixture(t, repo, wtDir, "undecided", "", "d3")
	live := addDispatchFixture(t, repo, wtDir, "live", dispatch.DispositionDismissed, "d4")

	held, err := lock.TryFile(filepath.Join(wtDir, live+".lock"))
	require.NoError(t, err)
	t.Cleanup(held)

	out := runDispatchPrune(t, repo, dataDir, false, false, false)
	require.Contains(t, out, "removed "+dismissed)
	require.Contains(t, out, "removed "+applied)
	require.Contains(t, out, "skipped "+undecided)
	require.Contains(t, out, "skipped "+live)
	require.Contains(t, out, "owner is alive")

	for _, gone := range []string{dismissed, applied} {
		_, err := os.Stat(filepath.Join(wtDir, gone))
		require.ErrorIs(t, err, os.ErrNotExist)
		require.False(t, branchExistsCLI(t, repo, gone))
	}
	_, err = os.Stat(filepath.Join(wtDir, undecided))
	require.NoError(t, err, "an undecided workspace is skipped, not removed")
	_, err = os.Stat(filepath.Join(wtDir, live))
	require.NoError(t, err, "a live owner's workspace survives any flags")
	require.True(t, branchExistsCLI(t, repo, live))
}

// --all-dead takes every dead owner but skips ones holding changes;
// --force lifts that too (#369).
func TestDispatchPruneAllDead(t *testing.T) {
	repo := newDispatchCLIRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	wtDir, err := dispatch.WorktreesDir(dataDir, repo)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(wtDir, 0o755))

	clean := addDispatchFixture(t, repo, wtDir, "clean", "", "d1")
	changed := addDispatchFixture(t, repo, wtDir, "changed", dispatch.DispositionApplied, "d2")

	// changed holds uncommitted work.
	require.NoError(t, os.WriteFile(filepath.Join(wtDir, changed, "wip.txt"), []byte("half"), 0o644))

	out := runDispatchPrune(t, repo, dataDir, true, false, false)
	require.Contains(t, out, "removed "+clean)
	require.Contains(t, out, "skipped "+changed)
	require.Contains(t, out, "has unapplied changes")

	_, err = os.Stat(filepath.Join(wtDir, changed))
	require.NoError(t, err, "changes keep a dead workspace without --force")

	out = runDispatchPrune(t, repo, dataDir, true, true, false)
	require.Contains(t, out, "removed "+changed)
	_, err = os.Stat(filepath.Join(wtDir, changed))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// --dry-run prints the plan and deletes nothing (#369).
func TestDispatchPruneDryRun(t *testing.T) {
	repo := newDispatchCLIRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	wtDir, err := dispatch.WorktreesDir(dataDir, repo)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(wtDir, 0o755))

	branch := addDispatchFixture(t, repo, wtDir, "gone", dispatch.DispositionDismissed, "d1")

	out := runDispatchPrune(t, repo, dataDir, false, false, true)
	require.Contains(t, out, "would remove "+branch)

	_, err = os.Stat(filepath.Join(wtDir, branch))
	require.NoError(t, err, "a dry run deletes nothing")
	require.True(t, branchExistsCLI(t, repo, branch), "a dry run deletes no branch")
}

// With no worktrees directory, both commands print nothing and exit 0
// (#369).
func TestDispatchCommandsNoWorktrees(t *testing.T) {
	repo := newDispatchCLIRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")

	require.Empty(t, runDispatchList(t, repo, dataDir, false))
	require.Empty(t, runDispatchPrune(t, repo, dataDir, false, false, false))
}

// Outside a git repository, both commands fail with a clear error
// (#369).
func TestDispatchCommandsOutsideGitRepo(t *testing.T) {
	notRepo := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")

	require.NoError(t, dispatchListCmd.Flags().Set("json", "false"))
	require.NoError(t, dispatchListCmd.ParseFlags([]string{"--cwd", notRepo, "--data-dir", dataDir}))
	err := dispatchListCmd.RunE(dispatchListCmd, nil)
	require.ErrorContains(t, err, "not a git repository")

	require.NoError(t, dispatchPruneCmd.ParseFlags([]string{"--cwd", notRepo, "--data-dir", dataDir}))
	err = dispatchPruneCmd.RunE(dispatchPruneCmd, nil)
	require.ErrorContains(t, err, "not a git repository")
}

// branchExistsCLI is branchExists for the cmd package, decided by git's
// exit code.
func branchExistsCLI(t *testing.T, repo, branch string) bool {
	t.Helper()
	err := exec.CommandContext(t.Context(), "git", "-C", repo,
		"show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run()
	return err == nil
}
