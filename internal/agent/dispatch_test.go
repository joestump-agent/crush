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

	// Bash runs without a prompt because the scoped permission service
	// follows the parent's live skip state (env.permissions is skip=true
	// here and is never toggled), so the command runs with the workspace
	// as its working directory: a relative redirect lands inside the
	// workspace. (A `pwd` content match would not survive Windows path
	// rendering.)
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


// A denied tool stays denied inside a dispatch (#376): the dispatched
// toolset must not add the write tools back over the parent's
// options.disabled_tools / permissions deny. Only a full write-tool deny
// or a todos deny narrows the set; the default set is unchanged.
func TestDispatchToolchainHonorsDisabledTools(t *testing.T) {
	cases := []struct {
		name     string
		disabled []string
		denied   []string
	}{
		{name: "default"},
		{
			name:     "bash denied",
			disabled: []string{tools.BashToolName},
			denied:   []string{tools.BashToolName},
		},
		{
			name:     "todos denied",
			disabled: []string{tools.TodosToolName},
			denied:   []string{tools.TodosToolName},
		},
		{
			name:     "all write tools denied",
			disabled: []string{tools.BashToolName, tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName},
			denied:   []string{tools.BashToolName, tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			c := newDispatchTestCoordinator(t, env)
			c.cfg.Config().Options.DisabledTools = tc.disabled
			c.cfg.Config().SetupAgents()

			workspace := t.TempDir()
			toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
			require.NoError(t, err)
			defer toolchain.Close(t.Context())

			got := make(map[string]bool, len(toolchain.Tools()))
			for _, tool := range toolchain.Tools() {
				got[tool.Info().Name] = true
			}
			for _, name := range tc.denied {
				require.NotContains(t, got, name, "denied tool %q leaked into the dispatch", name)
			}
			if tc.name == "default" {
				for _, name := range dispatchWriteTools {
					require.Contains(t, got, name, "default set lost %q", name)
				}
			}
			// A deny list narrows write tools; read tools stay.
			require.Contains(t, got, tools.GlobToolName)
			require.Contains(t, got, tools.ViewToolName)
		})
	}
}

// A dispatched agent must follow the parent service's live yolo state,
// not the startup snapshot it was built under (#378): built while the
// parent skips requests, a runtime toggle to prompting mode must make
// the next dispatched request surface on the parent and wait for it.
func TestDispatchPermissionFollowsParentSkipOffToggle(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	workspace := t.TempDir()
	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	parentEvents := env.permissions.Subscribe(t.Context())
	env.permissions.SetSkipRequests(false)

	type outcome struct {
		granted bool
		err     error
	}
	resCh := make(chan outcome, 1)
	go func() {
		granted, err := tc.permissions.Request(t.Context(), permission.CreatePermissionRequest{
			SessionID:  "dispatch-test-session",
			ToolCallID: "call-378",
			ToolName:   "bash",
			Action:     "execute",
			Path:       filepath.Join(workspace, "outside.txt"),
		})
		resCh <- outcome{granted, err}
	}()

	select {
	case ev := <-parentEvents:
		require.Equal(t, "call-378", ev.Payload.ToolCallID)
		require.True(t, env.permissions.Grant(ev.Payload), "parent grant should resolve the request")
	case res := <-resCh:
		t.Fatalf("dispatched request resolved without the parent: granted=%v err=%v", res.granted, res.err)
	}

	select {
	case res := <-resCh:
		require.NoError(t, res.err)
		require.True(t, res.granted, "parent grant should reach the dispatched waiter")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the dispatched request to resolve")
	}
}

// The other direction (#378): built in prompting mode, a runtime toggle
// to skip must auto-approve the next dispatched request, and the parent
// sees no request event.
func TestDispatchPermissionFollowsParentSkipOnToggle(t *testing.T) {
	env := testEnv(t)
	env.permissions = permission.NewPermissionService(env.workingDir, false, nil)
	c := newDispatchTestCoordinator(t, env)

	workspace := t.TempDir()
	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	parentEvents := env.permissions.Subscribe(t.Context())
	env.permissions.SetSkipRequests(true)

	granted, err := tc.permissions.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID:  "dispatch-test-session",
		ToolCallID: "call-379",
		ToolName:   "bash",
		Action:     "execute",
		Path:       filepath.Join(workspace, "outside.txt"),
	})
	require.NoError(t, err)
	require.True(t, granted, "toggled-on parent should auto-approve dispatched requests")

	time.Sleep(250 * time.Millisecond)
	select {
	case ev := <-parentEvents:
		t.Fatalf("parent saw an unexpected request event: %v", ev.Payload)
	default:

	}
}
