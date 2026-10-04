package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// runUniqueDispatchToolCall invokes the DispatchAgent tool like
// runDispatchToolCall but with unique message and tool-call IDs, so
// several dispatches can run in one environment (the task session ID is
// messageID$$toolCallID and must not repeat).
func runUniqueDispatchToolCall(t *testing.T, tool fantasy.AgentTool, params DispatchAgentParams, seq int) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "dispatch-parent-session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, fmt.Sprintf("dispatch-parent-message-%d", seq))
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    fmt.Sprintf("dispatch-tool-call-%d", seq),
		Name:  DispatchAgentToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

// The dispatch tool assigns handles (#313): an explicit one, one derived
// from the role, the default fallback — and collisions across concurrent
// dispatches get numeric suffixes. The running handle carries the handle
// so the model can address the agent from the moment the tool returns.
func TestDispatchToolAssignsHandles(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	first := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "@Tester",
		Role:   "writes tests",
	}, 1))
	require.Equal(t, "tester", first.Handle)

	second := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the other bug",
		Branch: "main",
		Handle: "tester",
	}, 2))
	require.Equal(t, "tester-2", second.Handle)

	third := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "document it",
		Branch: "main",
		Role:   "Docs Writer",
	}, 3))
	require.Equal(t, "docs-writer", third.Handle)

	fourth := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "do a thing",
		Branch: "main",
	}, 4))
	require.Equal(t, "agent", fourth.Handle)

	// The registry entries carry handle and role, and resolve by handle.
	snap, ok := c.DispatchByHandle("tester")
	require.True(t, ok)
	require.Equal(t, first.DispatchID, snap.Entry.ID)
	require.Equal(t, "writes tests", snap.Entry.Role)

	close(agent.gate)
}

// DispatchLive lists only non-terminal dispatches (#313): finished
// handles never appear in the completions, keeping the not-continuable
// rule honest.
func TestDispatchLiveExcludesFinished(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	first := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "a", Branch: "main", Handle: "tester"}, 5))
	agent.waitRunning(t)
	decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "b", Branch: "main", Handle: "docs"}, 6))

	live := c.DispatchLive()
	handles := make(map[string]bool, len(live))
	for _, snap := range live {
		handles[snap.Entry.Handle] = true
	}
	require.True(t, handles["tester"], "a running dispatch must be live")
	require.True(t, handles["docs"])

	// Finish the first dispatch; its handle leaves the live set but still
	// resolves by handle (finished dispatches keep their handle).
	close(agent.gate)
	require.Eventually(t, func() bool {
		ws, _ := c.dispatchWorkspace()
		entry, ok := ws.Get(first.DispatchID)
		return ok && entry.Status.IsTerminal()
	}, 10*time.Second, 50*time.Millisecond)

	live = c.DispatchLive()
	for _, snap := range live {
		require.NotEqual(t, "tester", snap.Entry.Handle, "a finished dispatch must not be live")
	}
	snap, ok := c.DispatchByHandle("tester")
	require.True(t, ok, "a finished dispatch still resolves by handle")
	require.True(t, snap.Entry.Status.IsTerminal())
	require.Equal(t, first.SessionID, snap.Entry.SessionID)

	// DeliverAgentMessageByHandle refuses the finished handle cleanly.
	err := c.DeliverAgentMessageByHandle(t.Context(), "tester", "one more thing")
	require.ErrorContains(t, err, "finished")
	require.ErrorContains(t, err, "dispatch a new agent")
}

// DeliverAgentMessageByHandle routes to the running agent behind the
// handle (#313's editor front door over the #312 seam), and the message
// tool accepts the handle as the address.
func TestDeliverByHandleRoutesAndToolAcceptsHandle(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "tester",
	}, 7))
	agent.waitRunning(t)

	require.NoError(t, c.DeliverAgentMessageByHandle(t.Context(), "tester", "stop writing Rust"))
	require.NoError(t, c.DeliverAgentMessageByHandle(t.Context(), "@Tester", "normalized too"))
	injected := agent.injected()
	require.Len(t, injected, 2)
	require.Equal(t, "stop writing Rust", injected[0].Prompt)
	require.Equal(t, "normalized too", injected[1].Prompt)
	require.Equal(t, handle.SessionID, injected[0].SessionID)

	// Unknown handle refuses.
	err := c.DeliverAgentMessageByHandle(t.Context(), "ghost", "hello")
	require.ErrorContains(t, err, "no agent with handle @ghost")

	// The model-facing tool addresses by handle as well, with the "@"
	// tolerated.
	resp := runTool(t, c.messageAgentTool(), MessageAgentToolName, MessageAgentParams{
		Handle:  "@tester",
		Message: "add tests please",
	})
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	require.Contains(t, resp.Content, "@tester")
	require.Len(t, agent.injected(), 3)

	close(agent.gate)
}
