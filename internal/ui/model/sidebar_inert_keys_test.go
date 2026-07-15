package model

import (
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// ctrl builds a KeyPressMsg for ctrl+<r>. Note: a Text field would override the
// derived key string (e.g. "p" instead of "ctrl+p") and silently break matching
// — so it is intentionally omitted.
func ctrl(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

// TestSidebarSuppressesGlobalKeys pins the invariant that the commands (ctrl+p),
// models (ctrl+m/ctrl+l), and help (ctrl+g) actions are inert while the sidebar
// is focused — matching the sidebar help, which hides them (see #114 /
// sidebar_help_test.go). Today the sidebar focus case does not route to the
// global key handler at all; handleGlobalKeys also gates these keys for sidebar
// focus. This test guards both: if a future change (e.g. the interactive
// secondary-agent sidebar) starts routing sidebar keys through the global
// handler, this fails unless commands/models/help remain suppressed.
func TestSidebarSuppressesGlobalKeys(t *testing.T) {
	t.Parallel()

	// Sanity: confirm these key events really are the global bindings, so the
	// assertions below can't pass vacuously if a binding changes.
	km := DefaultKeyMap()
	require.True(t, key.Matches(ctrl('p'), km.Commands), "ctrl+p must map to Commands")
	require.True(t, key.Matches(ctrl('m'), km.Models), "ctrl+m must map to Models")
	require.True(t, key.Matches(ctrl('l'), km.Models), "ctrl+l must map to Models")
	require.True(t, key.Matches(ctrl('g'), km.Help), "ctrl+g must map to Help")

	newUI := func() *UI {
		m := newSidebarTestUI()
		// ToggleHelp() derefs m.status; give it a real one so an un-suppressed
		// Help key would toggle (and be caught) rather than nil-panic.
		m.status = NewStatus(m.com, m)
		m.focus = uiFocusSidebar
		return m
	}

	t.Run("commands key does not open the commands dialog", func(t *testing.T) {
		t.Parallel()
		m := newUI()
		m.handleKeyPressMsg(ctrl('p'))
		require.False(t, m.dialog.ContainsDialog(dialog.CommandsID),
			"ctrl+p must be inert while the sidebar is focused")
	})

	t.Run("models keys do not open the models dialog", func(t *testing.T) {
		t.Parallel()
		m := newUI()
		m.handleKeyPressMsg(ctrl('m'))
		m.handleKeyPressMsg(ctrl('l'))
		require.False(t, m.dialog.ContainsDialog(dialog.ModelsID),
			"ctrl+m / ctrl+l must be inert while the sidebar is focused")
	})

	t.Run("help key does not toggle help", func(t *testing.T) {
		t.Parallel()
		m := newUI()
		require.False(t, m.status.ShowingAll())
		m.handleKeyPressMsg(ctrl('g'))
		require.False(t, m.status.ShowingAll(),
			"ctrl+g must be inert while the sidebar is focused")
	})
}
