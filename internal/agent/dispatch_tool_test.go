package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/scheduler"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/charmbracelet/crush/internal/skills"
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
	// mu guards calls: the concurrency-cap tests run several dispatches
	// against the same fake, whose turns then append from separate
	// goroutines.
	mu sync.Mutex
	// gate, when non-nil, blocks each turn until it is closed — the
	// seam the concurrency-cap tests use to hold a dispatch running.
	gate chan struct{}
	// onRun, when set, runs before each turn returns — the seam the
	// background-job tests use to start a job tagged with the dispatch
	// session from inside the run (#385).
	onRun func(call SessionAgentCall)
	calls []SessionAgentCall
}

func (f *dispatchTestAgent) Run(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if f.gate != nil {
		<-f.gate
	}
	if f.onRun != nil {
		f.onRun(call)
	}
	return f.result, f.err
}

func (f *dispatchTestAgent) lenCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *dispatchTestAgent) Model() Model { return f.model }

func (f *dispatchTestAgent) WaitReady() error { return nil }

// IsSessionBusy always reports idle: the delivery flush (#388) consults
// it before touching pending results, and these fakes never park a
// session mid-turn.
func (f *dispatchTestAgent) IsSessionBusy(string) bool { return false }

func dispatchTestModel() Model {
	return Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1024},
		ModelCfg:   config.SelectedModel{Provider: "test-provider", Model: "test-model"},
	}
}

func intPtr(v int) *int { return &v }

// heldDispatchSlots reports the dispatch concurrency slots currently
// held, so tests can wait for a finished dispatch to actually give its
// slot back instead of only seeing its registry status flip.
func (c *coordinator) heldDispatchSlots() int {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	return c.dispatchSlots
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
	reapDispatchRuns(t, c)
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

// A non-interactive coordinator (`crush run`) must not offer
// dispatch_agent or message_agent: the process exits when the parent's
// turn ends, so a dispatch started there would die with its result
// undelivered (#387). The interactive coordinator keeps both with the
// default config, and sub-agents keep the existing exclusion.
func TestBuildToolsGatesDispatchOnInteractive(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		subAgent    bool
		want        map[string]bool
	}{
		{
			name:        "interactive main agent keeps both",
			interactive: true,
			want:        map[string]bool{DispatchAgentToolName: true, MessageAgentToolName: true, CancelDispatchToolName: true},
		},
		{
			name:        "non-interactive main agent drops both",
			interactive: false,
			want:        map[string]bool{DispatchAgentToolName: false, MessageAgentToolName: false, CancelDispatchToolName: false},
		},
		{
			name:        "interactive sub-agent still drops both",
			interactive: true,
			subAgent:    true,
			want:        map[string]bool{DispatchAgentToolName: false, MessageAgentToolName: false, CancelDispatchToolName: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newDispatchTestCoordinator(t, testEnv(t))
			c.interactive = tt.interactive

			const providerID = "test-provider"
			c.cfg.Config().Providers.Set(providerID, config.ProviderConfig{
				ID:      providerID,
				Name:    "Test",
				Type:    openaicompat.Name,
				BaseURL: "http://127.0.0.1:0/v1",
				APIKey:  "test",
				Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
			})
			selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
			c.cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
			c.cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)

			agentCfg := c.cfg.Config().Agents[config.AgentCoder]
			built, err := c.buildTools(t.Context(), agentCfg, tt.subAgent)
			require.NoError(t, err)

			names := make(map[string]bool, len(built))
			for _, tool := range built {
				names[tool.Info().Name] = true
			}
			for toolName, present := range tt.want {
				require.Equal(t, present, names[toolName], "tool %q", toolName)
			}
		})
	}
}

// mustWorktreesDir returns the worktrees directory the coordinator
// provisions into — the repo-keyed path under the data directory (#383).
func mustWorktreesDir(t *testing.T, c *coordinator) string {
	t.Helper()
	dir, err := dispatch.WorktreesDir(c.cfg.Config().Options.DataDirectory, c.cfg.WorkingDir())
	require.NoError(t, err)
	return dir
}

// Dispatch worktrees live under the data directory, never beside the
// working directory (#383): a coordinator started in a repo
// subdirectory provisions into <dataDir>/worktrees/<repo-key>/ without
// creating <cwd>/.crush, and the parent repo's git view stays clean —
// nothing for git status to list, no gitlink for git add -A to stage.
// Both data-directory placements are exercised: the default lookup
// finding <repo>/.crush, and a custom data directory outside the repo.
func TestDispatchWorktreesLiveUnderDataDirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dataDir func(*testing.T) string
	}{
		{"data directory inside the repo", func(*testing.T) string { return "" }},
		{"custom data directory outside the repo", func(t *testing.T) string { return t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &dispatchTestAgent{
				model:  dispatchTestModel(),
				result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
			}
			env := testEnv(t)
			repo := env.workingDir
			initGitRepo(t, repo)
			// A .crush at the repo root, as after any launch from the
			// root: root.go gitignores it with a "*".
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".crush"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".crush", ".gitignore"), []byte("*\n"), 0o644))
			sub := filepath.Join(repo, "sub")
			require.NoError(t, os.MkdirAll(sub, 0o755))
			env.workingDir = sub

			c := newDispatchTestCoordinatorAt(t, env, sub, tc.dataDir(t))
			c.dispatchAgentBuilder = func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error) {
				return &dispatchedAgent{
					agent:       agent,
					model:       agent.model,
					providerCfg: config.ProviderConfig{ID: "test-provider"},
				}, nil
			}
			tool := c.dispatchTool()

			resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work", Branch: "main"})
			handle := decodeDispatchHandle(t, resp)

			// Release the workspace lease before TempDir cleanup runs:
			// the dispatch holds the lock file, and Windows refuses to
			// remove a file another process has open. Cleanups run LIFO,
			// so this lands ahead of testEnv's RemoveAll.
			provider, werr := c.dispatchWorkspaceProvider()
			require.NoError(t, werr)
			t.Cleanup(func() { _ = provider.Sweep(context.Background()) })

			// The workspace is <dataDir>/worktrees/<repo-key>/<branch>,
			// under the resolved data directory — never beside the cwd.
			wtDir := mustWorktreesDir(t, c)
			require.True(t, strings.HasPrefix(
				filepath.ToSlash(handle.WorkspacePath),
				filepath.ToSlash(wtDir)+"/",
			), "workspace %q is not under %q", handle.WorkspacePath, wtDir)
			require.NoDirExists(t, filepath.Join(sub, ".crush"))

			// The parent repo's git view is untouched by the dispatch.
			out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "status", "--porcelain").CombinedOutput()
			require.NoError(t, err)
			require.Empty(t, strings.TrimSpace(string(out)), "git status is not clean: %s", out)

			out, err = exec.CommandContext(t.Context(), "git", "-C", repo, "add", "-A", "--dry-run").CombinedOutput()
			require.NoError(t, err)
			require.NotContains(t, string(out), "crush-dispatch-", "git add -A stages dispatch worktree paths: %s", out)
		})
	}
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
	require.Empty(t, c.dispatchRegistry().List())
}

