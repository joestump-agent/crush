package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedToolCall is one tool call a scripted model makes.
type scriptedToolCall struct {
	name  string
	input string
}

// scriptedStep is one model turn: either tool calls (the run continues)
// or final text (the run ends).
type scriptedStep struct {
	toolCalls []scriptedToolCall
	text      string
}

// scriptedModel is a fantasy.LanguageModel replaying a fixed script, one
// step per Stream call, recording every prompt it was called with. It
// drives sessionAgent.Run through PrepareStep and the tool loop without a
// provider, which is what the todo enforcement ladder's timing tests
// need: the injected nudges must land in the recorded prompts at exactly
// the scripted step.
type scriptedModel struct {
	mu      sync.Mutex
	steps   []scriptedStep
	pos     int
	prompts []fantasy.Call
}

func (m *scriptedModel) Provider() string { return "fake" }
func (m *scriptedModel) Model() string    { return "fake-model" }

func (m *scriptedModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptedModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *scriptedModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// titlePromptMarker identifies the concurrent GenerateTitle goroutine's
// model calls, so the scripted model can serve them a canned title
// without consuming the script or polluting the recorded prompts.
const titlePromptMarker = "You will generate a short title"

func (m *scriptedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// A canceled context (the wander kill, mostly) must end the stream
	// deterministically, not let the script run on.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, titlePromptMarker) {
				return func(yield func(fantasy.StreamPart) bool) {
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
						return
					}
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "session"}) {
						return
					}
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
						return
					}
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
				}, nil
			}
		}
	}

	m.mu.Lock()
	m.prompts = append(m.prompts, call)
	step := scriptedStep{text: "done"}
	if m.pos < len(m.steps) {
		step = m.steps[m.pos]
		m.pos++
	}
	m.mu.Unlock()

	return func(yield func(fantasy.StreamPart) bool) {
		if step.text != "" {
			// A step may carry its state text before tool calls, so a
			// run's last assistant message can be captured mid-script.
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: step.text}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
				return
			}
		}
		if len(step.toolCalls) == 0 {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
			return
		}
		for i, tc := range step.toolCalls {
			id := fmt.Sprintf("tool-%d", i+1)
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: tc.name}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: tc.input}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: id}) {
				return
			}
			// The buffered call is only committed to the step by an explicit
			// ToolCall part; the Input parts alone are streaming decoration.
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: tc.name, ToolCallInput: tc.input}) {
				return
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

// promptTexts returns the text content of every message of every recorded
// model call, one call per entry, so tests can assert which step carried
// an injected nudge.
func (m *scriptedModel) promptTexts() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]string, 0, len(m.prompts))
	for _, call := range m.prompts {
		var texts []string
		for _, msg := range call.Prompt {
			for _, part := range msg.Content {
				if text, ok := part.(fantasy.TextPart); ok {
					texts = append(texts, text.Text)
				}
			}
		}
		out = append(out, texts)
	}
	return out
}

// probeTool is a read-only no-op tool the scripted model can call without
// touching the filesystem.
func probeTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"probe",
		"Probe the environment without changing anything.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		},
	)
}

// mutatingTool is a no-op tool named after a mutating tool (bash), so the
// ladder treats it as mutating while the test stays hermetic.
func mutatingTool(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		name,
		"Mutating placeholder.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("mutated"), nil
		},
	)
}

// todoAgentOpt tweaks the session agent options the test helper builds.
type todoAgentOpt func(*SessionAgentOptions)

// withTodoKill wires the wander-kill observer (#316) onto the agent.
func withTodoKill(fn func(sessionID, reason string)) todoAgentOpt {
	return func(o *SessionAgentOptions) { o.TodoKill = fn }
}

// withLoopStop wires the loop-stop observer (#343) onto the agent.
func withLoopStop(fn func(sessionID string)) todoAgentOpt {
	return func(o *SessionAgentOptions) { o.LoopStop = fn }
}

// withoutTodosTool drops the todos tool the helper otherwise adds, as
// options.disabled_tools does for a real agent.
func withoutTodosTool() todoAgentOpt {
	return func(o *SessionAgentOptions) {
		o.Tools = slices.DeleteFunc(o.Tools, func(t fantasy.AgentTool) bool {
			return t.Info().Name == tools.TodosToolName
		})
	}
}

