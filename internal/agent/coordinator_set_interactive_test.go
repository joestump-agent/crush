package agent

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// coderToolNames returns the coder agent's current tool palette as a
// name set, waiting for the agent's background build to settle first so
// the read cannot race buildAgent's async tool construction.
func coderToolNames(t *testing.T, c *coordinator) map[string]bool {
	t.Helper()
	c.agentMu.RLock()
	sa := c.agents[config.AgentCoder]
	c.agentMu.RUnlock()
	require.NoError(t, sa.WaitReady())
	concrete, ok := sa.(*sessionAgent)
	require.True(t, ok, "the gate test coordinator builds a real sessionAgent")
	names := make(map[string]bool)
	for _, tool := range concrete.tools.Copy() {
		names[tool.Info().Name] = true
	}
	return names
}

// TestSetInteractiveRebuildsPalette flips the coordinator between the
// interactive and non-interactive palettes in place (#420): the question
// and dispatch tools follow the flag, and a same-mode call is a no-op.
func TestSetInteractiveRebuildsPalette(t *testing.T) {
	c := newGateTestCoordinator(t, true)
	require.True(t, coderToolNames(t, c)[tools.QuestionToolName],
		"an interactive palette carries the question tool")
	require.True(t, coderToolNames(t, c)[DispatchAgentToolName],
		"an interactive palette carries the dispatch tool")

	ctx := context.Background()
	require.NoError(t, c.SetInteractive(ctx, false))
	require.False(t, c.isInteractive())
	names := coderToolNames(t, c)
	require.False(t, names[tools.QuestionToolName],
		"a non-interactive palette drops the question tool")
	require.False(t, names[DispatchAgentToolName],
		"a non-interactive palette drops the dispatch tool")
	require.False(t, names[MessageAgentToolName],
		"a non-interactive palette drops the message tool")
	require.False(t, names[CancelDispatchToolName],
		"a non-interactive palette drops the cancel tool")

	require.NoError(t, c.SetInteractive(ctx, true))
	require.True(t, c.isInteractive())
	names = coderToolNames(t, c)
	require.True(t, names[tools.QuestionToolName],
		"flipping back restores the question tool")
	require.True(t, names[DispatchAgentToolName],
		"flipping back restores the dispatch tool")

	require.NoError(t, c.SetInteractive(ctx, true),
		"a same-mode call is a no-op and must not error")
}

// TestSetInteractiveKeepsDispatchAddressable pins the acceptance
// criterion that motivated #420: a dispatch that was running before the
// mode flipped still resolves by handle and still takes injected
// messages afterwards — the registry and the injection targets belong
// to the coordinator, not to either palette.
func TestSetInteractiveKeepsDispatchAddressable(t *testing.T) {
	gated := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, gated)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "tester",
	}, 21))
	gated.waitRunning(t)

	require.NoError(t, c.SetInteractive(t.Context(), true))

	snap, ok := c.DispatchByHandle("dispatch-parent-session", "tester")
	require.True(t, ok, "the dispatch must still resolve after the mode flip")
	require.Equal(t, handle.DispatchID, snap.Entry.ID)

	require.NoError(t, c.DeliverAgentMessage(t.Context(), AgentMessage{
		SessionID: handle.SessionID,
		Text:      "still there?",
	}))
	injected := gated.injected()
	require.Len(t, injected, 1)
	require.Equal(t, "still there?", injected[0].Prompt)
	require.Equal(t, handle.SessionID, injected[0].SessionID)

	gated.release()
}
