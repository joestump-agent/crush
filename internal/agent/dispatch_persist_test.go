package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// runDispatchToolCallInSession is runDispatchToolCall against a caller
// supplied parent session, so the test can pre-persist the parent turn's
// tool result the way the real agent loop does.
func runDispatchToolCallInSession(t *testing.T, tool fantasy.AgentTool, sessionID string, params any) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, sessionID)
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

// createParentToolResult persists the parent turn's dispatch_agent tool
// result — the running handle the model saw — and returns the message ID.
func createParentToolResult(t *testing.T, env fakeEnv, parentSessionID, handleJSON string) string {
	t.Helper()
	msg, err := env.messages.Create(t.Context(), parentSessionID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{message.ToolResult{
			ToolCallID: "dispatch-tool-call",
			Name:       DispatchAgentToolName,
			Content:    handleJSON,
		}},
	})
	require.NoError(t, err)
	return msg.ID
}

// persistedTerminalMetadata reads the parent's persisted tool result and
// reports its Metadata, or "" when the stamp has not landed yet.
func persistedTerminalMetadata(t *testing.T, env fakeEnv, msgID, toolCallID string) string {
	t.Helper()
	msg, err := env.messages.Get(t.Context(), msgID)
	require.NoError(t, err)
	for _, part := range msg.Parts {
		if tr, ok := part.(message.ToolResult); ok && tr.ToolCallID == toolCallID {
			return tr.Metadata
		}
	}
	return ""
}

// After a dispatch run completes, the parent session's persisted
// dispatch_agent tool result carries the terminal DispatchResult in
// Metadata (#410) while Content stays byte-identical to the running
// handle the model already consumed: the durable record lives beside the
// model output, never inside it.
func TestRunDispatchStampsTerminalMetadataOnParentToolResult(t *testing.T) {
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
	}
	c, env := newDispatchToolEnv(t, agent)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	tool := c.dispatchTool()

	resp := runDispatchToolCallInSession(t, tool, parent.ID, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"})
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	handleJSON := resp.Content

	msgID := createParentToolResult(t, env, parent.ID, handleJSON)

	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(decodeDispatchID(t, handleJSON))
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool {
		return persistedTerminalMetadata(t, env, msgID, "dispatch-tool-call") != ""
	}, 10*time.Second, 100*time.Millisecond, "terminal Metadata never stamped on the parent tool result")

	var terminal dispatch.DispatchResult
	require.NoError(t, json.Unmarshal([]byte(persistedTerminalMetadata(t, env, msgID, "dispatch-tool-call")), &terminal))
	require.Equal(t, dispatch.StatusCompleted, terminal.Status)
	require.Equal(t, decodeDispatchID(t, handleJSON), terminal.DispatchID)

	// The model-facing Content is untouched.
	msg, err := env.messages.Get(t.Context(), msgID)
	require.NoError(t, err)
	for _, part := range msg.Parts {
		if tr, ok := part.(message.ToolResult); ok && tr.ToolCallID == "dispatch-tool-call" {
			require.Equal(t, handleJSON, tr.Content)
		}
	}
}

// decodeDispatchID parses a running-handle tool response back to its
// dispatch ID.
func decodeDispatchID(t *testing.T, handleJSON string) string {
	t.Helper()
	var handle dispatch.DispatchResult
	require.NoError(t, json.Unmarshal([]byte(handleJSON), &handle))
	return handle.DispatchID
}

// The run can end before the parent turn has persisted its tool result —
// the run is launched before dispatchTool even returns. The stamp waits,
// bounded, and lands once the result exists (#410).
func TestRunDispatchTerminalMetadataWaitsForLateToolResult(t *testing.T) {
	gated := newGatedDispatchAgent()
	c, env := newInjectionEnv(t, gated)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	tool := c.dispatchTool()

	resp := runDispatchToolCallInSession(t, tool, parent.ID, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"})
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	handleJSON := resp.Content
	gated.waitRunning(t)

	// The run finishes before the parent's tool result is persisted:
	// close the gate, wait for the terminal registry status, and only
	// then write the tool result the parent turn would have written.
	close(gated.gate)
	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(decodeDispatchID(t, handleJSON))
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	msgID := createParentToolResult(t, env, parent.ID, handleJSON)

	require.Eventually(t, func() bool {
		return persistedTerminalMetadata(t, env, msgID, "dispatch-tool-call") != ""
	}, 10*time.Second, 100*time.Millisecond, "the stamp never landed on the late-persisted tool result")
}

// The initial dispatch run's call is not a steer; only DeliverAgentMessage
// injections are (#410), so the rebuilt card can tell the prompt apart
// without text matching.
func TestDispatchRunCallIsNotMarkedAsSteer(t *testing.T) {
	agent := &dispatchTestAgent{
		model:  dispatchTestModel(),
		result: &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
	}
	c, _ := newDispatchToolEnv(t, agent)
	tool := c.dispatchTool()

	resp := runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"})
	handle := decodeDispatchHandle(t, resp)

	// The run executes in the background; wait for it to finish before
	// inspecting the recorded call.
	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	require.Len(t, agent.calls, 1)
	require.False(t, agent.calls[0].Steer)
}

// createUserMessage persists the Steer mark on the child session's user
// message, so a rebuilt dispatch card recovers its steer log from the
// transcript (#410).
func TestCreateUserMessagePersistsSteerMark(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "child")
	require.NoError(t, err)

	msg, err := agent.createUserMessage(ctx, SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "stop writing Rust and use Go",
		Steer:     true,
	})
	require.NoError(t, err)
	require.True(t, msg.Content().Steer)

	reloaded, err := env.messages.Get(ctx, msg.ID)
	require.NoError(t, err)
	require.True(t, reloaded.Content().Steer, "Steer mark must survive the DB round-trip")

	plain, err := agent.createUserMessage(ctx, SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "fix the bug",
	})
	require.NoError(t, err)
	require.False(t, plain.Content().Steer)
}

// Todo nudges are plain user messages: they read as harness chatter on
// the transcript and must never surface as steers on the dispatch card
// (#410).
func TestTodoNudgeMessageIsNotMarkedAsSteer(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "child")
	require.NoError(t, err)

	nudge, err := agent.createTodoNudgeMessage(ctx, sess.ID, "you have unfinished todos")
	require.NoError(t, err)

	reloaded, err := env.messages.Get(ctx, nudge.ID)
	require.NoError(t, err)
	require.False(t, reloaded.Content().Steer)
}
