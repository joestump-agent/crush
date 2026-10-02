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
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/stretchr/testify/require"
)

// dispatchTestAgent is a fake dispatched agent: it satisfies the
// SessionAgent surface runDispatch and the tool handler use, embedding
// the interface so only the exercised methods are implemented.
type dispatchTestAgent struct {
	SessionAgent
	model  Model
	result *fantasy.AgentResult
	err    error
	calls  []SessionAgentCall
}

func (f *dispatchTestAgent) Run(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	f.calls = append(f.calls, call)
	return f.result, f.err
}

func (f *dispatchTestAgent) Model() Model { return f.model }

func (f *dispatchTestAgent) WaitReady() error { return nil }

func dispatchTestModel() Model {
	return Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1024},
		ModelCfg:   config.SelectedModel{Provider: "test-provider", Model: "test-model"},
	}
}

// initGitRepo makes dir a git repository with one commit, so dispatch can
// provision worktrees from it.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	// -b main pins the branch name: CI runners default to master, the
	// local machine to whatever init.defaultBranch says, and the tests
	// below dispatch against the "main" base.
	git("init", "-q", "-b", "main")
	git("config", "user.email", "dispatch-test@example.com")
	git("config", "user.name", "dispatch test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one"), 0o644))
	git("add", "-A")
	git("commit", "-qm", "initial")
}

// newDispatchToolEnv builds a test coordinator whose working directory is
// a git repository, and swaps the dispatched-agent builder for a fake
// returning agent.
func newDispatchToolEnv(t *testing.T, agent *dispatchTestAgent) (*coordinator, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	initGitRepo(t, env.workingDir)

	c := newDispatchTestCoordinator(t, env)
	c.dispatchAgentBuilder = func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error) {
		return &dispatchedAgent{
			agent:       agent,
			model:       agent.model,
			providerCfg: config.ProviderConfig{ID: "test-provider"},
		}, nil
	}
	return c, env
}

// runDispatchToolCall invokes the DispatchAgent tool with a full tool-call
// context (session, message, width), mirroring what the agent loop
// provides in production.
func runDispatchToolCall(t *testing.T, tool fantasy.AgentTool, params any) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "dispatch-parent-session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "dispatch-parent-message")
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "dispatch-tool-call",
		Name:  DispatchAgentToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

func decodeDispatchHandle(t *testing.T, resp fantasy.ToolResponse) dispatch.DispatchResult {
	t.Helper()
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	var handle dispatch.DispatchResult
	require.NoError(t, json.Unmarshal([]byte(resp.Content), &handle))
	return handle
}

// The tool validates its arguments before touching git: a missing prompt
// and an unknown model type are tool errors, not failed dispatches.
func TestDispatchAgentToolValidatesArgs(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "prompt is required")

	resp = runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work", Model: "gpt-9"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, `invalid model "gpt-9"`)

	// Validation happens before any agent runs.
	require.Empty(t, agent.calls)
}

// The tool needs the session and message IDs from the tool-call context,
// exactly like the agent tool.
func TestDispatchAgentToolRequiresCallContext(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	input, err := json.Marshal(DispatchAgentParams{Prompt: "do work"})
	require.NoError(t, err)
	_, err = tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Name: DispatchAgentToolName, Input: string(input)})
	require.ErrorContains(t, err, "session id missing from context")
}

// A working directory that is not a git repository disables dispatch with
// a clear tool error, cached so every later call reports the same thing
// instead of re-probing.
func TestDispatchAgentToolNotAGitRepo(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not a git repository")

	resp = runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "not a git repository")
}

// Requested skill names that do not exist in the workspace fail the tool
// call before anything is dispatched.
func TestDispatchAgentToolUnknownSkill(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work", Skills: []string{"no-such-skill"}})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "unknown skills: no-such-skill")

	// Nothing was provisioned: no branches, no worktrees directory
	// entries, no registry entries.
	ws, err := c.dispatchWorkspace()
	require.NoError(t, err)
	require.Empty(t, ws.List())
}