// newTodoTestAgent builds a session agent with the ladder resolved from
// settings, plus the given tools.
func newTodoTestAgent(t *testing.T, env fakeEnv, model fantasy.LanguageModel, settings config.TodoEnforcementSettings, ts ...fantasy.AgentTool) *sessionAgent {
	return newTodoTestAgentOpts(t, env, model, settings, nil, ts...)
}

func newTodoTestAgentOpts(t *testing.T, env fakeEnv, model fantasy.LanguageModel, settings config.TodoEnforcementSettings, opts []todoAgentOpt, ts ...fantasy.AgentTool) *sessionAgent {
	t.Helper()
	largeModel := Model{
		Model:      model,
		CatwalkCfg: catwalkModelCfg(),
	}
	smallModel := Model{
		Model:      model,
		CatwalkCfg: catwalkModelCfg(),
	}
	ts = append(ts, tools.NewTodosTool(env.sessions))
	options := SessionAgentOptions{
		LargeModel:      largeModel,
		SmallModel:      smallModel,
		SystemPrompt:    "system",
		IsYolo:          true,
		Sessions:        env.sessions,
		Messages:        env.messages,
		Tools:           ts,
		TodoEnforcement: settings,
	}
	for _, opt := range opts {
		opt(&options)
	}
	return NewSessionAgent(options).(*sessionAgent)
}

func catwalkModelCfg() catwalk.Model {
	return catwalk.Model{
		ContextWindow:    200000,
		DefaultMaxTokens: 10000,
	}
}

// nudgeMessages returns the session's persisted user messages that carry
// the given nudge text.
func nudgeMessages(t *testing.T, env fakeEnv, model *scriptedModel, sessID string, nudge string) []message.Message {
	t.Helper()
	msgs, err := env.messages.List(t.Context(), sessID)
	require.NoError(t, err)
	var found []message.Message
	for _, m := range msgs {
		if m.Role == message.User && m.Content().String() == nudge {
			found = append(found, m)
		}
	}
	return found
}

// runWithScript runs the agent on a fresh session and returns it.
func runWithScript(t *testing.T, env fakeEnv, sa *sessionAgent, model *scriptedModel) session.Session {
	t.Helper()
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "do the work",
	})
	require.NoError(t, err)
	return sess
}

// TestTodoNudge_NoTodosRunGetsNudged pins the ladder's core timing: with
// the threshold at two tool calls and no todos activity, the third model
// call's input carries the nudge, and the nudge is persisted on the
// session as a user message (the DoD's "visible in the transcript").
func TestTodoNudge_NoTodosRunGetsNudged(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 2,
	}, probeTool())

	sess := runWithScript(t, env, sa, model)

	prompts := model.promptTexts()
	require.Len(t, prompts, 4)
	// The first two model calls predate the threshold; the third carries
	// the injected nudge as its most recent input.
	assert.NotContains(t, strings.Join(prompts[0], "\n"), todoNudgeMessage)
	assert.NotContains(t, strings.Join(prompts[1], "\n"), todoNudgeMessage)
	assert.Contains(t, strings.Join(prompts[2], "\n"), todoNudgeMessage)

	nudged := nudgeMessages(t, env, model, sess.ID, todoNudgeMessage)
	require.Len(t, nudged, 1, "exactly one nudge must be persisted on the session")
}

// TestTodoNudge_BelowThresholdNoNudge pins the threshold: fewer tool
// calls than the configured threshold, no todos activity, no nudge.
func TestTodoNudge_BelowThresholdNoNudge(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 2,
	}, probeTool())

	sess := runWithScript(t, env, sa, model)

	for _, call := range model.promptTexts() {
		assert.NotContains(t, strings.Join(call, "\n"), todoNudgeMessage)
	}
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage))
}

// TestTodoNudge_MutatingToolTripsImmediately pins the ladder's
// "or the first mutating tool call" clause: a mutating tool call with no
// todos activity trips the nudge regardless of the threshold.
func TestTodoNudge_MutatingToolTripsImmediately(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
	}, mutatingTool("bash"))

	sess := runWithScript(t, env, sa, model)

	prompts := model.promptTexts()
	require.Len(t, prompts, 2)
	assert.Contains(t, strings.Join(prompts[1], "\n"), todoNudgeMessage)
	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage), 1)
}

