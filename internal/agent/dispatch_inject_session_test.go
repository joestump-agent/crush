package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
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