// A model-supplied branch that git would read as an option is a tool
// error: no running handle, no provisioned workspace.
func TestDispatchAgentToolRejectsOptionLikeBranch(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work", Branch: "--lock"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "provision dispatch workspace")

	// The rejected branch never became a dispatch.
	require.Empty(t, c.dispatchRegistry().List())
	require.Empty(t, agent.calls)
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
	// Path-form-agnostic: the workspace is the worktrees dir + branch
	// under the data directory (which the Workspace absolutizes — on
	// Windows the test env's /tmp prefix gains a drive letter).
	wtDir, werr := dispatch.WorktreesDir(c.cfg.Config().Options.DataDirectory, c.cfg.WorkingDir())
	require.NoError(t, werr)
	require.True(t, strings.HasSuffix(
		filepath.ToSlash(handle.WorkspacePath),
		filepath.ToSlash(wtDir)+"/"+handle.Branch,
	), "workspace path %q is not the worktrees dir + branch", handle.WorkspacePath)
	require.NotEmpty(t, handle.SessionID)
	require.DirExists(t, handle.WorkspacePath)

	entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, handle.SessionID, entry.SessionID)
	require.True(t, strings.HasSuffix(
		filepath.ToSlash(entry.Path),
		filepath.ToSlash(wtDir)+"/"+entry.Branch,
	))

	// The background run completes and records the terminal status in
	// the registry.
	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
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

	require.Empty(t, c.dispatchRegistry().List())

	out, err := exec.CommandContext(t.Context(), "git", "-C", env.workingDir, "branch", "--list", dispatch.BranchPrefix+"*").CombinedOutput()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(out)), "dispatch branch left behind")
	entries, err := os.ReadDir(mustWorktreesDir(t, c))
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, e.IsDir(), "worktree directory left behind: %s", e.Name())
	}
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
			entry, provider := provisionDispatchEntry(t, c, "")

			toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
			require.NoError(t, err)

			run := dispatchRun{
				reg:             c.dispatchRegistry(),
				provider:        provider,
				entry:           entry,
				toolchain:       toolchain,
				agent:           tt.agent,
				model:           tt.agent.model,
				providerCfg:     config.ProviderConfig{ID: "test-provider"},
				prompt:          "do work",
				sessionID:       "dispatch-child-session",
				parentSessionID: "dispatch-parent-session",
			}
			serveDispatchRun(t, c, run)
			c.runDispatch(t.Context(), run)

			got, ok := c.dispatchRegistry().Get(entry.ID)
			require.True(t, ok)
			require.Equal(t, tt.status, got.Status)
		})
	}
}

// branchExists reports whether the repo at dir currently has the branch.
func branchExists(t *testing.T, dir, branch string) bool {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "branch", "--list", branch).CombinedOutput()
	require.NoError(t, err, "git branch --list %s: %s", branch, out)
	return strings.TrimSpace(string(out)) != ""
}

// A dispatched run's background jobs die with the run (#385): the fake
// agent starts a job tagged with the dispatch session, and runDispatch
// kills it before the terminal result is assembled, so the salvage diff
// cannot race a job still writing the workspace.
func TestRunDispatchKillsBackgroundJobsOnCompletion(t *testing.T) {
	var env fakeEnv
	jobID := ""
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
		onRun: func(call SessionAgentCall) {
			bgShell, err := shell.GetBackgroundShellManager().Start(context.Background(), call.SessionID, env.workingDir, nil, "sleep 30", "dispatch job")
			require.NoError(t, err)
			jobID = bgShell.ID
		},
	}
	c, env := newDispatchToolEnv(t, agent)
	entry, provider := provisionDispatchEntry(t, c, "")
	toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
	require.NoError(t, err)

	run := dispatchRun{
		reg:             c.dispatchRegistry(),
		provider:        provider,
		entry:           entry,
		toolchain:       toolchain,
		agent:           agent,
		model:           agent.model,
		providerCfg:     config.ProviderConfig{ID: "test-provider"},
		prompt:          "do work",
		sessionID:       "dispatch-child-session",
		parentSessionID: "dispatch-parent-session",
	}
	serveDispatchRun(t, c, run)
	c.runDispatch(t.Context(), run)

	got, ok := c.dispatchRegistry().Get(entry.ID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusCompleted, got.Status)

	// The job started during the run is gone after it.
	require.NotEmpty(t, jobID)
	_, ok = shell.GetBackgroundShellManager().Get(jobID)
	require.False(t, ok, "background job %s survived the run", jobID)
}

// The kill path kills the run's background jobs the same way: the run is
// killed, its Run returns context.Canceled, and the job started under the
// dispatch session is gone when runDispatch returns (#385).
func TestRunDispatchKillsBackgroundJobsOnKill(t *testing.T) {
	var env fakeEnv
	jobID := ""
	kill := &dispatchKill{}
	agent := &dispatchTestAgent{
		model: dispatchTestModel(),
		err:   context.Canceled,
		onRun: func(call SessionAgentCall) {
			kill.kill("test kill")
			bgShell, err := shell.GetBackgroundShellManager().Start(context.Background(), call.SessionID, env.workingDir, nil, "sleep 30", "dispatch job")
			require.NoError(t, err)
			jobID = bgShell.ID
		},
	}
	c, env := newDispatchToolEnv(t, agent)
	entry, provider := provisionDispatchEntry(t, c, "")
	toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
	require.NoError(t, err)

	run := dispatchRun{
		reg:             c.dispatchRegistry(),
		provider:        provider,
		entry:           entry,
		toolchain:       toolchain,
		agent:           agent,
		model:           agent.model,
		providerCfg:     config.ProviderConfig{ID: "test-provider"},
		prompt:          "do work",
		sessionID:       "dispatch-child-session",
		parentSessionID: "dispatch-parent-session",
		kill:            kill,
	}
	serveDispatchRun(t, c, run)
	c.runDispatch(t.Context(), run)

	got, ok := c.dispatchRegistry().Get(entry.ID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusKilled, got.Status)

	// The job started during the killed run is gone after it.
	require.NotEmpty(t, jobID)
	_, ok = shell.GetBackgroundShellManager().Get(jobID)
	require.False(t, ok, "background job %s survived the killed run", jobID)
}

