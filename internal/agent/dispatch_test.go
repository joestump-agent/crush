package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
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

	c := &coordinator{
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
	// The default test host (#347): runDispatch has one execution path
	// through the A2A client, so every coordinator test drives its
	// dispatches through a host. A test that needs a different one
	// replaces it with SetDispatchHost.
	c.SetDispatchHost(&runnerTransport{})
	// The session-end cleanup (#63's Sweep), run as a cleanup: a
	// dispatch's workspace outlives its background run — runDispatch
	// never releases it — so the worktree provider's ownership-lease
	// file stays open until something releases it. t.TempDir()'s cleanup
	// fails the test on Windows when it cannot delete an open file, so
	// the release must run before that removal. Registered after testEnv's
	// cleanups and before reapDispatchRuns' registration, it runs after
	// the reaper has waited out every run (LIFO) and before the
	// directory goes away (#422). Teardown wants the full Sweep, not the
	// selective exit release: only a test's assertions decide what
	// survives, and teardown must close every lease, salvageable work
	// included. Sweep is idempotent, so a test that swept or released
	// explicitly is unaffected.
	t.Cleanup(func() {
		c.dispatchMu.Lock()
		provider := c.dispatchProvider
		c.dispatchMu.Unlock()
		if provider == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), dispatchSweepTimeout)
		defer cancel()
		_ = provider.Sweep(ctx)
	})
	return c
}

// reapDispatchRuns installs the spawn seam (#422): every runDispatch the
// coordinator starts is tracked in a WaitGroup, and a t.Cleanup releases
// the given gates, then waits up to 10s for every run to finish. A run
// that is still going then fails the test instead of deleting the
// working directory out from under a concurrent test process. It is
// registered after testEnv's cleanups, so it runs first (LIFO) and the
// directory is still on disk while it waits. The WaitGroup lives in this
// closure, never on the coordinator: Go 1.27 panics on a WaitGroup Add
// racing a Wait (readiness.go, #298).
func reapDispatchRuns(t *testing.T, c *coordinator, gates ...func()) {
	t.Helper()
	var wg sync.WaitGroup
	c.spawnDispatch = func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}
	t.Cleanup(func() {
		for _, release := range gates {
			release()
		}
		finished := make(chan struct{})
		go func() {
			wg.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("timed out waiting for a dispatched run to finish")
		}
	})
}

// runnerTransport is the default test host (#343, #347):
// StartDispatchServer records the DispatchServerParams like the real
// factory and stamps a canned endpoint/card on the registry entry, and
// StreamDispatch drives the recorded runner with the recorded call,
// mapping the outcome the way the executor does (#342) — a canceled run
// to canceled, an error or a nil result to failed, anything else to
// completed with the response text and the diff artifact, with diff
// capture errors dropped. newDispatchTestCoordinator wires one by
// default, so every coordinator test runs its dispatches through the
// one execution path; a test that needs a different host calls
// SetDispatchHost with its own.
type runnerTransport struct {
	mu     sync.Mutex
	served []DispatchServerParams
}

func (f *runnerTransport) StartDispatchServer(ctx context.Context, params DispatchServerParams) (string, any, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, params)
	return "http://127.0.0.1:19999", "fake-card", func() {}, nil
}

// serve records the runner and call the way StartDispatchServer does,
// for tests that drive the run without standing up the server half.
func (f *runnerTransport) serve(params DispatchServerParams) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, params)
}

func (f *runnerTransport) lastServed() DispatchServerParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served[len(f.served)-1]
}

