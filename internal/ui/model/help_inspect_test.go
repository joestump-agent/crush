package model

// Tests for the inspect keybindings in the help views (#412): ShortHelp
// and FullHelp advertise ctrl+] when the selected transcript item is an
// agent block, lead with "esc back to chat" while inspecting, add
// "next agent" when the entry ring holds more than one live agent, and
// leave the sidebar help exactly as it was. Assertions are on the
// returned bindings' keys and help text, never on rendered strings.

import (
	"testing"

	"charm.land/bubbles/v2/key"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

// helpPairs flattens bindings to (key, desc) pairs.
func helpPairs(binds []key.Binding) [][2]string {
	out := make([][2]string, 0, len(binds))
	for _, b := range binds {
		h := b.Help()
		out = append(out, [2]string{h.Key, h.Desc})
	}
	return out
}

// hasHelpPair reports whether the pairs contain the given key/desc pair.
func hasHelpPair(pairs [][2]string, k, desc string) bool {
	for _, p := range pairs {
		if p[0] == k && p[1] == desc {
			return true
		}
	}
	return false
}

// fullHelpPairs flattens every FullHelp row to (key, desc) pairs.
func fullHelpPairs(m *UI) [][2]string {
	var out [][2]string
	for _, row := range m.FullHelp() {
		out = append(out, helpPairs(row)...)
	}
	return out
}

// addPlainMessage appends a user message with no tool parts, so the chat
// gains a non-agent item to select.
func addPlainMessage(t *testing.T, m *UI, id string) {
	t.Helper()
	m.Update(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        id,
			SessionID: inspectParentID,
			Role:      message.User,
		},
	})
}

// startInspecting activates inspect mode state directly, the way
// dispatch_cancel_test.go does, without running the enter transition.
func startInspecting(m *UI, ring ...string) {
	m.inspecting = &session.Session{ID: ring[0], ParentSessionID: inspectParentID}
	m.inspectRing = ring
	m.inspectRingPos = 0
}

func TestShortHelpInspectEntryHint(t *testing.T) {
	t.Parallel()

	selectLast := func(m *UI) { m.chat.SetSelected(m.chat.Len() - 1) }

	t.Run("agent block selected shows ctrl+]", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		selectLast(m)

		pairs := helpPairs(m.ShortHelp())
		require.True(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"an agent block selected on main focus must advertise ctrl+] inspect agent")
	})

	t.Run("dispatch block selected shows ctrl+]", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		addDispatchBlock(t, m, ws, inspectMessageID, inspectCallID)
		selectLast(m)

		pairs := helpPairs(m.ShortHelp())
		require.True(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"a dispatch block selected on main focus must advertise ctrl+] inspect agent")
	})

	t.Run("plain message selected hides ctrl+]", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		addPlainMessage(t, m, "plain-1")
		selectLast(m)

		pairs := helpPairs(m.ShortHelp())
		require.False(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"a plain message selected must not advertise ctrl+]")
	})

	t.Run("editor focus hides the entry hint", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		selectLast(m)

		pairs := helpPairs(m.ShortHelp())
		require.False(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"the entry hint is main-focus only")
	})

	t.Run("sidebar focus keeps its help", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusSidebar
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		selectLast(m)

		pairs := helpPairs(m.ShortHelp())
		require.False(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"sidebar focus help must not gain the entry hint")
	})
}

func TestShortHelpInspectingBackLeads(t *testing.T) {
	t.Parallel()

	t.Run("ring of one shows only back", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		startInspecting(m, inspectChildID)

		pairs := helpPairs(m.ShortHelp())
		require.Equal(t, [2]string{"esc", "back to chat"}, pairs[0],
			"esc back to chat must lead ShortHelp while inspecting")
		require.False(t, hasHelpPair(pairs, "ctrl+]", "next agent"),
			"a single-agent ring must not advertise next agent")
	})

	t.Run("ring of two adds next agent", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		startInspecting(m, inspectChildID, inspectChild2ID)

		pairs := helpPairs(m.ShortHelp())
		require.Equal(t, [2]string{"esc", "back to chat"}, pairs[0],
			"esc back to chat must still lead with a multi-agent ring")
		require.True(t, hasHelpPair(pairs, "ctrl+]", "next agent"),
			"a multi-agent ring must advertise ctrl+] next agent")
	})

	t.Run("sidebar focus keeps its help while inspecting", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusSidebar
		startInspecting(m, inspectChildID, inspectChild2ID)

		pairs := helpPairs(m.ShortHelp())
		require.False(t, hasHelpPair(pairs, "esc", "back to chat"),
			"sidebar focus help must not gain the inspect back binding")
		require.False(t, hasHelpPair(pairs, "ctrl+]", "next agent"),
			"sidebar focus help must not gain the next-agent binding")
	})
}

func TestFullHelpInspectGroup(t *testing.T) {
	t.Parallel()

	t.Run("agent block selected lists drill", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		m.chat.SetSelected(m.chat.Len() - 1)

		pairs := fullHelpPairs(m)
		require.True(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"FullHelp must list the drill binding for a selected agent block")
		require.False(t, hasHelpPair(pairs, "esc", "back to chat"),
			"FullHelp must not list the back binding outside inspect mode")
	})

	t.Run("plain message hides the group", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		addPlainMessage(t, m, "plain-1")
		m.chat.SetSelected(m.chat.Len() - 1)

		pairs := fullHelpPairs(m)
		require.False(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"FullHelp must not list the drill binding for a plain message")
	})

	t.Run("inspecting lists back then next agent", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusMain
		startInspecting(m, inspectChildID, inspectChild2ID)

		pairs := fullHelpPairs(m)
		require.True(t, hasHelpPair(pairs, "esc", "back to chat"),
			"FullHelp must list the back binding while inspecting")
		require.True(t, hasHelpPair(pairs, "ctrl+]", "next agent"),
			"FullHelp must list next agent for a multi-agent ring")
		require.False(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"the drill binding keeps its cycle label while inspecting")
	})

	t.Run("sidebar focus hides the group", func(t *testing.T) {
		t.Parallel()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.focus = uiFocusSidebar
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		m.chat.SetSelected(m.chat.Len() - 1)

		pairs := fullHelpPairs(m)
		require.False(t, hasHelpPair(pairs, "ctrl+]", "inspect agent"),
			"sidebar focus FullHelp must not gain the drill binding")
	})
}