// ReleaseDispatches is the synchronous session-end cleanup (#367): a
// completed dispatch whose workspace holds work the human might want —
// even just uncommitted changes — survives it, and a pending workspace
// that never produced work is removed.
func TestReleaseDispatchesKeepsWork(t *testing.T) {
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
	}
	c, env := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	// Each dispatch keys its task session off the tool-call and message
	// IDs, so both must be unique per dispatch.
	dispatchOnce := func(messageID string) dispatch.DispatchResult {
		t.Helper()
		input, err := json.Marshal(DispatchAgentParams{Prompt: "do work"})
		require.NoError(t, err)
		ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "dispatch-parent-session")
		ctx = context.WithValue(ctx, tools.MessageIDContextKey, messageID)
		ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
		resp, err := tool.Run(ctx, fantasy.ToolCall{
			ID:    "dispatch-tool-call-" + messageID,
			Name:  DispatchAgentToolName,
			Input: string(input),
		})
		require.NoError(t, err)
		return decodeDispatchHandle(t, resp)
	}
	withWork := dispatchOnce("release-with-work")
	empty := dispatchOnce("release-empty")

	for _, handle := range []dispatch.DispatchResult{withWork, empty} {
		require.Eventually(t, func() bool {
			entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
			return ok && entry.Status == dispatch.StatusCompleted
		}, 10*time.Second, 50*time.Millisecond)
	}

	// Uncommitted changes count as work (#367): no commit needed.
	require.NoError(t, os.WriteFile(filepath.Join(withWork.WorkspacePath, "salvage.txt"), []byte("keep"), 0o644))

	c.ReleaseDispatches(context.Background())

	_, err := os.Stat(withWork.WorkspacePath)
	require.NoError(t, err, "completed workspace with work must survive release")
	require.True(t, branchExists(t, env.workingDir, withWork.Branch), "branch of kept workspace must survive")
	entry, ok := c.dispatchRegistry().Get(withWork.DispatchID)
	require.True(t, ok, "kept workspace stays registered so it can still be removed")
	require.Equal(t, dispatch.StatusCompleted, entry.Status)

	_, err = os.Stat(empty.WorkspacePath)
	require.True(t, os.IsNotExist(err), "workless workspace must be released")
	require.False(t, branchExists(t, env.workingDir, empty.Branch), "branch of released workspace must be gone")
	require.Len(t, c.dispatchRegistry().List(), 1, "only the kept workspace stays registered")
}

// The session-end backstop: when the coordinator's context ends, a
// workless workspace is released (#367) — one with work stays on disk.
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

	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	cancel()

	require.Eventually(t, func() bool {
		_, err := os.Stat(handle.WorkspacePath)
		return os.IsNotExist(err)
	}, 10*time.Second, 50*time.Millisecond)
	// Sweep unregisters an entry only after its removal succeeds, so the
	// registry can drain a few git invocations behind the directory.
	require.Eventually(t, func() bool {
		return len(c.dispatchRegistry().List()) == 0
	}, 10*time.Second, 50*time.Millisecond)
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

	// The dispatched agent sees the context files committed at its
	// base revision (#386): commit a marker AGENTS.md before
	// provisioning so the worktree carries it.
	const projectMarker = "dispatch-project-context-marker-386"
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "AGENTS.md"), []byte(projectMarker), 0o644))
	git := func(args ...string) {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", env.workingDir}, args...)...).CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	git("add", "AGENTS.md")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "add AGENTS.md")

	c := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}

	entry, _ := provisionDispatchEntry(t, c, "")
	toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
	require.NoError(t, err)
	defer toolchain.Close(t.Context())

	// The scoped store's user context is what the dispatched agent
	// renders: point it at a temp file with a second marker (#386).
	const globalMarker = "dispatch-global-context-marker-386"
	globalCtxPath := filepath.Join(t.TempDir(), "CRUSH.md")
	require.NoError(t, os.WriteFile(globalCtxPath, []byte(globalMarker), 0o644))
	toolchain.Config().Config().Options.GlobalContextPaths = []string{globalCtxPath}

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

	// The worktree's AGENTS.md is rendered under the project context
	// section, and the user's global context file under the user
	// context section (#386).
	require.Contains(t, rendered, "# Project-Specific Context")
	require.Contains(t, rendered, projectMarker)
	require.Contains(t, rendered, "# User context")
	require.Contains(t, rendered, globalMarker)

	// With no context files, the prompt renders as it did before
	// (#386): no context sections at all.
	savedContextPaths := toolchain.Config().Config().Options.ContextPaths
	savedGlobalContextPaths := toolchain.Config().Config().Options.GlobalContextPaths
	toolchain.Config().Config().Options.ContextPaths = nil
	toolchain.Config().Config().Options.GlobalContextPaths = nil
	bare, err := c.buildDispatchedAgent(t.Context(), dispatchAgentOptions{Toolchain: toolchain})
	require.NoError(t, err)
	bareRendered := bare.agent.(*sessionAgent).systemPrompt.Get()
	require.NotContains(t, bareRendered, "# Project-Specific Context")
	require.NotContains(t, bareRendered, "# User context")
	toolchain.Config().Config().Options.ContextPaths = savedContextPaths
	toolchain.Config().Config().Options.GlobalContextPaths = savedGlobalContextPaths

	// The tools are the worker definition's default dispatch set
	// (#432): the read and write groups plus the support tools.
	toolNames := toolNamesOf(dispatched.agent)
	for _, name := range []string{
		tools.BashToolName, tools.EditToolName, tools.MultiEditToolName,
		tools.WriteToolName, tools.TodosToolName,
		tools.JobOutputToolName, tools.JobKillToolName, tools.DiagnosticsToolName,
		tools.GlobToolName, tools.ViewToolName,
	} {
		require.Contains(t, toolNames, name)
	}
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

// fakeMainAgent stands in for the coordinator's main agent when a test
// drives the full delivery path: it records the turns it is given and
// answers with a fixed result. The runs are mutex-guarded because the
// delivery turn runs on its own goroutine.
type fakeMainAgent struct {
	SessionAgent
	model Model
	mu    sync.Mutex
	runs  []SessionAgentCall
	// busy reports the session busy so a delivery flush parks in the
	// pending set instead of starting a turn.
	busy atomic.Bool
	// failures makes the next N Run calls fail, driving the retry path.
	failures atomic.Int64
	// queue makes Run answer as the busy branch does: the call is
	// recorded as if enqueued, and Run returns nil, nil.
	queue   atomic.Bool
	cancels atomic.Int64
	clears  atomic.Int64
}

func (f *fakeMainAgent) Run(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	f.runs = append(f.runs, call)
	f.mu.Unlock()
	if f.queue.Load() {
		return nil, nil
	}
	if f.failures.Add(-1) >= 0 {
		return nil, errors.New("delivery boom")
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "reviewed"}}},
	}, nil
}

func (f *fakeMainAgent) IsSessionBusy(sessionID string) bool { return f.busy.Load() }

func (f *fakeMainAgent) Cancel(sessionID string) { f.cancels.Add(1) }

func (f *fakeMainAgent) ClearQueue(sessionID string) { f.clears.Add(1) }