// TestTodoNudge_TodosActivitySuppressesNudge pins that a todos call is
// activity: a run that records its plan first is never nudged, however
// many tool calls follow.
func TestTodoNudge_TodosActivitySuppressesNudge(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: tools.TodosToolName, input: `{"todos":[{"content":"plan","status":"pending","active_form":"Planning"}]}`}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 2,
	}, probeTool())

	sess := runWithScript(t, env, sa, model)

	for _, call := range model.promptTexts() {
		assert.NotContains(t, strings.Join(call, "\n"), todoNudgeMessage)
	}
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage))
}

// TestTodoNudge_SessionTodosCountAsActivity pins the run-start seed: a
// session that already carries a todo list from an earlier turn is not
// nudged for not re-creating one.
func TestTodoNudge_SessionTodosCountAsActivity(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 2,
	}, probeTool())

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	sess.Todos = []session.Todo{{Content: "leftover", Status: session.TodoStatusPending}}
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "do the work",
	})
	require.NoError(t, err)

	for _, call := range model.promptTexts() {
		assert.NotContains(t, strings.Join(call, "\n"), todoNudgeMessage)
	}
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage))
}

// TestTodoNudge_EscalatesAndCaps pins the escalation ladder: a run that
// keeps ignoring the nudges gets the escalating nudge second, and never a
// third; the cap is where #316's wander kill picks up.
func TestTodoNudge_EscalatesAndCaps(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var steps []scriptedStep
	for i := 0; i < 6; i++ {
		steps = append(steps, scriptedStep{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}})
	}
	steps = append(steps, scriptedStep{text: "done"})
	model := &scriptedModel{steps: steps}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 1,
	}, probeTool())

	sess := runWithScript(t, env, sa, model)

	// Threshold 1: nudge lands in call 2's input, the escalating nudge in
	// call 3's, and nothing after (capped at two per run).
	prompts := model.promptTexts()
	require.Len(t, prompts, 7)
	assert.Contains(t, strings.Join(prompts[1], "\n"), todoNudgeMessage)
	assert.NotContains(t, strings.Join(prompts[1], "\n"), todoEscalatingNudgeMessage)
	assert.Contains(t, strings.Join(prompts[2], "\n"), todoEscalatingNudgeMessage)
	for i := 3; i < len(prompts); i++ {
		assert.NotContains(t, strings.Join(prompts[i], "\n"), todoNudgeMessage)
		assert.NotContains(t, strings.Join(prompts[i], "\n"), todoEscalatingNudgeMessage)
	}
	assert.Len(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage), 1)
	assert.Len(t, nudgeMessages(t, env, model, sess.ID, todoEscalatingNudgeMessage), 1)
}

// TestTodoNudge_DisabledByConfig pins the kill switch: a disabled ladder
// never nudges, whatever the run does.
func TestTodoNudge_DisabledByConfig(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        false,
		NudgeThreshold: 2,
	}, probeTool())

	sess := runWithScript(t, env, sa, model)

	for _, call := range model.promptTexts() {
		assert.NotContains(t, strings.Join(call, "\n"), todoNudgeMessage)
	}
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage))
}

// TestTodoNudge_NoTodosToolNoNudge pins that the ladder stays off for an
// agent without the todos tool: the run that would be nudged on the third
// call with the tool present (TestTodoNudge_NoTodosRunGetsNudged) is never
// asked to call a tool it does not have.
func TestTodoNudge_NoTodosToolNoNudge(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgentOpts(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 2,
	}, []todoAgentOpt{withoutTodosTool()}, probeTool(), mutatingTool("bash"))

	sess := runWithScript(t, env, sa, model)

	for _, call := range model.promptTexts() {
		joined := strings.Join(call, "\n")
		assert.NotContains(t, joined, todoNudgeMessage)
		assert.NotContains(t, joined, todoEscalatingNudgeMessage)
	}
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage))
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoEscalatingNudgeMessage))
}

