package agent

import (
	"context"
	"runtime"
	"testing"

	"charm.land/fantasy"
	"charm.land/x/vcr"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// toolErrorModel streams a single error part carrying a tool's own failure,
// which is the shape a dead URL or a failing command produces.
type toolErrorModel struct {
	fakeLanguageModel
}

func (m *toolErrorModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	toolErr := &fantasy.ToolExecutionError{
		ToolName:   "fetch",
		ToolCallID: "call_1",
		Err:        context.DeadlineExceeded,
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: toolErr})
	}, nil
}

// TestToolErrorRunFinishMessage pins that a tool's own failure is not reported
// as a provider error. Consumers watch the finish title: a monitoring hook that
// pages on provider errors fired for a refused connection because the fallback
// label said "Provider Error" for anything it did not recognise.
func TestToolErrorRunFinishMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows for now")
	}

	env := testEnv(t)
	model := &toolErrorModel{}
	agent, err := coderAgent(vcr.NewRecorder(t), env, model, model)
	require.NoError(t, err)

	session, err := env.sessions.Create(t.Context(), "tool error session")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "Hello",
		SessionID:       session.ID,
		MaxOutputTokens: 10000,
	})
	require.Error(t, err)

	msgs, err := env.messages.List(t.Context(), session.ID)
	require.NoError(t, err)

	var finish *message.Finish
	for _, msg := range msgs {
		if msg.Role != message.Assistant {
			continue
		}
		if part := msg.FinishPart(); part != nil {
			finish = part
		}
	}
	require.NotNil(t, finish, "the assistant message should carry a finish part")
	require.Equal(t, message.FinishReasonError, finish.Reason)
	require.Equal(t, "Tool Error", finish.Message)
	require.Contains(t, finish.Details, `tool "fetch" failed`)
	require.NotContains(t, finish.Message, "Provider")
}