// CancelAll has no runs to cancel: Run returns synchronously.
func (f *fakeMainAgent) CancelAll() {}

func (f *fakeMainAgent) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeMainAgent) lastRun() SessionAgentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[len(f.runs)-1]
}

func (f *fakeMainAgent) runsSnapshot() []SessionAgentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionAgentCall(nil), f.runs...)
}

func (f *fakeMainAgent) Model() Model                   { return f.model }
func (f *fakeMainAgent) WaitReady() error               { return nil }
func (f *fakeMainAgent) SetModels(_, _ Model)           {}
func (f *fakeMainAgent) SetTools(_ []fantasy.AgentTool) {}

// assembleDispatchResult maps a finished run onto the terminal
// DispatchResult (#66): completed runs carry the agent's final text as
// key findings and the workspace diff as the summary, failed runs carry
// the error, and a no-turn run fails with the reason.
func TestAssembleDispatchResult(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)
	entry, provider := provisionDispatchEntry(t, c, "")

	// Uncommitted work in the workspace: the work product the diff
	// summary must surface.
	require.NoError(t, os.WriteFile(filepath.Join(entry.Path, "new.txt"), []byte("work\n"), 0o644))

	run := dispatchRun{
		reg:             c.dispatchRegistry(),
		provider:        provider,
		entry:           entry,
		sessionID:       "dispatch-child-session",
		parentSessionID: "dispatch-parent-session",
	}

	completed := c.assembleDispatchResult(t.Context(), run, dispatchNaturalOutcome{
		completed: true,
		findings:  "fixed the bug",
		diff: func(ctx context.Context) (string, error) {
			return run.provider.Diff(ctx, run.entry)
		},
	})
	require.Equal(t, dispatch.StatusCompleted, completed.Status)
	require.Equal(t, "fixed the bug", completed.KeyFindings)
	require.Equal(t, entry.ID, completed.DispatchID)
	require.Equal(t, entry.Branch, completed.Branch)
	require.Equal(t, entry.Path, completed.WorkspacePath)
	require.Equal(t, "dispatch-child-session", completed.SessionID)
	require.Empty(t, completed.Error)
	require.Contains(t, completed.DiffSummary, "new.txt | +1 -0")

	failed := c.assembleDispatchResult(t.Context(), run, dispatchNaturalOutcome{
		runErr: errors.New("provider exploded"),
	})
	require.Equal(t, dispatch.StatusFailed, failed.Status)
	require.Equal(t, "provider exploded", failed.Error)
	require.Empty(t, failed.KeyFindings)
	require.Empty(t, failed.DiffSummary)

	noTurn := c.assembleDispatchResult(t.Context(), run, dispatchNaturalOutcome{})
	require.Equal(t, dispatch.StatusFailed, noTurn.Status)
	require.Contains(t, noTurn.Error, "did not start a turn")
}

// newDeliveryEnv builds the coordinator, fake main agent, and parent
// session the delivery tests share: config with a provider so c.run's
// model resolution succeeds, and a coordinator whose main agent is the
// recording fake.
func newDeliveryEnv(t *testing.T) (*coordinator, *fakeMainAgent, string) {
	env := testEnv(t)
	initGitRepo(t, env.workingDir)

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	const providerID = "test-openai-compat"
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)
	cfg.SetupAgents()
	coderCfg := cfg.Config().Agents[config.AgentCoder]
	coderCfg.AllowedTools = nil // keep the delivery run's tool build cheap
	cfg.Config().Agents[config.AgentCoder] = coderCfg

	mainModel := Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 4096},
		ModelCfg:   config.SelectedModel{Provider: providerID, Model: "test-model"},
	}
	main := &fakeMainAgent{model: mainModel}
	c := &coordinator{
		cfg:           cfg,
		sessions:      env.sessions,
		messages:      env.messages,
		permissions:   env.permissions,
		history:       env.history,
		filetracker:   *env.filetracker,
		cronStore:     scheduler.NewStore(""),
		skillTracker:  skills.NewTracker(nil),
		agents:        map[string]SessionAgent{config.AgentCoder: main},
		mainAgent:     main,
		mainAgentName: config.AgentCoder,
	}

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	return c, main, parent.ID
}

// The terminal payload is delivered to the parent session as a hidden
// follow-up turn on the main agent (#66): the main agent receives the
// DispatchResult JSON as its prompt, marked hidden so it does not render
// as a user message. The delivery's RunID is stripped, so the terminal
// RunComplete it may carry is never mistaken for the dispatch tool
// call's own (#388).
func TestDeliverDispatchResultToParentSession(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)

	terminal := dispatch.DispatchResult{
		DispatchID:  "d-deliver",
		Branch:      "crush-dispatch-d-deliver",
		SessionID:   "s-deliver",
		Status:      dispatch.StatusCompleted,
		KeyFindings: "fixed the bug",
		DiffSummary: "a.go | +2 -1",
	}
	c.deliverDispatchResult(t.Context(), parentID, terminal)

	require.Eventually(t, func() bool {
		return main.runCount() == 1
	}, 10*time.Second, 50*time.Millisecond)
	run := main.lastRun()
	require.Equal(t, parentID, run.SessionID)
	require.True(t, run.HiddenUserMessage)
	require.Empty(t, run.RunID)
	require.Contains(t, run.Prompt, `"dispatch_id": "d-deliver"`)
	require.Contains(t, run.Prompt, `"key_findings": "fixed the bug"`)
	require.Contains(t, run.Prompt, "Review the diff and decide whether to merge or dismiss")
}

// Delivery is dropped, not panicked on, when the parent session is gone
// (deleted, or a `crush run` process that already exited). The main
// agent is real so the flush reaches the session lookup that drops.
func TestDeliverDispatchResultDroppedForMissingParent(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)
	c.mainAgent = agent
	c.deliverDispatchResult(t.Context(), "no-such-parent-session", dispatch.DispatchResult{
		DispatchID: "d-gone",
		Status:     dispatch.StatusCompleted,
	})
	// The result stays pended only while the parent might come back; a
	// missing parent must not retry forever.
	c.dispatchMu.Lock()
	pended := c.pendingResults["no-such-parent-session"]
	c.dispatchMu.Unlock()
	require.Empty(t, pended)
}

