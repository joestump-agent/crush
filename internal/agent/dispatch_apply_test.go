package agent

// The apply and dismiss tools (#368): the model's front door for the
// review decision a finished dispatch's terminal message asks for. The
// fixtures dispatch real workspaces through the real dispatch tool, put
// committed and uncommitted work in them, and drive the tools end to
// end — git included. They commit with the user's own git config, so
// every test pins the global config to an empty file, and the fixtures
// rely on the repository-local identity initGitRepo sets.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// sealGitConfig pins the global and system git config to nothing for the
// duration of the test: the apply path commits as the user, and CI and
// agent hosts must not contribute identity or hooks of their own. Must
// be called before any git runs; it uses t.Setenv, so tests calling it
// cannot be parallel. The repository-local identity comes from
// initGitRepo, which the worktree shares.
func sealGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// gitOut runs one git command in dir and returns its trimmed output,
// failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// finishedDispatchFixture dispatches one agent through the real
// dispatch tool, lets the test put work in the workspace while the
// agent is parked, releases it, and waits for the dispatch to finish,
// returning the coordinator and the terminal entry. The workspace's
// path is entry.Path.
func finishedDispatchFixture(t *testing.T, mutate func(workspacePath string)) (*coordinator, dispatch.Entry) {
	t.Helper()
	sealGitConfig(t)
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()
	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)
	if mutate != nil {
		mutate(handle.WorkspacePath)
	}
	agent.release()
	entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusCompleted)
	return c, entry
}

// requireWorkspaceGone asserts the full teardown: worktree directory,
// branch, and registry entry (which frees the handle).
func requireWorkspaceGone(t *testing.T, c *coordinator, entry dispatch.Entry, handle string) {
	t.Helper()
	_, err := os.Stat(entry.Path)
	require.True(t, os.IsNotExist(err), "the worktree directory should be gone")
	require.Empty(t, gitOut(t, c.cfg.WorkingDir(), "branch", "--list", entry.Branch),
		"the dispatch branch should be gone")
	_, ok := c.dispatchRegistry().Get(entry.ID)
	require.False(t, ok, "the registry entry should be gone")
	_, ok = c.dispatchRegistry().ByHandle(handle)
	require.False(t, ok, "the handle should be freed")
}

// dismiss_dispatch removes the worktree, the branch, and the registry
// entry, and frees the handle — the full teardown, by ID and by handle.
func TestDismissDispatchRemovesWorkspace(t *testing.T) {
	tests := []struct {
		name   string
		params func(dispatch.Entry) DismissDispatchParams
	}{
		{
			name:   "by dispatch id",
			params: func(e dispatch.Entry) DismissDispatchParams { return DismissDispatchParams{DispatchID: e.ID} },
		},
		{
			name: "by handle",
			params: func(e dispatch.Entry) DismissDispatchParams {
				return DismissDispatchParams{Handle: e.Handle}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, entry := finishedDispatchFixture(t, func(ws string) {
				require.NoError(t, os.WriteFile(filepath.Join(ws, "doomed.txt"), []byte("discard me"), 0o644))
			})

			tool := c.dismissDispatchTool()
			resp := runTool(t, tool, DismissDispatchToolName, tt.params(entry))
			require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
			require.Contains(t, resp.Content, "dismissed")
			requireWorkspaceGone(t, c, entry, entry.Handle)
		})
	}
}

