package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// twoStepEchoModel is a fake language model whose first stream step emits
// a tool call to "echo" and whose second step returns text. Every step
// captures the messages it was called with, so a test can assert what the
// model saw after an injected message was folded in.
type twoStepEchoModel struct {
	mu      sync.Mutex
	prompts []fantasy.Prompt
}

func (m *twoStepEchoModel) Provider() string { return "fake" }
func (m *twoStepEchoModel) Model() string    { return "fake-model" }

func (m *twoStepEchoModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "done"}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *twoStepEchoModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	step := len(m.prompts)
	m.prompts = append(m.prompts, call.Prompt)
	m.mu.Unlock()
	return func(yield func(fantasy.StreamPart) bool) {
		// Every turn's first step is a tool call; the step after it (the
		// one a folded injection lands in) answers with text.
		if step%2 == 0 {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tool-1", ToolCallName: "echo"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tool-1"}) {
				return
			}
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "tool-1",
				ToolCallName:  "echo",
				ToolCallInput: `{}`,
			}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "steered answer"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *twoStepEchoModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.DeadlineExceeded
}

func (m *twoStepEchoModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.DeadlineExceeded
}

// gatedEchoTool builds the tool the two-step model calls: it blocks until
// its gate is released. The block is the window a test injects through —
// the turn is active, the model is between steps. The returned state
// exposes the entered/gate channels.
type gatedEchoState struct {
	entered chan struct{}
	gate    chan struct{}
}

func (t *gatedEchoState) onceEntered() {
	select {
	case <-t.entered:
	default:
		close(t.entered)
	}
}

func newGatedEchoTool() (fantasy.AgentTool, *gatedEchoState) {
	state := &gatedEchoState{entered: make(chan struct{}), gate: make(chan struct{})}
	return fantasy.NewAgentTool(
		"echo",
		"echo",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			state.onceEntered()
			select {
			case <-state.gate:
			case <-ctx.Done():
			}
			return fantasy.NewTextResponse("ok"), nil
		},
	), state
}

// fantasyMessageText extracts the text content of a fantasy message.
func fantasyMessageText(msg fantasy.Message) string {
	var b strings.Builder
	for _, part := range msg.Content {
		if tp, ok := part.(fantasy.TextPart); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}

func newInjectionSessionAgent(env fakeEnv, model fantasy.LanguageModel, tools []fantasy.AgentTool) *sessionAgent {
	small := &finishStreamModel{text: "title"}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel: Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SmallModel: Model{Model: small, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		IsSubAgent: true,
		Sessions:   env.sessions,
		Messages:   env.messages,
		Tools:      tools,
	}).(*sessionAgent)
}