// A result that lands while the parent is busy waits in the pending set,
// outside the queue the user's Esc tears through (#388): a ClearQueue on
// the busy parent leaves it intact, and the next idle delivers it.
func TestDeliverDispatchResultSurvivesClearQueue(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)
	main.busy.Store(true)

	c.deliverDispatchResult(t.Context(), parentID, dispatch.DispatchResult{
		DispatchID:  "d-clear",
		Branch:      "crush-dispatch-d-clear",
		SessionID:   "s-clear",
		Status:      dispatch.StatusCompleted,
		KeyFindings: "survived the clear",
	})
	require.Equal(t, int64(0), main.clears.Load(), "pending must not itself clear anything")
	c.ClearQueue(parentID)
	require.Equal(t, int64(1), main.clears.Load())

	main.busy.Store(false)
	go c.flushPendingResults(parentID)

	require.Eventually(t, func() bool {
		return main.runCount() == 1
	}, 10*time.Second, 50*time.Millisecond)
	run := main.lastRun()
	require.Equal(t, parentID, run.SessionID)
	require.True(t, run.HiddenUserMessage)
	require.Empty(t, run.RunID)
	require.Contains(t, run.Prompt, `"dispatch_id": "d-clear"`)
	require.Contains(t, run.Prompt, "Review the diff and decide whether to merge or dismiss")

	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 1, main.runCount(), "exactly one delivery turn must run")
}

// A pending cancel (Cancel on the busy parent) covers queued prompts, not
// the pending set (#388): the delivery survives and runs on the next idle.
func TestDeliverDispatchResultSurvivesCancel(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)
	main.busy.Store(true)

	c.deliverDispatchResult(t.Context(), parentID, dispatch.DispatchResult{
		DispatchID:  "d-cancel",
		Branch:      "crush-dispatch-d-cancel",
		SessionID:   "s-cancel",
		Status:      dispatch.StatusCompleted,
		KeyFindings: "survived the cancel",
	})
	require.Equal(t, int64(0), main.cancels.Load(), "pending must not itself cancel anything")
	c.Cancel(parentID)
	require.Equal(t, int64(1), main.cancels.Load())

	main.busy.Store(false)
	go c.flushPendingResults(parentID)

	require.Eventually(t, func() bool {
		return main.runCount() == 1
	}, 10*time.Second, 50*time.Millisecond)
	run := main.lastRun()
	require.True(t, run.HiddenUserMessage)
	require.Empty(t, run.RunID)
	require.Contains(t, run.Prompt, `"dispatch_id": "d-cancel"`)

	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 1, main.runCount(), "exactly one delivery turn must run")
}

// A delivery turn that errors goes back into the pending set and retries
// (#388): exactly one extra attempt, and the successful one carries the
// payload.
func TestDeliverDispatchResultRetriesOnError(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)
	main.failures.Store(1)

	c.deliverDispatchResult(t.Context(), parentID, dispatch.DispatchResult{
		DispatchID:  "d-retry",
		Branch:      "crush-dispatch-d-retry",
		SessionID:   "s-retry",
		Status:      dispatch.StatusCompleted,
		KeyFindings: "second time lucky",
	})

	require.Eventually(t, func() bool {
		return main.runCount() == 2
	}, 10*time.Second, 50*time.Millisecond)
	for _, call := range main.runsSnapshot() {
		require.Empty(t, call.RunID, "every delivery attempt strips the tool call's RunID")
	}
	require.Contains(t, main.lastRun().Prompt, `"dispatch_id": "d-retry"`)

	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 2, main.runCount(), "the failed attempt must retry exactly once")
}

// Results that stack up while the parent is busy deliver in one turn,
// each payload's terminal message in order (#388).
func TestDeliverDispatchResultBatchesWhileBusy(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)
	main.busy.Store(true)

	c.deliverDispatchResult(t.Context(), parentID, dispatch.DispatchResult{
		DispatchID: "d-batch-1",
		Branch:     "crush-dispatch-d-batch-1",
		SessionID:  "s-batch-1",
		Status:     dispatch.StatusCompleted,
	})
	c.deliverDispatchResult(t.Context(), parentID, dispatch.DispatchResult{
		DispatchID: "d-batch-2",
		Branch:     "crush-dispatch-d-batch-2",
		SessionID:  "s-batch-2",
		Status:     dispatch.StatusCompleted,
	})
	require.Equal(t, 0, main.runCount())

	main.busy.Store(false)
	go c.flushPendingResults(parentID)

	require.Eventually(t, func() bool {
		return main.runCount() == 1
	}, 10*time.Second, 50*time.Millisecond)
	run := main.lastRun()
	require.Empty(t, run.RunID)
	require.Contains(t, run.Prompt, `"dispatch_id": "d-batch-1"`)
	require.Contains(t, run.Prompt, `"dispatch_id": "d-batch-2"`)

	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 1, main.runCount(), "both results must share one delivery turn")
}

// The queue surfaces never see a queued dispatch delivery (#388): the
// prompt pill counts only user prompts, Esc's clear and cancel leave the
// delivery queued, and the step drain neither folds it into the active
// turn nor drops it under a pending cancel.
func TestQueueKeepsSystemDeliveriesAcrossClearAndCancel(t *testing.T) {
	env := testEnv(t)
	sa := newInjectionSessionAgent(env, &twoStepEchoModel{}, nil)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	sa.messageQueue.Set(sess.ID, []SessionAgentCall{
		{SessionID: sess.ID, Prompt: "user prompt"},
		{SessionID: sess.ID, Prompt: "dispatch payload", systemDelivery: true},
	})

	require.Equal(t, 1, sa.QueuedPrompts(sess.ID), "the pill counts prompts, not deliveries")
	require.Equal(t, []string{"user prompt"}, sa.QueuedPromptsList(sess.ID))

	sa.ClearQueue(sess.ID)
	kept, _ := sa.messageQueue.Get(sess.ID)
	require.Len(t, kept, 1, "ClearQueue must keep the delivery")
	require.True(t, kept[0].systemDelivery)

	sa.Cancel(sess.ID)
	kept, _ = sa.messageQueue.Get(sess.ID)
	require.Len(t, kept, 1, "Cancel must keep the delivery")

	sa.cancelMark.Set(sess.ID, 100)
	fold, canceled := sa.drainQueueForStep(sess.ID)
	require.Empty(t, fold, "the delivery must never fold into the active turn")
	require.Empty(t, canceled)
	kept, _ = sa.messageQueue.Get(sess.ID)
	require.Len(t, kept, 1, "the delivery must survive a pending cancel in the drain")
	require.True(t, kept[0].systemDelivery)
}

// agentSink is a dispatch.TodoSink that records snapshots as history,
// for asserting the collector's delivery: assertions scan the history
// rather than consuming a queue, so one stage's wait cannot steal a
// snapshot a later stage is waiting for when the registry and session
// streams interleave differently under load.
type agentSink struct {
	mu        sync.Mutex
	snapshots []dispatch.TodoSnapshot
}

func newAgentSink() *agentSink {
	return &agentSink{}
}

func (s *agentSink) DispatchTodos(snap dispatch.TodoSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = append(s.snapshots, snap)
}