// The happy path: the tool provisions a workspace, bootstraps the
// toolchain, registers the ephemeral session, and returns a running
// handle immediately; the background run then flips the registry entry to
// completed and propagates the prompt to the dispatched agent.
func TestDispatchAgentToolReturnsRunningHandleAndRunsInBackground(t *testing.T) {
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
	}
	c, _ := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"})
	handle := decodeDispatchHandle(t, resp)

	require.Equal(t, dispatch.StatusRunning, handle.Status)
	require.NotEmpty(t, handle.DispatchID)
	require.Equal(t, dispatch.BranchPrefix+handle.DispatchID, handle.Branch)
	// Path-form-agnostic: the workspace is the worktrees dir + branch,
	// under the repo root (which the Workspace absolutizes — on Windows
	// the test env's /tmp prefix gains a drive letter).
	require.True(t, strings.HasSuffix(
		filepath.ToSlash(handle.WorkspacePath),
		"/.crush/worktrees/"+handle.Branch,
	), "workspace path %q is not the worktrees dir + branch", handle.WorkspacePath)
	require.NotEmpty(t, handle.SessionID)
	require.DirExists(t, handle.WorkspacePath)

	ws, err := c.dispatchWorkspace()
	require.NoError(t, err)
	entry, ok := ws.Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, handle.SessionID, entry.SessionID)
	require.True(t, strings.HasSuffix(
		filepath.ToSlash(entry.Path),
		"/.crush/worktrees/"+entry.Branch,
	))

	// The background run completes and records the terminal status in
	// the registry.
	require.Eventually(t, func() bool {
		entry, ok := ws.Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	// The dispatched agent ran exactly one turn on the ephemeral session
	// with the dispatch prompt.
	require.Len(t, agent.calls, 1)
	require.Equal(t, handle.SessionID, agent.calls[0].SessionID)
	require.Equal(t, "fix the bug", agent.calls[0].Prompt)
	require.True(t, agent.calls[0].NonInteractive)
	require.Equal(t, 80, agent.calls[0].ContentWidth)
}

// A dispatch whose agent build fails cleans up after itself: no registry
// entry, no branch, no worktree directory.
func TestDispatchAgentToolCleansUpFailedSetup(t *testing.T) {
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)
	c.dispatchAgentBuilder = func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error) {
		return nil, errors.New("boom")
	}
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "build dispatched agent")

	ws, err := c.dispatchWorkspace()
	require.NoError(t, err)
	require.Empty(t, ws.List())

	out, err := exec.CommandContext(t.Context(), "git", "-C", env.workingDir, "branch", "--list", dispatch.BranchPrefix+"*").CombinedOutput()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(out)), "dispatch branch left behind")
	entries, err := os.ReadDir(filepath.Join(env.workingDir, ".crush", "worktrees"))
	require.NoError(t, err)
	require.Empty(t, entries, "worktree directory left behind")
}

// runDispatch maps the run outcome onto the registry status: a completed
// run records completed; an error and a no-turn run both record failed —
// a nil result is "no turn ran", never success (#173's review note).
func TestRunDispatchRecordsTerminalStatus(t *testing.T) {
	cases := []struct {
		name   string
		agent  *dispatchTestAgent
		status dispatch.Status
	}{
		{
			name: "completed",
			agent: &dispatchTestAgent{
				model:  dispatchTestModel(),
				result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
			},
			status: dispatch.StatusCompleted,
		},
		{
			name:   "run error",
			agent:  &dispatchTestAgent{model: dispatchTestModel(), err: context.DeadlineExceeded},
			status: dispatch.StatusFailed,
		},
		{
			name:   "no turn ran",
			agent:  &dispatchTestAgent{model: dispatchTestModel()},
			status: dispatch.StatusFailed,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newDispatchToolEnv(t, tt.agent)
			ws, err := c.dispatchWorkspace()
			require.NoError(t, err)
			entry, err := ws.Provision(t.Context(), dispatch.ProvisionOptions{})
			require.NoError(t, err)

			toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
			require.NoError(t, err)

			c.runDispatch(t.Context(), dispatchRun{
				workspace:       ws,
				entry:           entry,
				toolchain:       toolchain,
				agent:           tt.agent,
				model:           tt.agent.model,
				providerCfg:     config.ProviderConfig{ID: "test-provider"},
				prompt:          "do work",
				sessionID:       "dispatch-child-session",
				parentSessionID: "dispatch-parent-session",
			})

			got, ok := ws.Get(entry.ID)
			require.True(t, ok)
			require.Equal(t, tt.status, got.Status)
		})
	}
}