// TestEnqueueWhenBusyFoldsInjectionIntoRunningTurn is the mid-run
// contract (#312): a message enqueued while the agent's turn is active is
// folded into the next step's input — the second model call sees it as a
// user message — the injected text is persisted on the session as the
// agent's next input, and the turn's answer streams onto the session's
// assistant messages. The injection never starts a turn of its own.
func TestEnqueueWhenBusyFoldsInjectionIntoRunningTurn(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &twoStepEchoModel{}
	echo, echoState := newGatedEchoTool()
	sa := newInjectionSessionAgent(env, model, []fantasy.AgentTool{echo})

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{
			SessionID:      sess.ID,
			Prompt:         "original task",
			NonInteractive: true,
		})
		runDone <- runErr
	}()

	// Wait until the first tool call is executing: the turn is active,
	// the model is between steps.
	select {
	case <-echoState.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run never reached the tool call")
	}
	require.True(t, sa.IsSessionBusy(sess.ID), "the turn must be active while the tool runs")

	// Inject behind the busy session — the primitive behind #312's queue.
	require.True(t, sa.EnqueueWhenBusy(SessionAgentCall{
		SessionID:      sess.ID,
		Prompt:         "stop writing Rust",
		NonInteractive: true,
	}), "injection into a busy session must enqueue")
	require.Equal(t, 1, sa.QueuedPrompts(sess.ID))

	// Release the tool so the turn proceeds to its second step, which
	// drains the queue into the step's input.
	close(echoState.gate)
	require.NoError(t, <-runDone)

	// The second model call carried the injected message as a user turn.
	model.mu.Lock()
	prompts := append([]fantasy.Prompt(nil), model.prompts...)
	model.mu.Unlock()
	require.Len(t, prompts, 2, "the turn must have exactly two model steps")
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the injection must be consumed, not left queued")
	var injected bool
	for _, msg := range prompts[1] {
		if msg.Role == fantasy.MessageRoleUser && fantasyMessageText(msg) == "stop writing Rust" {
			injected = true
		}
	}
	require.True(t, injected, "the second model step must include the injected user message")

	// The injected text persisted as the agent's next input, and the
	// turn's answer persisted as an assistant message — the message
	// events the parent dispatch block renders (#312's streaming path).
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userTexts, assistantTexts []string
	for _, m := range msgs {
		switch m.Role {
		case message.User:
			userTexts = append(userTexts, m.Content().Text)
		case message.Assistant:
			if text := m.Content().Text; text != "" {
				assistantTexts = append(assistantTexts, text)
			}
		}
	}
	require.Equal(t, []string{"original task", "stop writing Rust"}, userTexts,
		"the injected message lands as the agent's next input, after the original prompt")
	require.Contains(t, assistantTexts, "steered answer")

	// A finished run refuses: task sessions are never continuable, and an
	// idle session must not quietly start a new turn.
	require.False(t, sa.EnqueueWhenBusy(SessionAgentCall{SessionID: sess.ID, Prompt: "too late"}),
		"an idle session must refuse an injection")
}

// TestEnqueueWhenBusyOrderingAndConcurrency is the queue contract: FIFO
// delivery order for sequential sends, and no loss under concurrent sends
// (run under -race).
func TestEnqueueWhenBusyOrderingAndConcurrency(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &twoStepEchoModel{}
	echo, echoState := newGatedEchoTool()
	sa := newInjectionSessionAgent(env, model, []fantasy.AgentTool{echo})

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "task"})
		runDone <- runErr
	}()
	select {
	case <-echoState.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run never reached the tool call")
	}

	// Sequential sends preserve order.
	for _, text := range []string{"first", "second", "third"} {
		require.True(t, sa.EnqueueWhenBusy(SessionAgentCall{SessionID: sess.ID, Prompt: text}))
	}
	require.Equal(t, []string{"first", "second", "third"}, sa.QueuedPromptsList(sess.ID))

	// Concurrent sends all land; the queue stays consistent (failures are
	// collected rather than asserted from the goroutines).
	var wg sync.WaitGroup
	results := make(chan bool, 32)
	for range 32 {
		wg.Go(func() {
			results <- sa.EnqueueWhenBusy(SessionAgentCall{SessionID: sess.ID, Prompt: "burst"})
		})
	}
	wg.Wait()
	close(results)
	for ok := range results {
		require.True(t, ok, "a concurrent injection into the busy session must enqueue")
	}
	require.Equal(t, 35, sa.QueuedPrompts(sess.ID))

	close(echoState.gate)
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after the gates were released")
	}
}

// gatedFinalTextModel is a scripted model for #397's final-step window:
// its first step streams the work's final answer and blocks midway
// through the text step — the turn has no step left for a queued steer
// to fold into, yet the session stays busy until Run releases it — and
// its second step answers the steer.
type gatedFinalTextModel struct {
	mu      sync.Mutex
	calls   int
	midText chan struct{}
	gate    chan struct{}
	midOnce sync.Once
}

func newGatedFinalTextModel() *gatedFinalTextModel {
	return &gatedFinalTextModel{
		midText: make(chan struct{}),
		gate:    make(chan struct{}),
	}
}

func (m *gatedFinalTextModel) Provider() string { return "fake" }
func (m *gatedFinalTextModel) Model() string    { return "fake-model" }