// until waits (bounded) for a snapshot matching cond and returns it.
func (s *agentSink) until(t *testing.T, cond func(dispatch.TodoSnapshot) bool) dispatch.TodoSnapshot {
	t.Helper()
	var found dispatch.TodoSnapshot
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, snap := range s.snapshots {
			if cond(snap) {
				found = snap
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond)
	return found
}

// Progress flows end to end (#65): the collector — created with the
// registry on the first dispatch — reduces the dispatched session's
// saves and the registry's transitions into snapshots for every sink,
// and the terminal snapshot carries the DispatchResult.
func TestDispatchProgressFlowsToSinks(t *testing.T) {
	dispatched := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
	}
	c, _ := newDispatchToolEnv(t, dispatched)
	sink := newAgentSink()
	c.dispatchSinks = []dispatch.TodoSink{sink}
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"})
	handle := decodeDispatchHandle(t, resp)

	// The first snapshots carry the session and the running state.
	running := sink.until(t, func(s dispatch.TodoSnapshot) bool {
		return s.Entry.SessionID == handle.SessionID && s.Entry.Status == dispatch.StatusRunning
	})
	require.False(t, running.Entry.StartedAt.IsZero())

	// The dispatched session's saves reduce into snapshots: publish one
	// with todos and usage through the real session service.
	sess, err := c.sessions.Get(t.Context(), handle.SessionID)
	require.NoError(t, err)
	sess.Todos = []session.Todo{
		{Content: "fix the bug", ActiveForm: "fixing the bug", Status: session.TodoStatusInProgress},
	}
	sess.PromptTokens = 900
	sess.CompletionTokens = 100
	_, err = c.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	reduced := sink.until(t, func(s dispatch.TodoSnapshot) bool {
		return s.Entry.SessionID == handle.SessionID && s.TodoTotal == 1
	})
	require.Equal(t, "fixing the bug", reduced.CurrentTodo)
	require.Equal(t, int64(900), reduced.PromptTokens)
	require.Equal(t, int64(100), reduced.CompletionTokens)

	// The terminal snapshot carries the DispatchResult.
	terminal := sink.until(t, func(s dispatch.TodoSnapshot) bool {
		return s.Entry.SessionID == handle.SessionID && s.Entry.Status == dispatch.StatusCompleted
	})
	require.NotNil(t, terminal.Entry.Result)
	require.Equal(t, "done", terminal.Entry.Result.KeyFindings)
}

// With every write tool denied there is nothing a dispatch can do
// (#376): the tool refuses before provisioning, so no crush-dispatch-*
// branch or worktree directory is ever created.
func TestDispatchAgentToolRefusedWhenAllWriteToolsDenied(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, env := newDispatchToolEnv(t, agent)
	c.cfg.Config().Options.DisabledTools = []string{
		tools.BashToolName,
		tools.EditToolName,
		tools.MultiEditToolName,
		tools.WriteToolName,
	}
	c.cfg.Config().SetupAgents()
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "do work"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "dispatch unavailable: bash/edit/write are disabled by your configuration")

	// Nothing was provisioned: no crush-dispatch-* branches, no
	// worktree directories, no registry entries — and the fake agent
	// never ran.
	branches, err := exec.CommandContext(t.Context(), "git", "-C", env.workingDir,
		"for-each-ref", "--format=%(refname:short)", "refs/heads/crush-dispatch-*",
	).Output()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(branches)))

	worktrees := filepath.Join(env.workingDir, ".crush", "worktrees")
	entries, err := os.ReadDir(worktrees)
	if err == nil {
		for _, entry := range entries {
			require.NotContains(t, entry.Name(), dispatch.BranchPrefix, "a dispatch worktree was provisioned")
		}
	}

	require.Empty(t, c.dispatchRegistry().List())
	require.Empty(t, agent.calls)
}

// runDispatchToolCallAs is runDispatchToolCall with the tool call ID
// parameterized: each dispatch keys its ephemeral task session off the
// call ID, so tests that dispatch several times need distinct IDs.
func runDispatchToolCallAs(t *testing.T, tool fantasy.AgentTool, params any, callID string) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "dispatch-parent-session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "dispatch-parent-message")
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    callID,
		Name:  DispatchAgentToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

// dispatchBranchCount counts the dispatch branches the repository holds.
func dispatchBranchCount(t *testing.T, workingDir string) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", workingDir,
		"for-each-ref", "--format=%(refname:short)", "refs/heads/"+dispatch.BranchPrefix+"*",
	).Output()
	require.NoError(t, err)
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// The concurrency cap (#390) with a gated fake agent at the cap of 2:
// the third concurrent call returns the at-capacity error and provisions
// nothing, and once one of the held dispatches finishes the next call
// succeeds.
func TestDispatchAgentToolRefusesAtCapacity(t *testing.T) {
	gate := make(chan struct{})
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
		gate:   gate,
	}
	c, env := newDispatchToolEnv(t, agent)
	c.cfg.Config().Options.Dispatch = &config.DispatchOptions{MaxConcurrent: intPtr(2)}
	tool := c.dispatchTool()

	handleA := decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "work A", Branch: "main"}, "dispatch-tool-call-A"))
	handleB := decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "work B", Branch: "main"}, "dispatch-tool-call-B"))

	// The third concurrent call hits the cap: an error, and nothing
	// provisioned — no branch, and the worktrees directory still holds
	// exactly the two running dispatches.
	resp := runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "work C", Branch: "main"}, "dispatch-tool-call-C")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "dispatch at capacity: 2 agents are already running (dispatch.max_concurrent=2)")
	require.Equal(t, 2, dispatchBranchCount(t, env.workingDir))
	wtDir := mustWorktreesDir(t, c)
	entries, err := os.ReadDir(wtDir)
	require.NoError(t, err)
	var dispatchEntries int
	for _, entry := range entries {
		// Count worktree directories, not the per-dispatch .lock and
		// .owner.json sidecar files beside each one.
		if entry.IsDir() && strings.HasPrefix(entry.Name(), dispatch.BranchPrefix) {
			dispatchEntries++
		}
	}
	require.Equal(t, 2, dispatchEntries)

	// Both held dispatches finish, and the freed slot lets the next
	// call through.
	close(gate)
	reg := c.dispatchRegistry()
	require.Eventually(t, func() bool {
		entryA, ok := reg.Get(handleA.DispatchID)
		entryB, okB := reg.Get(handleB.DispatchID)
		if !ok || !okB || entryA.Status == dispatch.StatusRunning || entryB.Status == dispatch.StatusRunning {
			return false
		}
		// The slot is released at the end of the run's teardown, after
		// the registry status flips: wait for the release itself, or the
		// next dispatch below races the still-running teardown and is
		// refused at the cap.
		return c.heldDispatchSlots() == 0
	}, 10*time.Second, 50*time.Millisecond)

	handleD := decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "work D", Branch: "main"}, "dispatch-tool-call-D"))
	require.Equal(t, dispatch.StatusRunning, handleD.Status)
	require.Equal(t, 3, dispatchBranchCount(t, env.workingDir))
}

