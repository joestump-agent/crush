package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/scheduler"
	"github.com/stretchr/testify/require"
)

// newDispatchTestCoordinator builds a coordinator with the session-scoped
// services BuildDispatchToolchain shares (sessions, history, filetracker,
// permissions), mirroring how the app wires the real one.
func newDispatchTestCoordinator(t *testing.T, env fakeEnv) *coordinator {
	t.Helper()

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		cronStore:   scheduler.NewStore(""),
	}
}

func runTool(t *testing.T, tool fantasy.AgentTool, name string, params any) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "dispatch-test-session")
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "dispatch-test-call",
		Name:  name,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

// A dispatch toolchain built at a workspace directory must root every
// path-resolving tool there: glob and view resolve relative paths inside
// the workspace (never the parent root), and bash runs with the
// workspace as its working directory. The scoped permission service
// starts with skip off, so the bash call also exercises the bridge to
// the parent service.
func TestBuildDispatchToolchainRootsToolsAtWorkspaceDir(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "a", "b", "one.md"), []byte("dispatched notes"), 0o644))
	// A decoy at the parent workspace root: a tool rooted at the parent
	// would find this instead.
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "decoy.md"), []byte("parent notes"), 0o644))

	agentCfg := c.cfg.Config().Agents[config.AgentTask]
	agentCfg.AllowedTools = []string{tools.GlobToolName, tools.ViewToolName, tools.BashToolName}
	c.cfg.Config().Agents[config.AgentTask] = agentCfg

	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	require.Equal(t, workspace, tc.WorkingDir())
	byName := make(map[string]fantasy.AgentTool, len(tc.Tools()))
	for _, tool := range tc.Tools() {
		byName[tool.Info().Name] = tool
	}
	require.Len(t, byName, 3)
	require.Contains(t, byName, tools.GlobToolName)
	require.Contains(t, byName, tools.ViewToolName)
	require.Contains(t, byName, tools.BashToolName)

	globResp := runTool(t, byName[tools.GlobToolName], tools.GlobToolName, map[string]any{
		"pattern": "**/*.md",
	})
	require.Contains(t, globResp.Content, filepath.Join("a", "b", "one.md"))
	require.NotContains(t, globResp.Content, "decoy.md", "glob escaped the dispatch workspace")

	viewResp := runTool(t, byName[tools.ViewToolName], tools.ViewToolName, map[string]any{
		"file_path": "a/b/one.md",
	})
	require.Contains(t, viewResp.Content, "dispatched notes")

	// Bash inherits the parent's skip-permissions setting through the
	// bridge (env.permissions is skip=true), so the command runs and
	// reports the workspace as its working directory.
	bashResp := runTool(t, byName[tools.BashToolName], tools.BashToolName, map[string]any{
		"command":     "pwd",
		"description": "print the dispatch working directory",
	})
	require.Contains(t, bashResp.Content, workspace)
}

// The default (non-dispatch) toolchain is unchanged: buildTools roots
// its tools at the workspace root, so a relative glob matches under
// env.workingDir.
func TestBuildToolsStillRootedAtWorkspaceRoot(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "root.md"), []byte("x"), 0o644))

	agentCfg := c.cfg.Config().Agents[config.AgentCoder]
	agentCfg.AllowedTools = []string{tools.GlobToolName}
	built, err := c.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.Len(t, built, 1)
	require.Equal(t, tools.GlobToolName, built[0].Info().Name)

	resp := runTool(t, built[0], tools.GlobToolName, map[string]any{
		"pattern": "*.md",
	})
	require.Contains(t, resp.Content, "root.md")
}

func TestBuildDispatchToolchainRequiresWorkingDir(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	_, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{})
	require.ErrorIs(t, err, ErrNoWorkingDir)
}

// Close is safe to call twice and on a nil toolchain.
func TestDispatchToolchainCloseIdempotent(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: t.TempDir()})
	require.NoError(t, err)
	tc.Close(t.Context())
	tc.Close(t.Context())

	var nilTC *DispatchToolchain
	nilTC.Close(t.Context())
}