// callsCount returns how many times the model was called.
func (m *gatedFinalTextModel) callsCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *gatedFinalTextModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "ack, noted"}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *gatedFinalTextModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	step := m.calls
	m.calls++
	m.mu.Unlock()
	text := "ORIGINAL FINDINGS: fixed 3 bugs; all verified."
	if step > 0 {
		text = "ack, noted"
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if step == 0 {
			// The mid-text block is #397's window: signal, then hold
			// the stream until the test releases it.
			m.midOnce.Do(func() { close(m.midText) })
			select {
			case <-m.gate:
			case <-ctx.Done():
				return
			}
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *gatedFinalTextModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.DeadlineExceeded
}

func (m *gatedFinalTextModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.DeadlineExceeded
}

// stepsCount returns how many model steps the two-step model served.
func (m *twoStepEchoModel) stepsCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

// newFinalStepDispatchEnv builds a dispatch environment whose dispatched
// agent is a real session agent on the given scripted model, so the run
// and the injection queue both behave for real.
func newFinalStepDispatchEnv(t *testing.T, env fakeEnv, model fantasy.LanguageModel, tools []fantasy.AgentTool) *coordinator {
	t.Helper()
	sa := newInjectionSessionAgent(env, model, tools)
	c := newDispatchTestCoordinator(t, env)
	c.dispatchAgentBuilder = func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error) {
		return &dispatchedAgent{
			agent:       sa,
			model:       dispatchTestModel(),
			providerCfg: config.ProviderConfig{ID: "test-provider"},
		}, nil
	}
	return c
}

// waitDispatchTerminal waits until the dispatch's registry entry turns
// terminal and returns it.
func waitDispatchTerminal(t *testing.T, c *coordinator, dispatchID string) dispatch.Entry {
	t.Helper()
	reg := c.dispatchRegistry()
	var e dispatch.Entry
	require.Eventually(t, func() bool {
		var ok bool
		e, ok = reg.Get(dispatchID)
		return ok && e.Status.IsTerminal()
	}, 10*time.Second, 50*time.Millisecond)
	return e
}

// A steer accepted while the final step is streaming (#397) runs as the
// follow-up turn: the terminal result's key findings are the work turn's
// report, the steer's reply lands in steer_replies, and the model
// served exactly two turns.
func TestFinalStepSteerKeepsDispatchFindings(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	model := newGatedFinalTextModel()
	c := newFinalStepDispatchEnv(t, env, model, nil)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "find the bugs",
		Branch: "main",
		Handle: "findings",
	}))

	// The work turn is streaming its final answer: the session is busy
	// and no step remains for a queued steer to fold into.
	select {
	case <-model.midText:
	case <-time.After(10 * time.Second):
		t.Fatal("work turn never reached its final text step")
	}
	require.NoError(t, c.DeliverAgentMessage(t.Context(), AgentMessage{
		SessionID: handle.SessionID,
		Text:      "stop, that is not the issue",
	}))

	// Release the stream; the steer runs as the follow-up turn.
	close(model.gate)

	e := waitDispatchTerminal(t, c, handle.DispatchID)
	require.Equal(t, dispatch.StatusCompleted, e.Status)
	require.NotNil(t, e.Result)
	// The findings are the work turn's report, not the steer's reply.
	require.Equal(t, "ORIGINAL FINDINGS: fixed 3 bugs; all verified.", e.Result.KeyFindings)
	require.Equal(t, []string{"ack, noted"}, e.Result.SteerReplies)
	require.Equal(t, 2, model.callsCount(), "the work turn plus the steer's follow-up turn")
}

// A steer folded into the running turn (#397's other half) is part of
// the work turn: the findings are unchanged and steer_replies stays
// empty, with no follow-up turn served.
func TestFoldedSteerLeavesDispatchFindingsUnchanged(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	model := &twoStepEchoModel{}
	echo, echoState := newGatedEchoTool()
	c := newFinalStepDispatchEnv(t, env, model, []fantasy.AgentTool{echo})
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "original task",
		Branch: "main",
		Handle: "folded",
	}))

	// The first step is executing its tool call: a steer delivered now
	// folds into the running turn's next step instead of queueing behind
	// the run.
	select {
	case <-echoState.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("run never reached the tool call")
	}
	require.NoError(t, c.DeliverAgentMessage(t.Context(), AgentMessage{
		SessionID: handle.SessionID,
		Text:      "fold me in",
	}))
	close(echoState.gate)

	e := waitDispatchTerminal(t, c, handle.DispatchID)
	require.Equal(t, dispatch.StatusCompleted, e.Status)
	require.NotNil(t, e.Result)
	require.Equal(t, "steered answer", e.Result.KeyFindings)
	require.Empty(t, e.Result.SteerReplies, "a folded steer is part of the work turn and records no reply")
	require.Equal(t, 2, model.stepsCount(), "the folded steer must not start a follow-up turn")
}

