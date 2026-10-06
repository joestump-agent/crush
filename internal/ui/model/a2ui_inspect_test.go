package model

// Tests for #407: A2UI surfaces in an inspected transcript are read-only.
// A form the viewed child authored must not take focus, must not retire on
// click, must not start a turn on the parent, and must not round-trip to an
// MCP server — and the parent's own forms must be interactive again after
// leaving inspect mode.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/joestump-agent/a2tea/event"
)

// a2uiFormMessage builds an assistant message carrying the live submit form
// (a2uiSubmitForm) under the given ID and session.
func a2uiFormMessage(id, sessionID string) message.Message {
	return message.Message{
		ID:        id,
		SessionID: sessionID,
		Role:      message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Fill this in:\n\n" + a2uiSubmitForm},
		},
	}
}

// childA2UITranscript is a small child transcript ending in an assistant
// message that carries the live submit form.
func childA2UITranscript() []message.Message {
	return []message.Message{
		{ID: "c1", SessionID: inspectChildID, Role: message.User},
		a2uiFormMessage("c2", inspectChildID),
	}
}

// renderChildFormFocus renders the chat list with the form item selected:
// the list's render callback is the only path that grants item focus, so
// this is exactly what would hand the form's surface focus in a live UI.
// It reports whether the surface consumes enter afterward (true when it
// took focus, false when it stayed blurred).
func renderChildFormFocus(t *testing.T, m *UI, itemID string) bool {
	t.Helper()
	item, ok := m.chat.MessageItem(itemID).(*chat.AssistantMessageItem)
	require.True(t, ok)
	_ = item.RawRender(80)
	m.chat.SetSize(80, 20)
	m.chat.Focus()
	m.chat.SelectLast()
	_ = m.chat.list.Render()
	handled, _ := item.HandleKeyEvent(tea.KeyPressMsg{Code: tea.KeyEnter})
	return handled
}

// clickA2UIButton sends a ButtonClicked event for the form's submit button
// through the real Update routing.
func clickA2UIButton(m *UI) []tea.Msg {
	_, cmd := m.Update(event.ButtonClicked{
		Source: event.Source{ComponentID: "btn-send", SurfaceID: "form"},
		ID:     "btn-send",
	})
	return runCmdTree(cmd)
}

// infoMsgs filters a message batch down to the info messages in it.
func infoMsgs(msgs []tea.Msg) []util.InfoMsg {
	var infos []util.InfoMsg
	for _, msg := range msgs {
		if info, ok := msg.(util.InfoMsg); ok {
			infos = append(infos, info)
		}
	}
	return infos
}

// TestA2UIFormReadOnlyWhileInspecting pins the inspect guard (#407): while
// a child transcript is on screen, a surface in it takes no focus, and
// clicking it starts no agent run, calls no MCP tool, and leaves the
// surface unretired.
func TestA2UIFormReadOnlyWhileInspecting(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", childA2UITranscript()...)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting(), "inspect mode must be active")

	// The selected child's surface must not take focus even when the list
	// renders and grants item focus.
	require.False(t, renderChildFormFocus(t, m, "c2"),
		"no surface in an inspected transcript may hold focus")

	msgs := clickA2UIButton(m)
	require.Empty(t, ws.agentRuns,
		"a click in an inspected transcript must not start a turn")

	// The click is answered with the read-only notice.
	infos := infoMsgs(msgs)
	require.Len(t, infos, 1, "the click must be answered with the read-only notice, got %#v", msgs)
	require.Equal(t, "This form belongs to a sub-agent transcript and is read-only", infos[0].Msg)

	// The surface was left live.
	_, live := m.chat.RetireA2UISurface("form")
	require.True(t, live, "the surface must be left unretired")
}

// TestA2UIFormReadOnlyForLiveChildMessages pins the live-append half of
// #407: a message that lands in the child transcript mid-inspect inherits
// the read-only state — its surface takes no focus.
func TestA2UIFormReadOnlyForLiveChildMessages(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent",
		message.Message{ID: "c1", SessionID: inspectChildID, Role: message.User})

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	// A live child message carrying a form lands mid-inspect.
	m.Update(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: a2uiFormMessage("c2", inspectChildID),
	})

	require.False(t, renderChildFormFocus(t, m, "c2"),
		"a live child surface must not hold focus while inspecting")
}

// TestA2UIFormInteractiveAfterInspectExit pins the other side of #407:
// after returning from inspect mode, the parent's own A2UI forms are
// interactive again — submitting one starts a parent turn as before.
func TestA2UIFormInteractiveAfterInspectExit(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent",
		message.Message{ID: "c1", SessionID: inspectChildID, Role: message.User})
	ws.messages[inspectParentID] = []message.Message{
		a2uiFormMessage("p1", inspectParentID),
	}

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	// Leave inspect: the parent transcript, with its own live form, comes
	// back.
	runInspectCmds(m, m.exitInspect())
	require.False(t, m.isInspecting())

	// The parent's form is interactive again: the selected item's surface
	// takes focus when the list renders.
	require.True(t, renderChildFormFocus(t, m, "p1"),
		"the parent's own surface must be focusable after inspect")

	clickA2UIButton(m)

	require.Equal(t, []string{inspectParentID}, ws.agentRuns,
		"the parent's own form must start a parent turn after inspect")
	_, retired := m.chat.RetireA2UISurface("form")
	require.False(t, retired, "the parent's submitted surface must be retired")
}