func (f *runnerTransport) StreamDispatch(ctx context.Context, _ DispatchTransportParams) (DispatchTransportOutcome, error) {
	params := f.lastServed()
	result, err := params.Runner.Run(ctx, params.Call)
	switch {
	case errors.Is(err, context.Canceled):
		return DispatchTransportOutcome{Status: transportStatusCanceled}, nil
	case err != nil:
		return DispatchTransportOutcome{Status: transportStatusFailed, Text: err.Error()}, nil
	case result == nil:
		return DispatchTransportOutcome{Status: transportStatusFailed, Text: "agent session did not start a turn (busy or canceled)"}, nil
	}
	outcome := DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   subAgentOutput(result),
	}
	if params.Usage != nil {
		// The executor attaches the dispatched session's final usage to
		// the terminal status (#364); a failed read simply emits none.
		if u, uerr := params.Usage(ctx); uerr == nil {
			outcome.Usage = &u
		}
	}
	if params.Diff != nil {
		// The wire carries the diff verdict itself (#361): the diff, or
		// the capture error when it failed.
		diff, derr := params.Diff(ctx)
		switch {
		case derr != nil:
			outcome.DiffError = derr.Error()
		case diff != "":
			outcome.Diff = diff
		}
	}
	return outcome, nil
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

// dispatchTestUsage mirrors the dispatch tool's usage closure (#364):
// the served task reports the dispatched session's final totals on the
// terminal status.
func dispatchTestUsage(c *coordinator, sessionID string) func(context.Context) (Usage, error) {
	return func(ctx context.Context) (Usage, error) {
		sess, err := c.sessions.Get(ctx, sessionID)
		if err != nil {
			return Usage{}, fmt.Errorf("get dispatch session: %w", err)
		}
		return Usage{PromptTokens: sess.PromptTokens, CompletionTokens: sess.CompletionTokens, Cost: sess.Cost}, nil
	}
}

// serveDispatchRun stands a directly-driven run's A2A server up on the
// coordinator's host, the way the dispatch tool does (#347): runDispatch
// takes only the served transport path, so a test that calls runDispatch
// with a hand-built run must serve its entry first. Call from the test
// goroutine.
func serveDispatchRun(t *testing.T, c *coordinator, run dispatchRun) {
	t.Helper()
	stop, err := c.startDispatchServer(context.Background(), run.provider, run.reg, run.entry.ID, run.sessionID, "tester", "dispatch test", run.agent, nil, run.call(c), run.killSettings.InactivityTimeout, run.kill.current, dispatchTestUsage(c, run.sessionID))
	require.NoError(t, err)
	t.Cleanup(func() {
		if stop != nil {
			stop()
		}
		c.stopDispatchServer(run.reg, run.entry.ID, nil)
	})
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
	// Release the workspace at test end: a provisioned workspace
	// outlives everything the test does with it, and its
	// ownership-lease file stays open until something releases it.
	// t.TempDir()'s cleanup fails the test on Windows when it cannot
	// delete an open file, so the lease must close before that removal
	// (#422). Release is idempotent, so tests that drive Release or
	// Sweep themselves are unaffected.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = provider.Release(ctx, entry)
	})
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