// Parallel tool calls within one step never go over the cap (#390): six
// parallel dispatches with the cap at 3, exactly three succeed and the
// other three get the at-capacity error.
func TestDispatchAgentToolParallelCallsNeverOvershootCap(t *testing.T) {
	gate := make(chan struct{})
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
		gate:   gate,
	}
	c, _ := newDispatchToolEnv(t, agent)
	c.cfg.Config().Options.Dispatch = &config.DispatchOptions{MaxConcurrent: intPtr(3)}
	tool := c.dispatchTool()

	var wg sync.WaitGroup
	responses := make([]fantasy.ToolResponse, 6)
	errs := make([]error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			input, err := json.Marshal(DispatchAgentParams{Prompt: fmt.Sprintf("work %d", i), Branch: "main"})
			if err != nil {
				errs[i] = err
				return
			}
			ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "dispatch-parent-session")
			ctx = context.WithValue(ctx, tools.MessageIDContextKey, fmt.Sprintf("dispatch-parent-message-%d", i))
			ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
			responses[i], errs[i] = tool.Run(ctx, fantasy.ToolCall{
				ID:    fmt.Sprintf("dispatch-tool-call-%d", i),
				Name:  DispatchAgentToolName,
				Input: string(input),
			})
		}(i)
	}
	wg.Wait()
	close(gate)

	var handles []dispatch.DispatchResult
	for i := 0; i < 6; i++ {
		require.NoError(t, errs[i], "parallel call %d", i)
		if responses[i].IsError {
			require.Contains(t, responses[i].Content, "dispatch at capacity: 3 agents are already running (dispatch.max_concurrent=3)")
			continue
		}
		handles = append(handles, decodeDispatchHandle(t, responses[i]))
	}
	// Exactly the cap succeeds; the rest are refused, never queued or
	// silently dropped.
	require.Len(t, handles, 3)

	// Every admitted run finishes and gives its slot back. The slot is
	// released at the end of the run's teardown, after the registry
	// status flips, so wait for the release itself.
	reg := c.dispatchRegistry()
	require.Eventually(t, func() bool {
		for _, handle := range handles {
			entry, ok := reg.Get(handle.DispatchID)
			if !ok || entry.Status == dispatch.StatusRunning {
				return false
			}
		}
		return c.heldDispatchSlots() == 0
	}, 10*time.Second, 50*time.Millisecond)
}

// A dispatch that fails setup does not use up a slot (#390): with the
// cap at 2, a running dispatch plus one that fails on an unknown skill
// still leaves room for a third call.
func TestDispatchAgentToolFailedSetupReleasesSlot(t *testing.T) {
	gate := make(chan struct{})
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
		gate:   gate,
	}
	c, _ := newDispatchToolEnv(t, agent)
	c.cfg.Config().Options.Dispatch = &config.DispatchOptions{MaxConcurrent: intPtr(2)}
	tool := c.dispatchTool()

	handleA := decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "work A", Branch: "main"}, "dispatch-tool-call-A"))

	// The failed setup must not consume a slot: the unknown skill
	// rejects the call and releases what it reserved.
	resp := runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "work B", Branch: "main", Skills: []string{"no-such-skill"}}, "dispatch-tool-call-B")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "unknown skills: no-such-skill")

	// And the slot it held is free: the third call succeeds while the
	// first dispatch is still running.
	handleC := decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "work C", Branch: "main"}, "dispatch-tool-call-C"))
	require.Equal(t, dispatch.StatusRunning, handleC.Status)
	require.NotEqual(t, handleA.DispatchID, handleC.DispatchID)

	close(gate)
}

// TestDispatchEnforcementFromConfig pins the #402 wiring: the dispatch
// tool's watchdog settings and the dispatched agent's ladder both resolve
// from the worker definition in the config file, so a kill threshold
// configured on the agent trips in both places or neither.
func TestDispatchEnforcementFromConfig(t *testing.T) {
	env := testEnv(t)
	initGitRepo(t, env.workingDir)

	// config.Init discovers crush.json in the working directory, so the
	// file is in place before the coordinator builds its config.
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(`{
		"options": {"todo_enforcement": {"nudge_threshold": 5}},
		"agents": {"worker": {"kill": {"after_ignored_nudges": 3}}}
	}`), 0o644))

	c := newDispatchTestCoordinator(t, env)

	settings := c.dispatchEnforcement(c.cfg.Config().Agents[config.AgentWorker])
	require.Equal(t, 3, settings.KillAfterNudges,
		"the worker definition's kill block feeds the dispatch enforcement")
	require.Equal(t, 5, settings.NudgeThreshold,
		"the global options underlay the dispatched agent's ladder")
	require.Equal(t, 2,
		c.cfg.Config().Agents[config.AgentCoder].
			ResolvedTodoEnforcement(c.cfg.Config().Options.TodoEnforcement).KillAfterNudges,
		"the kill rung stays dispatch-only")

	// The dispatched agent the tool builds carries the same resolved
	// ladder: the watchdog's killSettings and the agent's own
	// enforcement can never diverge.
	const providerID = "test-provider"
	c.cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
	c.cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	c.cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)

	entry, _ := provisionDispatchEntry(t, c, "")
	toolchain, err := c.BuildDispatchToolchain(t.Context(), DispatchToolchainOptions{WorkingDir: entry.Path})
	require.NoError(t, err)
	defer toolchain.Close(t.Context())

	dispatched, err := c.buildDispatchedAgent(t.Context(), dispatchAgentOptions{Toolchain: toolchain})
	require.NoError(t, err)
	sa := dispatched.agent.(*sessionAgent)
	require.NotNil(t, sa.todoEnforcement, "the dispatched agent wires its enforcement ladder")
	require.Equal(t, settings, sa.todoEnforcement.settings,
		"the agent's ladder matches the watchdog's kill settings")
}

// #433: the dispatch tool's schema names every agent a call may run —
// the worker plus each enabled user dispatch agent, never the main or
// task agents — and stamps the configured default into the description.
// fantasy derives the parameter schema from static tags, so the enum is
// injected by the wrapper's Info at call time.
func TestDispatchToolInfoStampsAgentEnum(t *testing.T) {
	c, _ := newDispatchToolEnv(t, &dispatchTestAgent{model: dispatchTestModel()})

	param, ok := c.dispatchTool().Info().Parameters[dispatchAgentParam].(map[string]any)
	require.True(t, ok, "agent parameter missing from the schema")
	require.Equal(t, []string{config.AgentWorker}, param["enum"])

	// A user dispatch agent joins the enum; a disabled one and the
	// main and task agents never do.
	var defs map[string]config.AgentDefinition
	require.NoError(t, json.Unmarshal([]byte(`{
		"reviewer": {"role": "dispatch"},
		"ghost": {"role": "dispatch", "disabled": true}
	}`), &defs))
	c.cfg.Config().AgentDefinitions = defs
	c.cfg.Config().SetupAgents()

	param, ok = c.dispatchTool().Info().Parameters[dispatchAgentParam].(map[string]any)
	require.True(t, ok, "agent parameter missing from the schema")
	require.Equal(t, []string{"reviewer", config.AgentWorker}, param["enum"])
	for _, absent := range []string{config.AgentCoder, config.AgentPlan, config.AgentTask, "ghost"} {
		require.NotContains(t, param["enum"], absent)
	}
	require.Contains(t, param["description"], config.AgentWorker,
		"the description names the configured default agent")
}