// TestTodoHardGate_NoTodosToolNoGate pins that the hard gate stays off
// for an agent without the todos tool. With nothing to open it, the gate
// would refuse every mutating call for the rest of the run.
func TestTodoHardGate_NoTodosToolNoGate(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var bashCalls int
	bash := fantasy.NewAgentTool(
		"bash",
		"Run a command.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			bashCalls++
			return fantasy.NewTextResponse("ran"), nil
		},
	)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgentOpts(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
		HardGate:       true,
	}, []todoAgentOpt{withoutTodosTool()}, bash)

	runWithScript(t, env, sa, model)

	assert.Equal(t, 2, bashCalls, "no call may be gated without a todos tool")
}

// TestTodoHardGate_OffByDefault pins the gate's opt-in contract: with the
// gate off, a mutating tool runs without any todo list.
func TestTodoHardGate_OffByDefault(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var bashCalls int
	bash := fantasy.NewAgentTool(
		"bash",
		"Run a command.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			bashCalls++
			return fantasy.NewTextResponse("ran"), nil
		},
	)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
	}, bash)

	runWithScript(t, env, sa, model)

	assert.Equal(t, 1, bashCalls, "the mutating tool must run when the hard gate is off")
}

// TestTodoHardGate_BlocksUntilTodos pins the gate's behavior when opted
// in: the first mutating call is rejected with the gate's reason, the
// todos tool opens the gate, and the retry succeeds.
func TestTodoHardGate_BlocksUntilTodos(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var bashCalls int
	bash := fantasy.NewAgentTool(
		"bash",
		"Run a command.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			bashCalls++
			return fantasy.NewTextResponse("ran"), nil
		},
	)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: tools.TodosToolName, input: `{"todos":[{"content":"plan","status":"in_progress","active_form":"Planning"}]}`}}},
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
		HardGate:       true,
	}, bash)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "do the work",
	})
	require.NoError(t, err)

	// The first bash call was gated, the second ran: one execution.
	assert.Equal(t, 1, bashCalls)

	// The gate's rejection is the tool result the model saw: an error
	// naming the todos tool.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var gateErrs int
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, tr := range m.ToolResults() {
			if tr.IsError && strings.Contains(tr.Content, "hard gate") {
				gateErrs++
			}
		}
	}
	assert.Equal(t, 1, gateErrs, "the gated call must return the gate's error")
}

// TestTodoHardGate_SessionTodosOpenGate pins that the gate, like the
// nudge, treats a session that already has todos as satisfied.
func TestTodoHardGate_SessionTodosOpenGate(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var bashCalls int
	bash := fantasy.NewAgentTool(
		"bash",
		"Run a command.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			bashCalls++
			return fantasy.NewTextResponse("ran"), nil
		},
	)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
		HardGate:       true,
	}, bash)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	sess.Todos = []session.Todo{{Content: "leftover", Status: session.TodoStatusPending}}
	_, err = env.sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "do the work",
	})
	require.NoError(t, err)

	assert.Equal(t, 1, bashCalls, "existing todos must satisfy the gate")
}

// TestWrapTodoGate_LeavesNonMutatingToolsAlone pins the wrap set: only
// the mutating tools get the wrapper; everything else passes through by
// identity.
func TestWrapTodoGate_LeavesNonMutatingToolsAlone(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	probe := probeTool()
	bash := mutatingTool("bash")
	e := newTodoEnforcement(config.TodoEnforcementSettings{
		Enabled:  false,
		HardGate: true,
	}, env.sessions)
	todos := tools.NewTodosTool(env.sessions)
	wrapped := wrapTodoGate([]fantasy.AgentTool{probe, bash, todos}, e)

	assert.Same(t, probe, wrapped[0], "non-mutating tools must not be wrapped")
	assert.NotSame(t, bash, wrapped[1], "mutating tools must be wrapped")
	_, ok := wrapped[1].(*todoGateTool)
	assert.True(t, ok)
	assert.Same(t, todos, wrapped[2], "the todos tool must not be wrapped")

	// Without the todos tool nothing could open the gate, so nothing is
	// wrapped.
	bare := []fantasy.AgentTool{probe, bash}
	assert.Equal(t, bare, wrapTodoGate(bare, e))
}