// A dispatched agent's file tools refuse any path that resolves outside
// the workspace (#379): absolute paths, .. escapes, and symlinks inside
// the workspace that point out. Refusal happens in the tool, before any
// permission check, so it holds in yolo mode too (env.permissions skips
// prompts here).
func TestDispatchedFileToolsContained(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "inside.txt"), []byte("inside"), 0o644))

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "parent_owned.go")
	require.NoError(t, os.WriteFile(outsideFile, []byte("package main"), 0o644))

	agentCfg := c.cfg.Config().Agents[config.AgentTask]
	agentCfg.AllowedTools = []string{
		tools.ViewToolName,
		tools.GlobToolName,
		tools.GrepToolName,
		tools.LSToolName,
		tools.DownloadToolName,
	}
	c.cfg.Config().Agents[config.AgentTask] = agentCfg

	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	byName := toolsByName(tc)
	write := byName[tools.WriteToolName]

	resp := runToolAsSession(t, write, tools.WriteToolName, map[string]any{
		"file_path": outsideFile,
		"content":   "hacked",
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "outside the dispatch workspace")
	content, err := os.ReadFile(outsideFile)
	require.NoError(t, err)
	require.Equal(t, "package main", string(content), "outside file was modified")

	resp = runToolAsSession(t, write, tools.WriteToolName, map[string]any{
		"file_path": "../../escape.go",
		"content":   "hacked",
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)

	resp = runToolAsSession(t, write, tools.WriteToolName, map[string]any{
		"file_path": "new.txt",
		"content":   "written inside",
	}, sess.ID)
	require.False(t, resp.IsError, resp.Content)
	require.FileExists(t, filepath.Join(workspace, "new.txt"))

	resp = runToolAsSession(t, byName[tools.EditToolName], tools.EditToolName, map[string]any{
		"file_path":  outsideFile,
		"old_string": "package main",
		"new_string": "package hacked",
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)

	viewResp := runToolAsSession(t, byName[tools.ViewToolName], tools.ViewToolName, map[string]any{
		"file_path": "inside.txt",
	}, sess.ID)
	require.False(t, viewResp.IsError, viewResp.Content)

	resp = runToolAsSession(t, byName[tools.EditToolName], tools.EditToolName, map[string]any{
		"file_path":  "inside.txt",
		"old_string": "inside",
		"new_string": "edited",
	}, sess.ID)
	require.False(t, resp.IsError, resp.Content)

	resp = runToolAsSession(t, byName[tools.MultiEditToolName], tools.MultiEditToolName, map[string]any{
		"file_path": outsideFile,
		"edits": []map[string]string{{
			"old_string": "package main",
			"new_string": "package hacked",
		}},
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)

	// The download tool refuses before it touches the network.
	resp = runToolAsSession(t, byName[tools.DownloadToolName], tools.DownloadToolName, map[string]any{
		"url":       "https://example.com/evil.txt",
		"file_path": "../outside_download.txt",
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)

	resp = runToolAsSession(t, byName[tools.GrepToolName], tools.GrepToolName, map[string]any{
		"pattern": "package",
		"path":    outsideDir,
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "outside the dispatch workspace")

	resp = runToolAsSession(t, byName[tools.GlobToolName], tools.GlobToolName, map[string]any{
		"pattern": "**/*.go",
		"path":    outsideDir,
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)

	resp = runToolAsSession(t, byName[tools.LSToolName], tools.LSToolName, map[string]any{
		"path": outsideDir,
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)
}

// A view through a symlink inside the workspace that points outside is
// refused: containment resolves symlinks, so the escape is caught even
// though the path text stays inside.
func TestDispatchedViewRefusesSymlinkEscape(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	workspace := t.TempDir()
	outsideFile := filepath.Join(t.TempDir(), "parent_owned.go")
	require.NoError(t, os.WriteFile(outsideFile, []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "inside.txt"), []byte("inside"), 0o644))

	if err := os.Symlink(outsideFile, filepath.Join(workspace, "escape-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	agentCfg := c.cfg.Config().Agents[config.AgentTask]
	agentCfg.AllowedTools = []string{tools.ViewToolName}
	c.cfg.Config().Agents[config.AgentTask] = agentCfg

	tc, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: workspace})
	require.NoError(t, err)
	defer tc.Close(t.Context())

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	byName := toolsByName(tc)

	resp := runToolAsSession(t, byName[tools.ViewToolName], tools.ViewToolName, map[string]any{
		"file_path": "escape-link",
	}, sess.ID)
	require.True(t, resp.IsError, resp.Content)

	resp = runToolAsSession(t, byName[tools.ViewToolName], tools.ViewToolName, map[string]any{
		"file_path": "inside.txt",
	}, sess.ID)
	require.False(t, resp.IsError, resp.Content)
	require.Contains(t, resp.Content, "inside")
}

// The main agent's tools are untouched: no containment root rides its
// context, so buildTools' write handles an absolute path outside the
// working directory exactly as before.
func TestMainAgentWriteOutsideWorkingDirUnchanged(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	agentCfg := c.cfg.Config().Agents[config.AgentCoder]
	agentCfg.AllowedTools = []string{tools.WriteToolName}
	built, err := c.buildTools(t.Context(), agentCfg, false)
	require.NoError(t, err)
	require.Len(t, built, 1)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	outsideFile := filepath.Join(t.TempDir(), "absolute.go")
	resp := runToolAsSession(t, built[0], tools.WriteToolName, map[string]any{
		"file_path": outsideFile,
		"content":   "package main",
	}, sess.ID)
	require.False(t, resp.IsError, resp.Content)
	require.FileExists(t, outsideFile)
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
