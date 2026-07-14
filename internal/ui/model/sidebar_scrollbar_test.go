package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShortHelpTabStability verifies that the Tab key binding description
// in ShortHelp remains stable and focus-neutral, rather than fluctuating,
// and doesn't pollute the global/base key map definitions when focus changes.
func TestShortHelpTabStability(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()

	// Capture initial tab help description
	m.focus = uiFocusEditor
	bindsEditor := m.ShortHelp()
	var editorTabHelp string
	for _, b := range bindsEditor {
		if b.Help().Key == "tab" {
			editorTabHelp = b.Help().Desc
			break
		}
	}

	// Change focus to Main
	m.focus = uiFocusMain
	bindsMain := m.ShortHelp()
	var mainTabHelp string
	for _, b := range bindsMain {
		if b.Help().Key == "tab" {
			mainTabHelp = b.Help().Desc
			break
		}
	}

	// Change focus to Sidebar
	m.focus = uiFocusSidebar
	bindsSidebar := m.ShortHelp()
	var sidebarTabHelp string
	for _, b := range bindsSidebar {
		if b.Help().Key == "tab" {
			sidebarTabHelp = b.Help().Desc
			break
		}
	}

	// Validate they are all stable and focus-neutral ("change focus")
	// rather than shifting to "focus chat", "focus sidebar", or "focus editor".
	require.Equal(t, "change focus", editorTabHelp)
	require.Equal(t, "change focus", mainTabHelp)
	require.Equal(t, "change focus", sidebarTabHelp)
}

// TestSidebarUpDownHelpNoStatePollution verifies that customizing UpDown help
// text during sidebar focus is scoped correctly and doesn't permanently pollute
// the base keymap configuration.
func TestSidebarUpDownHelpNoStatePollution(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()

	// First verify default state is preserved
	require.Equal(t, "scroll", m.keyMap.Chat.UpDown.Help().Desc)

	// Trigger ShortHelp when focused on sidebar (which updates the cloned UpDown)
	m.focus = uiFocusSidebar
	m.ShortHelp()

	// Ensure base keymap is unpolluted
	require.Equal(t, "scroll", m.keyMap.Chat.UpDown.Help().Desc)
}