// The session-end backstop: when the coordinator's context ends, every
// workspace dispatch created is swept away.
func TestSweepDispatchOnCoordinatorEnd(t *testing.T) {
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
	}
	c, _ := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	ctx, cancel := context.WithCancel(t.Context())
	go c.sweepDispatchOnDone(ctx)

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work"})
	handle := decodeDispatchHandle(t, resp)

	ws, err := c.dispatchWorkspace()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		entry, ok := ws.Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	cancel()

	require.Eventually(t, func() bool {
		_, err := os.Stat(handle.WorkspacePath)
		return os.IsNotExist(err)
	}, 10*time.Second, 50*time.Millisecond)
	require.Empty(t, ws.List())
}

// buildDispatchedAgent constructs a real dispatched agent offline: the
// model defaults to the small model (large when asked), the system prompt
// is the dispatch template rendered against the workspace's scoped store
// and rooted at the workspace directory, and the tools are the widened
// dispatch set.
func TestBuildDispatchedAgent(t *testing.T) {
	env := testEnv(t)
	initGitRepo(t, env.workingDir)

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	const (
		largeProviderID = "test-openai-compat"
		smallProviderID = "test-openai-compat-small"
		modelID         = "test-model"
	)
	for _, providerID := range []string{largeProviderID, smallProviderID} {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:      providerID,
			Name:    "Test",
			Type:    openaicompat.Name,
			BaseURL: "http://127.0.0.1:0/v1",
			APIKey:  "test",
			Models:  []catwalk.Model{{ID: modelID, DefaultMaxTokens: 4096}},
		})
	}
	cfg.OverridePreferredModel(config.SelectedModelTypeLarge, config.SelectedModel{Provider: largeProviderID, Model: modelID})
	cfg.OverridePreferredModel(config.SelectedModelTypeSmall, config.SelectedModel{Provider: smallProviderID, Model: modelID})
	cfg.SetupAgents()

	c := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}

	ws, err := c.dispatchWorkspace()
	require.NoError(t, err)
	entry, err := ws.Provision(t.Context(), dispatch.ProvisionOptions{})
	require.NoError(t, err)
	toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
	require.NoError(t, err)
	defer toolchain.Close(t.Context())

	cases := []struct {
		name      string
		modelType config.SelectedModelType
	}{
		{name: "defaults to small model", modelType: ""},
		{name: "large model", modelType: config.SelectedModelTypeLarge},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dispatched, err := c.buildDispatchedAgent(t.Context(), dispatchAgentOptions{
				Toolchain: toolchain,
				ModelType: tt.modelType,
			})
			require.NoError(t, err)
			require.NoError(t, dispatched.agent.WaitReady())

			// The agent runs on its large slot: the chosen model is
			// whatever the dispatch asked for, defaulting to small.
			if tt.modelType == config.SelectedModelTypeLarge {
				require.Equal(t, largeProviderID, dispatched.model.ModelCfg.Provider)
			} else {
				require.Equal(t, smallProviderID, dispatched.model.ModelCfg.Provider)
			}
		})
	}

	dispatched, err := c.buildDispatchedAgent(t.Context(), dispatchAgentOptions{Toolchain: toolchain})
	require.NoError(t, err)

	// The rendered prompt is the dispatch contract rooted at the
	// workspace: it carries the workspace as the working directory and
	// the no-merge rule.
	rendered := dispatched.agent.(*sessionAgent).systemPrompt.Get()
	require.Contains(t, rendered, "dispatched agent")
	require.Contains(t, rendered, filepath.ToSlash(entry.Path))
	require.Contains(t, rendered, "Do NOT merge, rebase, push")

	// The tools are the task agent's read-only set widened with the
	// dispatch write tools.
	toolNames := toolNamesOf(dispatched.agent)
	for _, name := range dispatchWriteTools {
		require.Contains(t, toolNames, name)
	}
	require.Contains(t, toolNames, tools.GlobToolName)
	require.Contains(t, toolNames, tools.ViewToolName)
}

// toolNamesOf returns the names of an agent's current tool set.
func toolNamesOf(a SessionAgent) []string {
	agentTools := a.(*sessionAgent).tools.Copy()
	names := make([]string, 0, len(agentTools))
	for _, tool := range agentTools {
		names = append(names, tool.Info().Name)
	}
	return names
}
