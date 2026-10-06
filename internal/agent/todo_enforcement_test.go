package agent

import (
	"context"
	"encoding/json"
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

// isTitleCall reports whether a model call is the GenerateTitle
// goroutine's, by the same marker the scripted model serves canned
// titles on.
func isTitleCall(call fantasy.Call) bool {
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, titlePromptMarker) {
				return true
			}
		}
	}
	return false
}

func (m *scriptedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// A canceled context (the wander kill, mostly) must end the stream
	// deterministically, not let the script run on.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isTitleCall(call) {
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

// countingTool is a no-op tool named after a real one, recording its
// executions so the gate tests can tell a gated call from a run one.
func countingTool(name string, calls *int) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		name,
		"Counting placeholder.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			*calls++
			return fantasy.NewTextResponse("ok"), nil
		},
	)
}

// fakeMCPToolNoHint is a no-op tool duck-typing an MCP tool the server
// did not annotate at all.
type fakeMCPToolNoHint struct {
	fantasy.AgentTool
}

func (f *fakeMCPToolNoHint) MCP() string { return "fake-server" }

// fakeMCPTool is a no-op tool duck-typing an annotated MCP tool, so the
// ladder's classification tests can cover the read-only hint without a
// server.
type fakeMCPTool struct {
	fantasy.AgentTool
	readOnly bool
}

func (f *fakeMCPTool) MCP() string { return "fake-server" }

func (f *fakeMCPTool) ReadOnlyHint() bool { return f.readOnly }

// TestIsMutatingCall pins the ladder's mutation classification (#395):
// the file writers count outright, bash only when the command is not a
// single read-only utility, MCP tools unless the server marks them
// read-only, and the readers never.
func TestIsMutatingCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		tool  fantasy.AgentTool
		input string
		want  bool
	}{
		{"write", mutatingTool(tools.WriteToolName), `{}`, true},
		{"edit", mutatingTool(tools.EditToolName), `{}`, true},
		{"multiedit", mutatingTool(tools.MultiEditToolName), `{}`, true},
		{"download", mutatingTool(tools.DownloadToolName), `{}`, true},
		{"lsp_rename", mutatingTool(tools.RenameToolName), `{}`, true},
		{"lsp_replace_symbol", mutatingTool(tools.ReplaceSymbolToolName), `{}`, true},
		{"bash read-only git status", mutatingTool(tools.BashToolName), `{"command":"git status"}`, false},
		{"bash read-only ls", mutatingTool(tools.BashToolName), `{"command":"ls -la"}`, false},
		{"bash mutating rm", mutatingTool(tools.BashToolName), `{"command":"rm x"}`, true},
		{"bash chained", mutatingTool(tools.BashToolName), `{"command":"git status && rm x"}`, true},
		{"bash wrapped by timeout", mutatingTool(tools.BashToolName), `{"command":"timeout 5 rm x"}`, true},
		{"bash empty object input", mutatingTool(tools.BashToolName), `{}`, true},
		{"bash unparseable input", mutatingTool(tools.BashToolName), `{not json`, true},
		{"bash empty command", mutatingTool(tools.BashToolName), `{"command":""}`, true},
		{"mcp without hint", &fakeMCPToolNoHint{mutatingTool("mcp_fake-server_run")}, `{}`, true},
		{"mcp read-only hint", &fakeMCPTool{AgentTool: mutatingTool("mcp_fake-server_lookup"), readOnly: true}, `{}`, false},
		{"mcp hint false", &fakeMCPTool{AgentTool: mutatingTool("mcp_fake-server_run"), readOnly: false}, `{}`, true},
		{"hooked write", newHookedTool(mutatingTool(tools.WriteToolName), nil), `{}`, true},
		{"hooked mcp without hint", newHookedTool(&fakeMCPToolNoHint{mutatingTool("mcp_fake-server_run")}, nil), `{}`, true},
		{"hooked mcp read-only hint", newHookedTool(&fakeMCPTool{AgentTool: mutatingTool("mcp_fake-server_lookup"), readOnly: true}, nil), `{}`, false},
		{"probe reader", probeTool(), `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isMutatingCall(tt.tool, tt.input), "isMutatingCall(%q, %q)", tt.tool.Info().Name, tt.input)
		})
	}
}

// TestTodoNudge_ReadOnlyBashDoesNotTripImmediately pins the fix for the
// over-match: a bash call running a read-only command is a plain tool
// call, not a mutating one, so a single git status far below the
// threshold trips no nudge.
func TestTodoNudge_ReadOnlyBashDoesNotTripImmediately(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: tools.BashToolName, input: `{"command":"git status"}`}}},
		{toolCalls: []scriptedToolCall{{name: tools.BashToolName, input: `{"command":"ls -la"}`}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
	}, mutatingTool(tools.BashToolName))

	sess := runWithScript(t, env, sa, model)

	for _, call := range model.promptTexts() {
		assert.NotContains(t, strings.Join(call, "\n"), todoNudgeMessage)
	}
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage))
}

// TestTodoNudge_ReadOnlyBashCountsTowardThreshold pins the other half of
// the bash fix: a read-only command is still a tool call, so it nudges
// at the threshold like any other call.
func TestTodoNudge_ReadOnlyBashCountsTowardThreshold(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: tools.BashToolName, input: `{"command":"git status"}`}}},
		{toolCalls: []scriptedToolCall{{name: tools.BashToolName, input: `{"command":"ls -la"}`}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 2,
	}, mutatingTool(tools.BashToolName))

	sess := runWithScript(t, env, sa, model)

	prompts := model.promptTexts()
	require.Len(t, prompts, 3)
	assert.NotContains(t, strings.Join(prompts[1], "\n"), todoNudgeMessage)
	assert.Contains(t, strings.Join(prompts[2], "\n"), todoNudgeMessage)
	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage), 1)
}

// TestTodoNudge_MutatingBashTripsImmediately pins the fail-closed side:
// a bash command that is not a single read-only utility trips the
// immediate nudge, chained or not.
func TestTodoNudge_MutatingBashTripsImmediately(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"chained", `{"command":"git status && rm x"}`},
		{"wrapped by timeout", `{"command":"timeout 5 rm x"}`},
		{"unparseable", `{not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			model := &scriptedModel{steps: []scriptedStep{
				{toolCalls: []scriptedToolCall{{name: tools.BashToolName, input: tc.input}}},
				{text: "done"},
			}}
			sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
				Enabled:        true,
				NudgeThreshold: 10,
			}, mutatingTool(tools.BashToolName))

			sess := runWithScript(t, env, sa, model)

			prompts := model.promptTexts()
			require.Len(t, prompts, 2)
			assert.Contains(t, strings.Join(prompts[1], "\n"), todoNudgeMessage)
			require.Len(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage), 1)
		})
	}
}

