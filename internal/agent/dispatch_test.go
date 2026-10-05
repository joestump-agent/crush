package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/scheduler"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// newDispatchTestCoordinator builds a coordinator with the session-scoped
// services BuildDispatchToolchain shares (sessions, history, filetracker,
// permissions), mirroring how the app wires the real one.
func newDispatchTestCoordinator(t *testing.T, env fakeEnv) *coordinator {
	t.Helper()
	return newDispatchTestCoordinatorAt(t, env, env.workingDir, "")
}

// newDispatchTestCoordinatorAt is newDispatchTestCoordinator with the
// coordinator's working directory and data directory overridden, for
// tests that start dispatch away from the repo root (#383). An empty
// dataDir keeps config's own resolution.
func newDispatchTestCoordinatorAt(t *testing.T, env fakeEnv, workingDir, dataDir string) *coordinator {
	t.Helper()

	cfg, err := config.Init(workingDir, dataDir, false)
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

// provisionDispatchEntry provisions a workspace through the
// coordinator's git worktree provider and registers its entry, the same
// two steps the dispatch tool performs. It returns the registered entry
// and the provider, for tests that drive Sweep or Release directly.
func provisionDispatchEntry(t *testing.T, c *coordinator, base string) (dispatch.Entry, *dispatch.GitWorktreeProvider) {
	t.Helper()
	provider, err := c.dispatchWorkspaceProvider()
	require.NoError(t, err)
	entry := provisionProviderEntry(t, provider, c.dispatchRegistry(), dispatch.ProvisionOptions{Base: base})
	return entry, provider
}

// provisionProviderEntry provisions a workspace on provider and
// registers the entry in reg, for tests that drive a provider they
// built themselves.
func provisionProviderEntry(t *testing.T, provider *dispatch.GitWorktreeProvider, reg *dispatch.AgentRegistry, opts dispatch.ProvisionOptions) dispatch.Entry {
	t.Helper()
	id := uuid.NewString()
	placement, err := provider.Provision(t.Context(), id, opts)
	require.NoError(t, err)
	entry := dispatch.Entry{
		ID:      id,
		Path:    placement.Path,
		Branch:  placement.Branch,
		Base:    placement.Base,
		BaseSHA: placement.BaseSHA,
		Status:  dispatch.StatusProvisioned,
	}
	reg.Register(entry)
	return entry
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
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the dispatched request to surface on the parent")
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

// With the default config a dispatched agent gets exactly the task
// agent's read-only set widened with the dispatch write tools (#64) and
// the dispatch support tools (#384): job_output and job_kill read back
// and stop the background shells bash auto-starts, and lsp_diagnostics
// checks the LSP's view of an edit (constructed only while the LSP tools
// are registered, which the default config satisfies). The set is
// compared as a sorted list so future drift in the union shows up here.
func TestBuildDispatchToolchainDefaultToolSet(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: t.TempDir()})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	names := make([]string, 0, len(tc.Tools()))
	for _, tool := range tc.Tools() {
		names = append(names, tool.Info().Name)
	}
	slices.Sort(names)
	require.Equal(t, []string{
		tools.BashToolName,
		tools.EditToolName,
		tools.GlobToolName,
		tools.GrepToolName,
		tools.JobKillToolName,
		tools.JobOutputToolName,
		tools.LSToolName,
		tools.CallHierarchyToolName,
		tools.DefinitionToolName,
		tools.DiagnosticsToolName,
		tools.SymbolsToolName,
		tools.MultiEditToolName,
		tools.TodosToolName,
		tools.ViewToolName,
		tools.WriteToolName,
	}, names)
}

// A dispatched bash command started with run_in_background lands in the
// in-process background-shell buffer, and the dispatched job_output reads
// it back by shell ID (#384). Before the fix job_output was not in the
// dispatched tool set, so the result of any auto-backgrounded command
// was unreachable from the dispatched agent.
func TestBuildDispatchToolchainBashBackgroundJobReadBack(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: t.TempDir()})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	byName := make(map[string]fantasy.AgentTool, len(tc.Tools()))
	for _, tool := range tc.Tools() {
		byName[tool.Info().Name] = tool
	}

	bashResp := runTool(t, byName[tools.BashToolName], tools.BashToolName, map[string]any{
		// Over a second: shorter commands finish inside the bash tool's
		// fast-failure check and come back inline, not as a background
		// job.
		"command":           "sleep 2 && echo dispatched-background-marker",
		"description":       "run a short command in the background",
		"run_in_background": true,
	})
	shellID := extractBackgroundShellID(t, bashResp.Content)

	outputResp := runTool(t, byName[tools.JobOutputToolName], tools.JobOutputToolName, map[string]any{
		"shell_id": shellID,
		"wait":     true,
	})
	require.Contains(t, outputResp.Content, "Status: completed")
	require.Contains(t, outputResp.Content, "dispatched-background-marker")
}

// extractBackgroundShellID pulls the shell ID out of the bash tool's
// background response, which renders it as "Background shell started
// with ID: <id>" (explicit) or "Background shell ID: <id>" (auto).
func extractBackgroundShellID(t *testing.T, content string) string {
	t.Helper()

	matches := regexp.MustCompile(`Background shell (?:started with )?ID: (\S+)`).FindStringSubmatch(content)
	require.NotEmpty(t, matches, "no background shell ID in %q", content)
	return matches[1]
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
