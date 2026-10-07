package agent

// This file exposes the dispatch e2e harness to the external test
// package (dispatch_e2e_test.go), which can import internal/a2a — the
// real ServerFactory — without the cycle package agent cannot cross.
// Everything here wraps helpers that already exist in the package's own
// test files.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

// ScriptedToolCall is one tool call a scripted e2e model makes.
type ScriptedToolCall struct {
	Name  string
	Input string
}

// ScriptedStep is one model turn of an e2e dispatch: tool calls keep the
// run going, final text ends it.
type ScriptedStep struct {
	ToolCalls []ScriptedToolCall
	Text      string
}

// NewScriptedModel builds a fantasy.LanguageModel replaying the steps,
// one per stream call, recording every prompt it was called with.
func NewScriptedModel(steps ...ScriptedStep) fantasy.LanguageModel {
	return newScriptedModelFromSteps(steps)
}

// newScriptedModelFromSteps converts the exported step shape onto the
// package's scripted model.
func newScriptedModelFromSteps(steps []ScriptedStep) *scriptedModel {
	out := make([]scriptedStep, 0, len(steps))
	for _, s := range steps {
		calls := make([]scriptedToolCall, 0, len(s.ToolCalls))
		for _, c := range s.ToolCalls {
			calls = append(calls, scriptedToolCall{name: c.Name, input: c.Input})
		}
		out = append(out, scriptedStep{toolCalls: calls, text: s.Text})
	}
	return &scriptedModel{steps: out}
}

// BlockingScriptedModel holds every dispatched stream call until Release
// or the run's context ends, so a test can hold a served run open while
// a watchdog kills it or a client deadline passes. Title calls are
// served immediately.
type BlockingScriptedModel struct {
	fantasy.LanguageModel
	inner *blockingScriptedModel
	once  sync.Once
}

// NewBlockingScriptedModel builds a model that parks on the dispatched
// turn's first stream call.
func NewBlockingScriptedModel(steps ...ScriptedStep) *BlockingScriptedModel {
	scripted := newScriptedModelFromSteps(steps)
	b := &BlockingScriptedModel{
		inner: &blockingScriptedModel{scriptedModel: scripted, hold: make(chan struct{})},
	}
	b.LanguageModel = scripted
	return b
}

// Stream parks like the inner blocking model; everything else promotes
// from the scripted model.
func (b *BlockingScriptedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	return b.inner.Stream(ctx, call)
}

// Release unblocks the held stream call. Safe to call more than once.
func (b *BlockingScriptedModel) Release() {
	b.once.Do(func() { close(b.inner.hold) })
}

// DispatchHarness drives one dispatch end to end: the dispatch_agent
// tool provisions the workspace, the scripted dispatched agent serves
// its turn over the wired starter's A2A server, the coordinator streams
// the dispatch over the A2A client, and the terminal result assembles
// and delivers to a real parent session.
type DispatchHarness struct {
	c        *coordinator
	main     *fakeMainAgent
	tool     fantasy.AgentTool
	parentID string

	mu       sync.Mutex
	terminal map[string]int
	last     map[string]dispatch.Status
}

// NewDispatchHarness builds the harness: a git-rooted coordinator, the
// scripted model installed as the dispatched agent through the real
// dispatched-agent builder, a recording fake main agent on a real
// parent session, the A2A host wired, and a terminal
// transition counter fed by the dispatch registry's event stream. The
// settings resolve through the coordinator's global todo-enforcement
// options — the same path the dispatch tool reads — so the watchdog and
// the A2A backstop arm exactly as configured.
func NewDispatchHarness(t *testing.T, model fantasy.LanguageModel, settings config.TodoEnforcementSettings, host DispatchHost) *DispatchHarness {
	t.Helper()
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)

	// The delivery run resolves the main agent's models from the config
	// store; register the offline test provider the fake main agent
	// reports, mirroring the delivery tests.
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

	// The dispatch tool resolves the kill settings from the global
	// options; stamp the scenario's settings there so they arm through
	// the production path.
	c.cfg.Config().Options.TodoEnforcement = todoEnforcementConfigFrom(settings)

	h := &DispatchHarness{c: c, terminal: make(map[string]int), last: make(map[string]dispatch.Status)}

	c.dispatchAgentBuilder = func(ctx context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		// The toolchain ships its own todos tool and the helper appends
		// one too; keep exactly one.
		toolset := slices.DeleteFunc(slices.Clone(opts.Toolchain.Tools()), func(tool fantasy.AgentTool) bool {
			return tool.Info().Name == tools.TodosToolName
		})
		dispatched := newTodoTestAgentOpts(t, env, model, settings, []todoAgentOpt{
			withTodoKill(opts.TodoKill),
			withLoopStop(opts.LoopStop),
		}, toolset...)
		return &dispatchedAgent{
			agent:       dispatched,
			model:       Model{Model: model, CatwalkCfg: catwalkModelCfg()},
			providerCfg: config.ProviderConfig{},
		}, nil
	}

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	h.parentID = parent.ID

	h.main = &fakeMainAgent{model: dispatchTestModel()}
	c.mainAgent = h.main
	c.mainAgentName = config.AgentCoder
	c.agents = map[string]SessionAgent{config.AgentCoder: h.main}

	if host != nil {
		c.SetDispatchHost(host)
	}

	events := c.dispatchRegistry().Subscribe(t.Context())
	go func() {
		for {
			select {
			case <-t.Context().Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				// The registry republishes the entry on every mutation,
				// terminal states included; count only the crossings from
				// a non-terminal status into a terminal one.
				h.mu.Lock()
				if ev.Payload.Status.IsTerminal() && !h.last[ev.Payload.ID].IsTerminal() {
					h.terminal[ev.Payload.ID]++
				}
				h.last[ev.Payload.ID] = ev.Payload.Status
				h.mu.Unlock()
			}
		}
	}()

	h.tool = c.dispatchTool()
	return h
}