// TestTodoNudge_MCPReadOnlyHintNoImmediateNudge pins the acceptance for
// annotated MCP tools: a tool the server marked readOnlyHint is neither
// a mutating call nor an immediate nudge trigger.
func TestTodoNudge_MCPReadOnlyHintNoImmediateNudge(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	tool := &fakeMCPTool{AgentTool: mutatingTool("mcp_fake-server_lookup"), readOnly: true}
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: tool.Info().Name, input: `{}`}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
	}, tool)

	sess := runWithScript(t, env, sa, model)

	prompts := model.promptTexts()
	require.Len(t, prompts, 2)
	assert.NotContains(t, strings.Join(prompts[1], "\n"), todoNudgeMessage)
	assert.Empty(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage))
}

// gateErrorCount counts the persisted tool results carrying the gate's
// rejection, so the gate tests can assert exactly one gated call.
func gateErrorCount(t *testing.T, env fakeEnv, sessID string) int {
	t.Helper()
	msgs, err := env.messages.List(t.Context(), sessID)
	require.NoError(t, err)
	var count int
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, tr := range m.ToolResults() {
			if tr.IsError && strings.Contains(tr.Content, "hard gate") {
				count++
			}
		}
	}
	return count
}

// TestTodoHardGate_RejectsNewlyClassifiedMutatingTools pins the fix for
// the under-match: with the gate on and no todo list, each of the file
// writers the ladder now classifies (lsp_rename, lsp_replace_symbol,
// download) and an MCP tool without a read-only hint is rejected with
// the gate's reason and never executed.
func TestTodoHardGate_RejectsNewlyClassifiedMutatingTools(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		make func(calls *int) fantasy.AgentTool
	}{
		{"lsp_rename", func(calls *int) fantasy.AgentTool { return countingTool(tools.RenameToolName, calls) }},
		{"lsp_replace_symbol", func(calls *int) fantasy.AgentTool { return countingTool(tools.ReplaceSymbolToolName, calls) }},
		{"download", func(calls *int) fantasy.AgentTool { return countingTool(tools.DownloadToolName, calls) }},
		{"mcp without hint", func(calls *int) fantasy.AgentTool {
			return &fakeMCPToolNoHint{countingTool("mcp_fake-server_run", calls)}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t)
			var calls int
			tool := tc.make(&calls)
			model := &scriptedModel{steps: []scriptedStep{
				{toolCalls: []scriptedToolCall{{name: tool.Info().Name, input: `{}`}}},
				{text: "done"},
			}}
			sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
				Enabled:        true,
				NudgeThreshold: 10,
				HardGate:       true,
			}, tool)

			sess, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			_, err = sa.Run(t.Context(), SessionAgentCall{
				SessionID: sess.ID,
				Prompt:    "do the work",
			})
			require.NoError(t, err)

			assert.Equal(t, 0, calls, "the gated tool must not run without a todo list")
			assert.Equal(t, 1, gateErrorCount(t, env, sess.ID), "the gated call must return the gate's error")
		})
	}
}