// consumedRecorder collects the verdicts a queue fire site delivers
// through a call's OnConsumed (#351), so a test can assert exactly-once
// delivery after the run has settled.
type consumedRecorder struct {
	mu   sync.Mutex
	seen []bool
}

func (r *consumedRecorder) record(ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, ok)
}

func (r *consumedRecorder) verdicts() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.seen...)
}

// TestOnConsumedTrueOnFold covers the fold fire site: a queued call
// consumed by the active turn's next step reports consumed exactly once,
// with true (#351).
func TestOnConsumedTrueOnFold(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &twoStepEchoModel{}
	echo, echoState := newGatedEchoTool()
	sa := newInjectionSessionAgent(env, model, []fantasy.AgentTool{echo})

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "task", NonInteractive: true})
		runDone <- runErr
	}()
	select {
	case <-echoState.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run never reached the tool call")
	}

	var rec consumedRecorder
	require.True(t, sa.EnqueueWhenBusy(SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "fold me in",
		OnConsumed: func(ok bool) {
			rec.record(ok)
		},
	}))

	close(echoState.gate)
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after the gate was released")
	}

	require.Equal(t, []bool{true}, rec.verdicts(),
		"a folded call must report consumed exactly once, with true")
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
}

// TestOnConsumedFalseOnCancelDrop covers the drop fire sites: a queued
// call removed from the queue by a Cancel — via clearQueueAndNotify or
// the canceled drain, whichever wins the lock — reports consumed exactly
// once, with false (#351).
func TestOnConsumedFalseOnCancelDrop(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &twoStepEchoModel{}
	echo, echoState := newGatedEchoTool()
	sa := newInjectionSessionAgent(env, model, []fantasy.AgentTool{echo})

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "task", NonInteractive: true})
		runDone <- runErr
	}()
	select {
	case <-echoState.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run never reached the tool call")
	}

	var rec consumedRecorder
	require.True(t, sa.EnqueueWhenBusy(SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "never delivered",
		OnConsumed: func(ok bool) {
			rec.record(ok)
		},
	}))

	// The cancel covers both the active turn and the queued call: the
	// queue is cleared and the call's waiter learns the drop.
	sa.Cancel(sess.ID)
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish after the cancel")
	}

	require.Equal(t, []bool{false}, rec.verdicts(),
		"a queue-dropped call must report consumed exactly once, with false")
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
}

// TestOnConsumedTrueOnDequeuedTurn covers the handoff fire site: a
// RunID-bearing queued call never folds (drainQueueForStep keeps it), so
// when the active turn ends the run hands it off as its own turn —
// reporting consumed exactly once, with true, before the recursive run
// starts (#351).
func TestOnConsumedTrueOnDequeuedTurn(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := &twoStepEchoModel{}
	echo, echoState := newGatedEchoTool()
	sa := newInjectionSessionAgent(env, model, []fantasy.AgentTool{echo})

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "task", NonInteractive: true})
		runDone <- runErr
	}()
	select {
	case <-echoState.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run never reached the tool call")
	}

	var rec consumedRecorder
	require.True(t, sa.EnqueueWhenBusy(SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "run as my own turn",
		RunID:     "run-1",
		OnConsumed: func(ok bool) {
			rec.record(ok)
		},
	}))

	close(echoState.gate)
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("run (including the dequeued follow-up turn) did not finish")
	}

	require.Equal(t, []bool{true}, rec.verdicts(),
		"a dequeued call must report consumed exactly once, with true, at the handoff")
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID))
	require.Equal(t, 4, model.stepsCount(),
		"the work turn and the RunID call's own turn each serve two steps")
}
