package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/dispatch"
)

// dispatchInitRepo creates a one-commit git repo in a temp dir for
// workspace provisioning tests.
func dispatchInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	git("init", "-b", "main")
	git("config", "user.email", "test@crush.test")
	git("config", "user.name", "Crush Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644))
	git("add", "-A")
	git("commit", "-m", "initial")
	return dir
}

// gitDispatchCoordinator returns a bare coordinator wired only with a
// git-backed dispatch registry — enough to exercise result assembly and
// tool validation without the full provider-dependent construction.
func gitDispatchCoordinator(t *testing.T) *coordinator {
	t.Helper()
	repo := dispatchInitRepo(t)
	reg := dispatch.NewRegistry(filepath.Join(t.TempDir(), "dispatch"), dispatch.NewGitBackend(repo))
	return &coordinator{dispatchRegistry: reg}
}

func runDispatchTool(t *testing.T, c *coordinator, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := c.dispatchAgentTool().Run(t.Context(), fantasy.ToolCall{
		ID:    "dispatch-1",
		Name:  DispatchAgentToolName,
		Input: input,
	})
	require.NoError(t, err)
	return resp
}

func TestDispatchAgentTool_RequiresPrompt(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	resp := runDispatchTool(t, c, `{"prompt":""}`)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "prompt is required")
}

func TestDispatchAgentTool_UnavailableWithoutRegistry(t *testing.T) {
	t.Parallel()
	c := &coordinator{} // nil dispatchRegistry
	resp := runDispatchTool(t, c, `{"prompt":"do something"}`)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "unavailable")
}

func TestAssembleResult_RunErrorIsFailure(t *testing.T) {
	t.Parallel()
	c := gitDispatchCoordinator(t)
	ws, err := c.dispatchRegistry.Create(context.Background(), "")
	require.NoError(t, err)

	res := c.assembleResult(context.Background(), ws, "sess-1", nil, errors.New("model exploded"))
	require.Equal(t, "failed", res.Status)
	require.Equal(t, "model exploded", res.Error)
	require.Equal(t, ws.ID, res.DispatchID)
	require.Equal(t, "sess-1", res.SessionID)
	require.Empty(t, res.Diff)

	tracked, ok := c.dispatchRegistry.Get(ws.ID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusFailed, tracked.Status)
}

func TestAssembleResult_NilResultIsFailure(t *testing.T) {
	t.Parallel()
	c := gitDispatchCoordinator(t)
	ws, err := c.dispatchRegistry.Create(context.Background(), "")
	require.NoError(t, err)

	// Run returning (nil, nil) means no turn ran — a failure, never a
	// silent success.
	res := c.assembleResult(context.Background(), ws, "sess-1", nil, nil)
	require.Equal(t, "failed", res.Status)
	require.Contains(t, res.Error, "did not start a turn")

	tracked, _ := c.dispatchRegistry.Get(ws.ID)
	require.Equal(t, dispatch.StatusFailed, tracked.Status)
}

func TestAssembleResult_SuccessCapturesFindingsAndDiff(t *testing.T) {
	t.Parallel()
	c := gitDispatchCoordinator(t)
	ws, err := c.dispatchRegistry.Create(context.Background(), "")
	require.NoError(t, err)

	// The dispatched agent's work product: a new file in the workspace.
	require.NoError(t, os.WriteFile(filepath.Join(ws.Path, "feature.go"), []byte("package feature\n"), 0o644))

	result := &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: "added the feature"}},
		},
	}
	res := c.assembleResult(context.Background(), ws, "sess-1", result, nil)

	require.Equal(t, "completed", res.Status)
	require.Equal(t, "added the feature", res.Findings)
	require.Contains(t, res.Diff, "feature.go")
	require.Empty(t, res.Error)

	tracked, _ := c.dispatchRegistry.Get(ws.ID)
	require.Equal(t, dispatch.StatusComplete, tracked.Status)
}

func TestFailedResult_PopulatesReason(t *testing.T) {
	t.Parallel()
	ws := dispatch.Workspace{ID: "abc", Path: "/tmp/ws"}
	c := &coordinator{}
	res := c.failedResult(ws, "sess-9", "boom")
	require.Equal(t, "failed", res.Status)
	require.Equal(t, "boom", res.Error)
	require.Equal(t, "abc", res.DispatchID)
	require.Equal(t, "sess-9", res.SessionID)
	require.Equal(t, "/tmp/ws", res.WorkspacePath)
}

// TestBuildDispatchAgent_Constructs is a construction smoke test for the
// dispatched agent: small and large model selection both yield a live
// ephemeral agent rooted at the given workspace. Like the other
// coordinator tests it needs a provider catalog, so it runs in CI rather
// than the offline sandbox.
func TestBuildDispatchAgent_Constructs(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")
	ws := t.TempDir()

	small, smallModel, err := coord.buildDispatchAgent(t.Context(), ws, "small")
	require.NoError(t, err)
	require.NotNil(t, small)
	require.NotNil(t, small.SessionAgent)
	require.NotEmpty(t, smallModel.ModelCfg.Model)

	large, largeModel, err := coord.buildDispatchAgent(t.Context(), ws, "large")
	require.NoError(t, err)
	require.NotNil(t, large)
	require.NotEmpty(t, largeModel.ModelCfg.Model)
}

func TestDispatchResult_JSONShape(t *testing.T) {
	t.Parallel()
	res := DispatchResult{
		DispatchID:    "abc",
		SessionID:     "sess",
		WorkspacePath: "/tmp/ws",
		Status:        "completed",
		Findings:      "done",
		Diff:          "diff --git ...",
	}
	b, err := json.Marshal(res)
	require.NoError(t, err)
	var round map[string]any
	require.NoError(t, json.Unmarshal(b, &round))
	require.Equal(t, "abc", round["dispatch_id"])
	require.Equal(t, "completed", round["status"])
	require.NotContains(t, string(b), "\"error\"", "empty error should be omitted")
}