// An unknown dispatch refuses, and a dispatch that is still running
// refuses with the cancel hint — neither tool races a live run.
func TestApplyAndDismissRefuseUnknownAndRunning(t *testing.T) {
	sealGitConfig(t)
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()
	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)

	applyTool := c.applyDispatchTool()
	resp := runTool(t, applyTool, ApplyDispatchToolName, ApplyDispatchParams{DispatchID: handle.DispatchID})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, "still running")
	require.Contains(t, resp.Content, "cancel it first")

	dismissTool := c.dismissDispatchTool()
	resp = runTool(t, dismissTool, DismissDispatchToolName, DismissDispatchParams{DispatchID: handle.DispatchID})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, "still running")

	resp = runTool(t, applyTool, ApplyDispatchToolName, ApplyDispatchParams{DispatchID: "no-such-dispatch"})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, "no dispatch")

	resp = runTool(t, dismissTool, DismissDispatchToolName, DismissDispatchParams{DispatchID: "no-such-dispatch"})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, "no dispatch")

	// Both params at once is refused too: exactly one address.
	resp = runTool(t, applyTool, ApplyDispatchToolName, ApplyDispatchParams{DispatchID: "a", Handle: "b"})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, "exactly one")

	// The gated dispatch never noticed any of this.
	agent.release()
	waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusCompleted)
}

// Every apply mode brings both the committed and the uncommitted
// workspace work into the parent checkout: merge lands a merge commit,
// squash stages without committing, cherry-pick replays the commits
// since the base. The workspace is removed afterwards either way.
func TestApplyDispatchBringsCommittedAndUncommittedWork(t *testing.T) {
	tests := []struct {
		mode string
	}{
		{mode: "merge"},
		{mode: "squash"},
		{mode: "cherry-pick"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			c, entry := finishedDispatchFixture(t, func(ws string) {
				require.NoError(t, os.WriteFile(filepath.Join(ws, "committed.txt"), []byte("committed work"), 0o644))
				gitOut(t, ws, "add", "committed.txt")
				gitOut(t, ws, "commit", "-m", "dispatch: committed work")
				require.NoError(t, os.WriteFile(filepath.Join(ws, "uncommitted.txt"), []byte("uncommitted work"), 0o644))
			})
			repoRoot := c.cfg.WorkingDir()
			headBefore := gitOut(t, repoRoot, "rev-parse", "HEAD")

			tool := c.applyDispatchTool()
			resp := runTool(t, tool, ApplyDispatchToolName, ApplyDispatchParams{DispatchID: entry.ID, Mode: tt.mode})
			require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)

			// Both files are in the parent checkout.
			require.FileExists(t, filepath.Join(repoRoot, "committed.txt"))
			require.FileExists(t, filepath.Join(repoRoot, "uncommitted.txt"))

			// The workspace is removed either way.
			_, err := os.Stat(entry.Path)
			require.True(t, os.IsNotExist(err), "the worktree should be gone")
			_, ok := c.dispatchRegistry().Get(entry.ID)
			require.False(t, ok, "the registry entry should be gone")

			switch tt.mode {
			case "merge":
				_, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "rev-parse", "HEAD^2").Output()
				require.NoError(t, err, "a merge apply leaves a merge commit")
			case "squash":
				require.Equal(t, headBefore, gitOut(t, repoRoot, "rev-parse", "HEAD"),
					"a squash apply does not commit")
				status := gitOut(t, repoRoot, "status", "--porcelain")
				require.Contains(t, status, "committed.txt", "the squash result is staged")
				require.Contains(t, resp.Content, "staged", "the report says the result is not committed")
			case "cherry-pick":
				require.NotEqual(t, headBefore, gitOut(t, repoRoot, "rev-parse", "HEAD"),
					"a cherry-pick apply commits the picked work")
				require.Contains(t, gitOut(t, repoRoot, "log", "--format=%s"), "dispatch: committed work")
			}
		})
	}
}

// A dirty parent is refused before anything changes.
func TestApplyDispatchRefusesDirtyParent(t *testing.T) {
	c, entry := finishedDispatchFixture(t, nil)
	repoRoot := c.cfg.WorkingDir()
	dirty := filepath.Join(repoRoot, "dirty.txt")
	require.NoError(t, os.WriteFile(dirty, []byte("parent work"), 0o644))
	t.Cleanup(func() { _ = os.Remove(dirty) })

	tool := c.applyDispatchTool()
	resp := runTool(t, tool, ApplyDispatchToolName, ApplyDispatchParams{DispatchID: entry.ID})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, "dirty")

	// Nothing changed: the workspace and the entry survive untouched.
	require.DirExists(t, entry.Path)
	_, ok := c.dispatchRegistry().Get(entry.ID)
	require.True(t, ok, "a refusal leaves the dispatch in the registry")
}