// #433: the call's agent parameter chooses the definition the dispatch
// runs on — an explicit id wins, an omitted id takes
// options.dispatch.default_agent, and the chosen id rides the builder
// call and the returned handle.
func TestDispatchAgentParamSelectsDefinition(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)

	var built []dispatchAgentOptions
	c.dispatchAgentBuilder = func(_ context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		built = append(built, opts)
		return &dispatchedAgent{agent: agent, model: agent.model, providerCfg: config.ProviderConfig{ID: "test-provider"}}, nil
	}

	var defs map[string]config.AgentDefinition
	require.NoError(t, json.Unmarshal([]byte(`{"reviewer": {"role": "dispatch"}}`), &defs))
	c.cfg.Config().AgentDefinitions = defs
	c.cfg.Config().SetupAgents()
	tool := c.dispatchTool()

	// Omitted agent with no configured default: the worker.
	handle := decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "p"}, "dispatch-tool-call-1"))
	require.Equal(t, config.AgentWorker, handle.Agent)
	require.Len(t, built, 1)
	require.Equal(t, config.AgentWorker, built[0].AgentID)

	// A configured default applies when the call omits the agent —
	// but an explicit agent wins over it.
	c.cfg.Config().Options.Dispatch = &config.DispatchOptions{DefaultAgent: "reviewer"}

	handle = decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "p"}, "dispatch-tool-call-2"))
	require.Equal(t, "reviewer", handle.Agent)
	require.Len(t, built, 2)
	require.Equal(t, "reviewer", built[1].AgentID)

	handle = decodeDispatchHandle(t, runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "p", Agent: "reviewer"}, "dispatch-tool-call-3"))
	require.Equal(t, "reviewer", handle.Agent)
	require.Len(t, built, 3)
	require.Equal(t, "reviewer", built[2].AgentID)
}

// #433: an unknown, disabled, or non-dispatch agent id is refused with
// the dispatchable ids listed, before anything is provisioned.
func TestDispatchAgentParamRefusals(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)

	var built []dispatchAgentOptions
	c.dispatchAgentBuilder = func(_ context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		built = append(built, opts)
		return &dispatchedAgent{agent: agent, model: agent.model, providerCfg: config.ProviderConfig{ID: "test-provider"}}, nil
	}

	var defs map[string]config.AgentDefinition
	require.NoError(t, json.Unmarshal([]byte(`{
		"reviewer": {"role": "dispatch"},
		"ghost": {"role": "dispatch", "disabled": true}
	}`), &defs))
	c.cfg.Config().AgentDefinitions = defs
	c.cfg.Config().SetupAgents()
	tool := c.dispatchTool()

	tests := []struct {
		name  string
		agent string
		want  string
	}{
		{name: "unknown agent", agent: "nope", want: `unknown agent "nope"`},
		{name: "disabled agent", agent: "ghost", want: `agent "ghost" is disabled`},
		{name: "non-dispatch agent", agent: "coder", want: `agent "coder" is a "main" agent, not a dispatch agent`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "p", Agent: tt.agent})
			require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
			require.Contains(t, resp.Content, tt.want)
			require.Contains(t, resp.Content, "available dispatch agents: reviewer, worker")
		})
	}
	require.Empty(t, built, "a refused call must not build an agent")
}

// #433: the model parameter on a pinned dispatch definition is refused —
// the pin already names the provider and model, so a slot choice would
// be silently ignored. Without the parameter the pinned definition
// dispatches normally.
func TestDispatchAgentParamModelOnPinnedDefinition(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)

	var built []dispatchAgentOptions
	c.dispatchAgentBuilder = func(_ context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		built = append(built, opts)
		return &dispatchedAgent{agent: agent, model: agent.model, providerCfg: config.ProviderConfig{ID: "test-provider"}}, nil
	}

	var defs map[string]config.AgentDefinition
	require.NoError(t, json.Unmarshal([]byte(`{"pinned": {"role": "dispatch", "model": {"provider": "p", "model": "m"}}}`), &defs))
	c.cfg.Config().AgentDefinitions = defs
	c.cfg.Config().SetupAgents()
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "p", Agent: "pinned", Model: "large"})
	require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
	require.Contains(t, resp.Content, `agent "pinned" pins model p/m; omit model`)
	require.Empty(t, built)

	bogus := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "p", Agent: "pinned", Model: "medium"})
	require.True(t, bogus.IsError, "expected a tool error, got: %s", bogus.Content)
	require.Contains(t, bogus.Content, `invalid model "medium"`)

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "p", Agent: "pinned"}))
	require.Equal(t, "pinned", handle.Agent)
	require.Len(t, built, 1)
	require.Empty(t, built[0].ModelType, "no model parameter reaches the builder")
}

// #433: the model parameter overrides a slot-based definition's slot,
// and an omitted parameter leaves the choice to the definition.
func TestDispatchAgentParamSlotOverride(t *testing.T) {
	agent := &dispatchTestAgent{model: dispatchTestModel()}
	c, _ := newDispatchToolEnv(t, agent)

	var built []dispatchAgentOptions
	c.dispatchAgentBuilder = func(_ context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		built = append(built, opts)
		return &dispatchedAgent{agent: agent, model: agent.model, providerCfg: config.ProviderConfig{ID: "test-provider"}}, nil
	}

	var defs map[string]config.AgentDefinition
	require.NoError(t, json.Unmarshal([]byte(`{"reviewer": {"role": "dispatch", "model": "large"}}`), &defs))
	c.cfg.Config().AgentDefinitions = defs
	c.cfg.Config().SetupAgents()
	tool := c.dispatchTool()

	_ = runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "p", Agent: "reviewer", Model: "small"}, "dispatch-tool-call-slot-1")
	require.Len(t, built, 1)
	require.Equal(t, "reviewer", built[0].AgentID)
	require.Equal(t, config.SelectedModelTypeSmall, built[0].ModelType)

	_ = runDispatchToolCallAs(t, tool, DispatchAgentParams{Prompt: "p", Agent: "reviewer"}, "dispatch-tool-call-slot-2")
	require.Len(t, built, 2)
	require.Empty(t, built[1].ModelType, "an omitted model defers to the definition's slot")
}
