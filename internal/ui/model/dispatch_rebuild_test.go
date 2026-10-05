package model

// Tests for dispatch card persistence (#410): steers survive the
// inspect round-trip because the rebuild path reconstructs them from the
// persisted child transcript, and a Tool-role update carrying the
// stamped terminal record flips the card terminal on live events.

import (
	"encoding/json"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/ui/chat"
)

// addDispatchBlock appends an assistant message carrying a
// dispatch_agent tool call into the parent transcript through the live
// event path — the dispatch flavor of addAgentBlock — and mirrors it
// into the stub workspace, so an inspect round-trip's parent reload
// rebuilds the same block.
func addDispatchBlock(t *testing.T, m *UI, ws *inspectWorkspace, messageID, toolCallID string) {
	t.Helper()
	msg := message.Message{
		ID:        messageID,
		SessionID: inspectParentID,
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       toolCallID,
				Name:     agent.DispatchAgentToolName,
				Input:    `{"prompt":"implement the login form with validation"}`,
				Finished: true,
			},
		},
	}
	ws.messages[inspectParentID] = append(ws.messages[inspectParentID], msg)
	m.Update(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: msg})
}

// dispatchBlock returns the parent transcript's dispatch block, or nil.
func dispatchBlock(m *UI, toolCallID string) *chat.DispatchToolMessageItem {
	item := m.chat.MessageItem(toolCallID)
	if item == nil {
		return nil
	}
	block, _ := item.(*chat.DispatchToolMessageItem)
	return block
}

// A steer recorded on a dispatch block survives leaving inspect mode:
// the parent reload rebuilds the steer log from the persisted child
// transcript (#410), so the injected message and its answer are still on
// the card — without depending on the live event path having run.
func TestInspectRoundTripRebuildsDispatchSteers(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)

	// The child transcript carries the dispatch prompt (unmarked), one
	// Steer-marked injection, and the agent's reply.
	steer := message.Message{
		ID: "c-steer", SessionID: inspectChildID, Role: message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "stop writing Rust and use Go", Steer: true}},
	}
	reply := message.Message{
		ID: "c-reply", SessionID: inspectChildID, Role: message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "switched to Go"}},
	}
	prompt := message.Message{
		ID: "c-prompt", SessionID: inspectChildID, Role: message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "implement the login form with validation"}},
	}
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", prompt, steer, reply)

	addDispatchBlock(t, m, ws, inspectMessageID, inspectCallID)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	_, backCmd := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	require.True(t, backCmd != nil)
	runInspectCmds(m, backCmd)
	require.False(t, m.isInspecting())

	block := dispatchBlock(m, inspectCallID)
	require.NotNil(t, block, "the dispatch block must be back in the rebuilt parent transcript")
	steers := block.Steers()
	require.Len(t, steers, 1, "exactly the Steer-marked message is a steer")
	require.Equal(t, "stop writing Rust and use Go", steers[0].Text)
	require.Equal(t, "c-reply", steers[0].ResponseMessageID)
	require.Equal(t, "switched to Go", steers[0].Response)
}

// A Tool-role update carrying the stamped terminal record (#410) flips a
// live dispatch card terminal: without applying results on update, a
// client/server card would spin forever on a finished dispatch.
func TestUpdatedToolEventFlipsDispatchCardTerminal(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addDispatchBlock(t, m, ws, inspectMessageID, inspectCallID)

	block := dispatchBlock(m, inspectCallID)
	require.NotNil(t, block)
	require.True(t, block.Spinning(), "the card spins while the tool call is open")

	// The dispatch tool returns: the parent turn persists a Tool-role
	// message holding the running handle.
	toolMsg := func(tr message.ToolResult) message.Message {
		return message.Message{
			ID:        "tool-msg-1",
			SessionID: inspectParentID,
			Role:      message.Tool,
			Parts:     []message.ContentPart{tr},
		}
	}
	handle := dispatch.DispatchResult{
		DispatchID: "dispatch-1",
		SessionID:  inspectChildID,
		Status:     dispatch.StatusRunning,
	}
	handleJSON, err := json.Marshal(handle)
	require.NoError(t, err)
	m.Update(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: toolMsg(message.ToolResult{ToolCallID: inspectCallID, Name: agent.DispatchAgentToolName, Content: string(handleJSON)}),
	})
	// The running handle alone is a static "working": without a live
	// snapshot nothing proves the dispatch is still going, so the card
	// never spins on it (#410's stale-handle rule).
	require.Contains(t, ansi.Strip(block.Render(100)), "working")

	// Later — the run completes and the coordinator stamps the terminal
	// record into the persisted message's Metadata. The card learns of
	// it only through the Tool-role UpdatedEvent.
	terminal := dispatch.DispatchResult{
		DispatchID:  "dispatch-1",
		SessionID:   inspectChildID,
		Status:      dispatch.StatusCompleted,
		KeyFindings: "Added validation and two tests.",
	}
	terminalJSON, err := json.Marshal(terminal)
	require.NoError(t, err)
	m.Update(pubsub.Event[message.Message]{
		Type: pubsub.UpdatedEvent,
		Payload: toolMsg(message.ToolResult{
			ToolCallID: inspectCallID,
			Name:       agent.DispatchAgentToolName,
			Content:    string(handleJSON),
			Metadata:   string(terminalJSON),
		}),
	})

	require.False(t, block.Spinning(), "the stamped terminal record must stop the card")

	// The terminal record renders: the findings show and "working" is
	// gone, exactly as a completed card renders in a restarted session.
	out := ansi.Strip(block.Render(100))
	require.Contains(t, out, "complete")
	require.Contains(t, out, "Added validation and two tests.")
	require.NotContains(t, out, "working")
}
