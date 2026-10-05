package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
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
		// The todo collector (#65) runs on this context when the first
		// dispatch provisions the registry; t.Context() ends it with the
		// test instead of leaking a Background subscription.
		dispatchCtx: t.Context(),
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
	// The task agent's set is widened with the dispatch write tools
	// (#64): a dispatched agent must be able to produce work, not just
	// read.
	for _, name := range dispatchWriteTools {
		require.Contains(t, byName, name)
	}
	require.Contains(t, byName, tools.GlobToolName)
	require.Contains(t, byName, tools.ViewToolName)
	require.Contains(t, byName, tools.BashToolName)

	globResp := runTool(t, byName[tools.GlobToolName], tools.GlobToolName, map[string]any{
		"pattern": "**/*.md",
	})
	require.Contains(t, globResp.Content, "a/b/one.md")
	require.NotContains(t, globResp.Content, "decoy.md", "glob escaped the dispatch workspace")

	viewResp := runTool(t, byName[tools.ViewToolName], tools.ViewToolName, map[string]any{
		"file_path": "a/b/one.md",
	})
	require.Contains(t, viewResp.Content, "dispatched notes")

	// Bash inherits the parent's skip-permissions setting through the
	// bridge (env.permissions is skip=true), so the command runs with
	// the workspace as its working directory: a relative redirect lands
	// inside the workspace. (A `pwd` content match would not survive
	// Windows path rendering.)
	bashResp := runTool(t, byName[tools.BashToolName], tools.BashToolName, map[string]any{
		"command":     "echo marker > marker.txt",
		"description": "write a marker into the dispatch working directory",
	})
	require.NotContains(t, bashResp.Content, "User denied permission")
	require.FileExists(t, filepath.Join(workspace, "marker.txt"))
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

// setDispatchHook configures a single PreToolUse hook on the parent
// store, the way a user's config would carry it.
func setDispatchHook(t *testing.T, c *coordinator, hook config.HookConfig) {
	t.Helper()
	c.cfg.Config().Hooks = map[string][]config.HookConfig{
		hooks.EventPreToolUse: {hook},
	}
	require.NoError(t, c.cfg.Config().ValidateHooks())
}

// toolsByName indexes a toolchain's tools by name.
func toolsByName(tc *DispatchToolchain) map[string]fantasy.AgentTool {
	byName := make(map[string]fantasy.AgentTool, len(tc.Tools()))
	for _, tool := range tc.Tools() {
		byName[tool.Info().Name] = tool
	}
	return byName
}

// A PreToolUse deny hook configured on the parent blocks a dispatched
// agent's bash call: the hook fires before the inner tool runs, the
// response carries the hook's reason, and the command never executes
// (#377). A deny matters most for dispatch, whose agents hold bash and
// the write tools even when the main agent's policy blocks them.
func TestDispatchToolsPreToolUseDenyBlocksBash(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)
	setDispatchHook(t, c, config.HookConfig{Matcher: "^bash$", Command: `echo "no pushes" >&2; exit 2`})

	workspace := t.TempDir()
	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	bash, ok := toolsByName(tc)[tools.BashToolName].(*hookedTool)
	require.Truef(t, ok, "dispatched bash should be wrapped in a hookedTool")

	resp := runTool(t, bash, tools.BashToolName, map[string]any{
		"command":     "echo marker > marker.txt",
		"description": "write a marker into the dispatch working directory",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Tool call blocked by hook")
	require.Contains(t, resp.Content, "no pushes")
	require.NoFileExists(t, filepath.Join(workspace, "marker.txt"))
}

// A hook's updated_input patch rewrites the dispatched call's input
// before the inner tool runs: bash executes the rewritten command, not
// the one the model sent (#377).
func TestDispatchToolsPreToolUseUpdatedInputRewritesCall(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)
	setDispatchHook(t, c, config.HookConfig{Matcher: "^bash$", Command: `echo '{"updated_input":{"command":"echo rewritten > rewritten.txt"}}'`})

	workspace := t.TempDir()
	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	bash, ok := toolsByName(tc)[tools.BashToolName].(*hookedTool)
	require.Truef(t, ok, "dispatched bash should be wrapped in a hookedTool")

	resp := runTool(t, bash, tools.BashToolName, map[string]any{
		"command":     "echo marker > marker.txt",
		"description": "write a marker into the dispatch working directory",
	})
	require.False(t, resp.IsError, resp.Content)
	require.FileExists(t, filepath.Join(workspace, "rewritten.txt"))
	require.NoFileExists(t, filepath.Join(workspace, "marker.txt"))
}

// A PreToolUse allow hook pre-approves the dispatched call's permission
// request the same way it does for the main agent: the command runs and
// the parent's (prompting) permission service sees no request through
// the bridge (#377).
func TestDispatchToolsPreToolUseAllowSkipsPermission(t *testing.T) {
	env := testEnv(t)
	// The parent prompts (skip off), so a bridged request would surface
	// here; the allow hook must short-circuit before that.
	env.permissions = permission.NewPermissionService(env.workingDir, false, []string{})
	c := newDispatchTestCoordinator(t, env)
	setDispatchHook(t, c, config.HookConfig{Matcher: "^bash$", Command: `echo '{"decision":"allow"}'`})

	workspace := t.TempDir()
	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	bash, ok := toolsByName(tc)[tools.BashToolName].(*hookedTool)
	require.Truef(t, ok, "dispatched bash should be wrapped in a hookedTool")

	events := env.permissions.Subscribe(t.Context())
	resp := runTool(t, bash, tools.BashToolName, map[string]any{
		"command":     "echo marker > marker.txt",
		"description": "write a marker into the dispatch working directory",
	})
	require.NotContains(t, resp.Content, "User denied permission")
	require.FileExists(t, filepath.Join(workspace, "marker.txt"))

	select {
	case ev := <-events:
		t.Fatalf("parent saw a permission request: %+v", ev)
	case <-time.After(250 * time.Millisecond):
	}
}

// With no hooks configured, dispatched tools are returned unwrapped: the
// hook runner is nil and the tool set is the plain filtered list (#377).
func TestDispatchToolsUnwrappedWithoutHooks(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: t.TempDir()})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	require.NotEmpty(t, tc.Tools())
	for _, tool := range tc.Tools() {
		_, isHooked := tool.(*hookedTool)
		require.Falsef(t, isHooked, "tool %s should not be wrapped", tool.Info().Name)
	}
}

// The `agent` and `agentic_fetch` sub-agents stay unhooked even with
// hooks configured: buildTools passes isSubAgent=true and the wrap is a
// no-op for them (#377).
func TestBuildToolsSubAgentUnwrappedWithHooks(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)
	setDispatchHook(t, c, config.HookConfig{Matcher: "^bash$", Command: `exit 0`})

	agentCfg := c.cfg.Config().Agents[config.AgentCoder]
	agentCfg.AllowedTools = []string{tools.GlobToolName, tools.BashToolName}
	built, err := c.buildTools(t.Context(), agentCfg, true)
	require.NoError(t, err)
	require.NotEmpty(t, built)
	for _, tool := range built {
		_, isHooked := tool.(*hookedTool)
		require.Falsef(t, isHooked, "sub-agent tool %s should not be wrapped", tool.Info().Name)
	}
}