// Dispatch starts one dispatched agent through the real dispatch_agent
// tool and returns its running handle.
func (h *DispatchHarness) Dispatch(t *testing.T, prompt string) dispatch.DispatchResult {
	t.Helper()
	input, err := json.Marshal(DispatchAgentParams{Prompt: prompt, Branch: "main", Handle: "tester"})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, h.parentID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "dispatch-parent-message")
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	resp, err := h.tool.Run(ctx, fantasy.ToolCall{
		ID:    "dispatch-tool-call",
		Name:  DispatchAgentToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return decodeDispatchHandle(t, resp)
}

// ParentSessionID is the session the harness dispatches from.
func (h *DispatchHarness) ParentSessionID() string { return h.parentID }

// PromptingPermissions gives the parent a permission service that
// prompts — no yolo, no allowlist — and returns it, so a test can watch
// and decide the requests its dispatched agents raise (#353). Call it
// before Dispatch: the dispatch's scoped service is built on the
// parent's when the toolchain is.
func (h *DispatchHarness) PromptingPermissions(t *testing.T) permission.Service {
	t.Helper()
	svc := permission.NewPermissionService(t.TempDir(), false, nil)
	h.c.permissions = svc
	return svc
}

// WaitTerminal blocks until the dispatch's registry entry is terminal
// and returns it.
func (h *DispatchHarness) WaitTerminal(t *testing.T, dispatchID string) dispatch.Entry {
	t.Helper()
	require.Eventually(t, func() bool {
		entry, ok := h.c.dispatchRegistry().Get(dispatchID)
		return ok && entry.Status.IsTerminal()
	}, 30*time.Second, 25*time.Millisecond)
	entry, ok := h.c.dispatchRegistry().Get(dispatchID)
	require.True(t, ok)
	return entry
}

// TerminalTransitions returns how many terminal status events the
// registry streamed for the dispatch — exactly one per run, whatever
// the terminal state is.
func (h *DispatchHarness) TerminalTransitions(dispatchID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.terminal[dispatchID]
}

// ParentDeliveries returns how many turns the parent's main agent ran —
// one per delivered terminal result.
func (h *DispatchHarness) ParentDeliveries() int {
	return h.main.runCount()
}

// SubAgentParams is the exported alias of the sub-agent turn's
// parameters, so the external test package can drive runSubAgentOverA2A
// (#392) with the same shape the tools build.
type SubAgentParams = subAgentParams

// RunSubAgentOverA2A drives one sub-agent turn through the A2A runtime
// on c — the production path the agentic_fetch tool uses (#392). The
// coordinator must have a DispatchHost wired.
func RunSubAgentOverA2A(c Coordinator, ctx context.Context, params SubAgentParams) (fantasy.ToolResponse, error) {
	co, ok := c.(*coordinator)
	if !ok {
		return fantasy.ToolResponse{}, errors.New("coordinator is not *agent.coordinator")
	}
	return co.runSubAgentOverA2A(ctx, params)
}

// NewSubAgentMock builds a mock SessionAgent whose Run delegates to
// runFunc, for driving sub-agent turns without a real model.
func NewSubAgentMock(providerID string, maxTokens int64, runFunc func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)) SessionAgent {
	return newMockAgent(providerID, maxTokens, runFunc)
}

// SubAgentMockCancellations returns the session IDs Cancel was called
// with on a mock built by NewSubAgentMock.
func SubAgentMockCancellations(a SessionAgent) []string {
	m, ok := a.(*mockSessionAgent)
	if !ok {
		return nil
	}
	return m.cancellations()
}

// todoEnforcementConfigFrom inverts config.ResolveTodoEnforcement for
// the knobs the e2e scenarios set: only fields that differ from the
// defaults are stamped, so the tool's resolution reads back the
// settings the harness was built with. The A2A inactivity backstop is
// configured in whole seconds; a sub-second value is dropped.
func todoEnforcementConfigFrom(settings config.TodoEnforcementSettings) *config.TodoEnforcementConfig {
	defaults := config.ResolveTodoEnforcement(nil, nil)
	cfg := &config.TodoEnforcementConfig{}
	if settings.Enabled != defaults.Enabled {
		enabled := settings.Enabled
		cfg.Enabled = &enabled
	}
	if settings.NudgeThreshold != defaults.NudgeThreshold {
		v := config.IntOrOff(settings.NudgeThreshold)
		cfg.NudgeThreshold = &v
	}
	if settings.HardGate != defaults.HardGate {
		gate := settings.HardGate
		cfg.HardGate = &gate
	}
	if settings.KillAfterNudges != defaults.KillAfterNudges {
		v := config.IntOrOff(settings.KillAfterNudges)
		cfg.KillAfterNudges = &v
	}
	if settings.StallWindow != 0 {
		d := config.Duration(settings.StallWindow)
		cfg.StallWindow = &d
	}
	if settings.HardTimeout != 0 {
		d := config.Duration(settings.HardTimeout)
		cfg.HardTimeout = &d
	}
	if settings.InactivityTimeout != 0 {
		secs := int(settings.InactivityTimeout / time.Second)
		cfg.InactivityTimeout = &secs
	}
	return cfg
}