// A conflicting apply aborts and lists the conflicting paths, and the
// parent is left exactly as it was: no MERGE_HEAD, the same clean
// status, the same HEAD.
func TestApplyDispatchConflictAbortsCleanly(t *testing.T) {
	c, entry := finishedDispatchFixture(t, func(ws string) {
		require.NoError(t, os.WriteFile(filepath.Join(ws, "f.txt"), []byte("dispatch side"), 0o644))
		gitOut(t, ws, "add", "-A")
		gitOut(t, ws, "commit", "-m", "dispatch: conflicting change")
	})
	repoRoot := c.cfg.WorkingDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "f.txt"), []byte("parent side"), 0o644))
	gitOut(t, repoRoot, "add", "-A")
	gitOut(t, repoRoot, "commit", "-m", "parent: conflicting change")
	headBefore := gitOut(t, repoRoot, "rev-parse", "HEAD")

	tool := c.applyDispatchTool()
	resp := runTool(t, tool, ApplyDispatchToolName, ApplyDispatchParams{DispatchID: entry.ID})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, "conflicts")
	require.Contains(t, resp.Content, "f.txt")

	// The parent is exactly as it was.
	require.Equal(t, headBefore, gitOut(t, repoRoot, "rev-parse", "HEAD"))
	require.Empty(t, gitOut(t, repoRoot, "status", "--porcelain"))
	mergeHead, err := gitQuietPath(t, repoRoot, "MERGE_HEAD")
	require.NoError(t, err)
	require.NoFileExists(t, mergeHead, "no merge state survives the abort")
}

// gitQuietPath resolves a git-path marker to its absolute location
// without failing when it is absent.
func gitQuietPath(t *testing.T, repoRoot, marker string) (string, error) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "rev-parse", "--git-path", marker).Output()
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	return path, nil
}

// A denied permission refuses both tools and changes nothing.
func TestApplyAndDismissPermissionDenied(t *testing.T) {
	sealGitConfig(t)
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	// The parent service starts in prompting mode; the test plays the
	// human and denies every request.
	c.permissions = permission.NewPermissionService(c.cfg.WorkingDir(), false, nil)
	tool := c.dispatchTool()
	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)
	agent.release()
	entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusCompleted)

	events := c.permissions.Subscribe(t.Context())
	type outcome struct {
		resp fantasy.ToolResponse
	}
	denyFirst := func(run func() fantasy.ToolResponse) fantasy.ToolResponse {
		resCh := make(chan outcome, 1)
		go func() { resCh <- outcome{resp: run()} }()
		var ev pubsub.Event[permission.PermissionRequest]
		require.Eventually(t, func() bool {
			select {
			case ev = <-events:
				return true
			default:
				return false
			}
		}, 5*time.Second, 20*time.Millisecond, "the tool should ask the parent for permission")
		require.True(t, c.permissions.Deny(ev.Payload))
		res := <-resCh
		return res.resp
	}

	applyResp := denyFirst(func() fantasy.ToolResponse {
		return runTool(t, c.applyDispatchTool(), ApplyDispatchToolName, ApplyDispatchParams{DispatchID: entry.ID})
	})
	require.True(t, applyResp.IsError, "a denied apply must refuse: %s", applyResp.Content)

	dismissResp := denyFirst(func() fantasy.ToolResponse {
		return runTool(t, c.dismissDispatchTool(), DismissDispatchToolName, DismissDispatchParams{DispatchID: entry.ID})
	})
	require.True(t, dismissResp.IsError, "a denied dismiss must refuse: %s", dismissResp.Content)

	// Nothing changed: the workspace and the entry survive untouched.
	require.DirExists(t, entry.Path)
	_, ok := c.dispatchRegistry().Get(entry.ID)
	require.True(t, ok, "a denial leaves the dispatch in the registry")
}
