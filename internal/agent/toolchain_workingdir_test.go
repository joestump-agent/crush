package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// findTool returns the built tool with the given name.
func findTool(t *testing.T, built []fantasy.AgentTool, name string) fantasy.AgentTool {
	t.Helper()
	for _, tool := range built {
		if tool.Info().Name == name {
			return tool
		}
	}
	require.Failf(t, "tool not built", "no %q tool in set", name)
	return nil
}

// runGlob invokes a glob tool with the given pattern rooted at the tool's
// working dir (empty path), returning the raw response content.
func runGlob(t *testing.T, tool fantasy.AgentTool, pattern string) string {
	t.Helper()
	input, err := json.Marshal(tools.GlobParams{Pattern: pattern})
	require.NoError(t, err)
	resp, err := tool.Run(t.Context(), fantasy.ToolCall{
		ID:    "glob-call",
		Name:  tools.GlobToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "glob returned error: %s", resp.Content)
	return resp.Content
}

// TestBuildToolsRootsToolchainAtWorkingDir pins the #62 isolation seam:
// a dispatched agent's toolchain is rooted at the workspace path passed
// to buildTools, so its file tools resolve inside that workspace — while
// an empty working dir is unchanged and resolves against the main tree.
func TestBuildToolsRootsToolchainAtWorkingDir(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	// An isolated workspace holding a file that exists nowhere else.
	workspace := t.TempDir()
	const sentinel = "sentinel-dispatch-marker.txt"
	require.NoError(t, os.WriteFile(filepath.Join(workspace, sentinel), []byte("x\n"), 0o644))

	agentCfg := config.Agent{
		ID:           config.AgentCoder,
		AllowedTools: []string{tools.GlobToolName},
	}

	// Scoped to the workspace: the glob tool finds the sentinel.
	scoped, err := coord.buildTools(t.Context(), agentCfg, true, workspace)
	require.NoError(t, err)
	require.Contains(t, runGlob(t, findTool(t, scoped, tools.GlobToolName), "sentinel-*.txt"), sentinel)

	// Unset working dir: the toolchain roots at the main tree, which has
	// no sentinel — proving the default path is unchanged.
	require.NotEqual(t, workspace, coord.cfg.WorkingDir())
	def, err := coord.buildTools(t.Context(), agentCfg, true, "")
	require.NoError(t, err)
	require.NotContains(t, runGlob(t, findTool(t, def, tools.GlobToolName), "sentinel-*.txt"), sentinel)
}
