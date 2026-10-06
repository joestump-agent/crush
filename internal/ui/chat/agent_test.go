package chat

// Tests for the agent-tool block liveness accessor (#405): HasResult is
// what the inspect mode uses to tell a running agent from a finished
// one, because the raw status stays Running after the result lands.

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newAgentToolForLiveness builds a plain agent-tool block the way the
// live and history paths do, with or without a recorded result.
func newAgentToolForLiveness(result *message.ToolResult) *AgentToolMessageItem {
	sty := styles.CharmtonePantera()
	return NewAgentToolMessageItem(&sty, message.ToolCall{
		ID:    "call-agent-liveness",
		Name:  agent.AgentToolName,
		Input: `{"prompt":"do things"}`,
	}, result, false)
}

// TestAgentToolHasResult pins the accessor the inspect ring relies on:
// no result means the tool is not finished, and a result of either
// kind means it is (#405).
func TestAgentToolHasResult(t *testing.T) {
	t.Parallel()

	t.Run("no result is not finished", func(t *testing.T) {
		t.Parallel()
		item := newAgentToolForLiveness(nil)
		require.False(t, item.HasResult())
	})

	t.Run("result is finished", func(t *testing.T) {
		t.Parallel()
		item := newAgentToolForLiveness(&message.ToolResult{
			ToolCallID: "call-agent-liveness",
			Content:    "done",
		})
		require.True(t, item.HasResult())
	})

	t.Run("error result is finished", func(t *testing.T) {
		t.Parallel()
		item := newAgentToolForLiveness(&message.ToolResult{
			ToolCallID: "call-agent-liveness",
			Content:    "boom",
			IsError:    true,
		})
		require.True(t, item.HasResult())
	})

	t.Run("SetResult flips the answer", func(t *testing.T) {
		t.Parallel()
		item := newAgentToolForLiveness(nil)
		require.False(t, item.HasResult())
		item.SetResult(&message.ToolResult{ToolCallID: "call-agent-liveness", Content: "done"})
		require.True(t, item.HasResult())
	})
}