// TestTodoHardGate_MCPReadOnlyHintNotGated pins the other side of the
// MCP classification: a tool the server annotated readOnlyHint is never
// wrapped, so it runs even with the gate on and no todo list.
func TestTodoHardGate_MCPReadOnlyHintNotGated(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var calls int
	tool := &fakeMCPTool{AgentTool: countingTool("mcp_fake-server_lookup", &calls), readOnly: true}
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: tool.Info().Name, input: `{}`}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
		HardGate:       true,
	}, tool)

	sess := runWithScript(t, env, sa, model)

	assert.Equal(t, 1, calls, "a read-only MCP tool must run with the gate on")
	assert.Equal(t, 0, gateErrorCount(t, env, sess.ID))
}

// TestTodoHardGate_ReadOnlyBashPasses pins the gate's bash carve-out:
// with the gate on and no todo list, a read-only command runs and a
// mutating one is gated.
func TestTodoHardGate_ReadOnlyBashPasses(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var commands []string
	bash := fantasy.NewAgentTool(
		tools.BashToolName,
		"Run a command.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var p struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(call.Input), &p); err == nil {
				commands = append(commands, p.Command)
			}
			return fantasy.NewTextResponse("ran"), nil
		},
	)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: tools.BashToolName, input: `{"command":"git status"}`}}},
		{toolCalls: []scriptedToolCall{{name: tools.BashToolName, input: `{"command":"rm x"}`}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:        true,
		NudgeThreshold: 10,
		HardGate:       true,
	}, bash)

	sess := runWithScript(t, env, sa, model)

	assert.Equal(t, []string{"git status"}, commands, "the read-only command must run, the mutating one must be gated")
	assert.Equal(t, 1, gateErrorCount(t, env, sess.ID), "the mutating call must return the gate's error")
}

// TestWrapTodoGate_WrapsMutatingTools pins the widened wrap set: the
// file writers, bash, and MCP tools without a read-only hint are
// wrapped; MCP tools with the hint and the readers are not.
func TestWrapTodoGate_WrapsMutatingTools(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	write := mutatingTool(tools.WriteToolName)
	download := mutatingTool(tools.DownloadToolName)
	rename := mutatingTool(tools.RenameToolName)
	bash := mutatingTool(tools.BashToolName)
	mcpNoHint := &fakeMCPToolNoHint{mutatingTool("mcp_fake-server_write")}
	mcpHint := &fakeMCPTool{AgentTool: mutatingTool("mcp_fake-server_read"), readOnly: true}
	probe := probeTool()
	e := newTodoEnforcement(config.TodoEnforcementSettings{
		Enabled:  false,
		HardGate: true,
	}, env.sessions)
	todos := tools.NewTodosTool(env.sessions)

	wrapped := wrapTodoGate([]fantasy.AgentTool{write, download, rename, bash, mcpNoHint, mcpHint, probe, todos}, e)

	assert.NotSame(t, write, wrapped[0], "write must be wrapped")
	assert.NotSame(t, download, wrapped[1], "download must be wrapped")
	assert.NotSame(t, rename, wrapped[2], "lsp_rename must be wrapped")
	assert.NotSame(t, bash, wrapped[3], "bash must be wrapped")
	assert.NotSame(t, mcpNoHint, wrapped[4], "an MCP tool without a read-only hint must be wrapped")
	assert.Same(t, mcpHint, wrapped[5], "a read-only-hint MCP tool must not be wrapped")
	assert.Same(t, probe, wrapped[6], "readers must not be wrapped")
	assert.Same(t, todos, wrapped[7], "the todos tool must not be wrapped")

	_, ok := wrapped[0].(*todoGateTool)
	assert.True(t, ok)
}

// TestWrapTodoGate_ClassifiesThroughHookWrapper pins classification
// through the agent's own wrappers: the tools reach the gate already
// wrapped with the hook runner, so the wrap decision must unwrap them —
// a hooked MCP tool without a read-only hint is gated, a hooked
// read-only MCP tool and a hooked reader are not.
func TestWrapTodoGate_ClassifiesThroughHookWrapper(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	mcpNoHint := newHookedTool(&fakeMCPToolNoHint{mutatingTool("mcp_fake-server_write")}, nil)
	mcpHint := newHookedTool(&fakeMCPTool{AgentTool: mutatingTool("mcp_fake-server_read"), readOnly: true}, nil)
	probe := newHookedTool(probeTool(), nil)
	e := newTodoEnforcement(config.TodoEnforcementSettings{
		Enabled:  false,
		HardGate: true,
	}, env.sessions)
	todos := tools.NewTodosTool(env.sessions)

	wrapped := wrapTodoGate([]fantasy.AgentTool{mcpNoHint, mcpHint, probe, todos}, e)

	assert.NotSame(t, mcpNoHint, wrapped[0], "a hooked MCP tool without a read-only hint must be wrapped")
	assert.Same(t, mcpHint, wrapped[1], "a hooked read-only MCP tool must not be wrapped")
	assert.Same(t, probe, wrapped[2], "a hooked reader must not be wrapped")
	assert.Same(t, todos, wrapped[3], "the todos tool must not be wrapped")

	gate, ok := wrapped[0].(*todoGateTool)
	require.True(t, ok)
	assert.Same(t, mcpNoHint, gate.inner, "the gate must wrap the hooked tool the model calls")
}
