package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/stretchr/testify/require"
)

// TestShortHelpSidebarHidesCommandOptions verifies that when the sidebar is
// focused, ShortHelp does not include the commands (ctrl+p), models
// (ctrl+m/ctrl+l), or help/more (ctrl+g) bindings — those actions are not
// meaningful from the sidebar.
func TestShortHelpSidebarHidesCommandOptions(t *testing.T) {
	t.Parallel()

	// Helper: collect all help keys from ShortHelp bindings.
	collectKeys := func(m *UI) map[string]bool {
		out := make(map[string]bool)
		for _, b := range m.ShortHelp() {
			out[b.Help().Key] = true
		}
		return out
	}

	t.Run("sidebar focus hides command options", func(t *testing.T) {
		t.Parallel()
		m := newSidebarTestUI()
		m.focus = uiFocusSidebar
		keys := collectKeys(m)

		for _, key := range []string{"ctrl+l", "ctrl+m", "ctrl+g", "ctrl+p", "/ or ctrl+p"} {
			require.False(t, keys[key], "key %q should NOT appear in ShortHelp when sidebar is focused", key)
		}
	})

	t.Run("editor focus still shows command options", func(t *testing.T) {
		t.Parallel()
		m := newSidebarTestUI()
		m.focus = uiFocusEditor
		keys := collectKeys(m)

		require.True(t, keys["ctrl+l"] || keys["ctrl+m"], "models binding should appear for editor focus")
		require.True(t, keys["ctrl+g"], "help/more binding should appear for editor focus")
		require.True(t, keys["ctrl+p"] || keys["/ or ctrl+p"], "commands binding should appear for editor focus")
	})

	t.Run("main focus still shows command options", func(t *testing.T) {
		t.Parallel()
		m := newSidebarTestUI()
		m.focus = uiFocusMain
		keys := collectKeys(m)

		require.True(t, keys["ctrl+l"] || keys["ctrl+m"], "models binding should appear for main focus")
		require.True(t, keys["ctrl+g"], "help/more binding should appear for main focus")
		require.True(t, keys["ctrl+p"], "commands binding should appear for main focus")
	})
}

// TestFullHelpSidebarHidesCommandOptions verifies that when the sidebar is
// focused, FullHelp does not include the commands, models, or help/more
// bindings.
func TestFullHelpSidebarHidesCommandOptions(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()
	m.attachments = attachments.New(nil, attachments.Keymap{})
	m.focus = uiFocusSidebar
	binds := m.FullHelp()

	helpKeys := make(map[string]bool)
	for _, row := range binds {
		for _, b := range row {
			helpKeys[b.Help().Key] = true
		}
	}

	for _, key := range []string{"ctrl+p", "ctrl+l", "ctrl+m", "ctrl+g", "/ or ctrl+p"} {
		require.False(t, helpKeys[key], "key %q should NOT appear in FullHelp when sidebar is focused", key)
	}
}

// TestFullHelpSidebarHidesUnroutedKeys verifies that FullHelp under sidebar
// focus does not advertise sessions (ctrl+s), yolo (ctrl+y), or new session
// (ctrl+n) — the sidebar key handler never routes them, so they are dead keys
// while it has focus.
func TestFullHelpSidebarHidesUnroutedKeys(t *testing.T) {
	t.Parallel()

	collectKeys := func(m *UI) map[string]bool {
		out := make(map[string]bool)
		for _, row := range m.FullHelp() {
			for _, b := range row {
				out[b.Help().Key] = true
			}
		}
		return out
	}

	m := newSidebarTestUI()
	m.attachments = attachments.New(nil, attachments.Keymap{})
	// The editor-focus branch consults the model config and agent state, so
	// give the UI a workspace stub with an empty config.
	m.com.Workspace = &sidebarHeightTestWorkspace{cfg: &config.Config{}}
	m.session = &session.Session{ID: "s1"} // so NewSession would otherwise be listed
	m.focus = uiFocusSidebar
	keys := collectKeys(m)
	for _, key := range []string{"ctrl+s", "ctrl+y", "ctrl+n"} {
		require.False(t, keys[key], "key %q should NOT appear in FullHelp when sidebar is focused", key)
	}

	// The same keys stay advertised for the editor, where they are routed.
	m.focus = uiFocusEditor
	keys = collectKeys(m)
	for _, key := range []string{"ctrl+s", "ctrl+y", "ctrl+n"} {
		require.True(t, keys[key], "key %q should appear in FullHelp when the editor is focused", key)
	}
}

// TestShortHelpSidebarOnlyRelevantOptions verifies that ShortHelp for sidebar
// focus contains only the expected set of help keys.
func TestShortHelpSidebarOnlyRelevantOptions(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()
	m.focus = uiFocusSidebar
	binds := m.ShortHelp()

	// Sidebar help should only show: tab, scroll (↑/↓), esc, and quit.
	expected := map[string]bool{
		"tab":    true,
		"↑/↓":    true,
		"esc":    true,
		"ctrl+c": true,
	}
	for _, b := range binds {
		key := b.Help().Key
		require.True(t, expected[key], "unexpected key %q in sidebar ShortHelp", key)
	}
	// Ensure we don't accidentally produce an empty list.
	require.NotEmpty(t, binds)
}

// TestFullHelpSidebarNoIrrelevantDescs does a description-based check to
// ensure that none of the FullHelp entries mention "commands", "models", or
// "more"/"less" when the sidebar is focused.
func TestFullHelpSidebarNoIrrelevantDescs(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()
	m.attachments = attachments.New(nil, attachments.Keymap{})
	m.focus = uiFocusSidebar
	binds := m.FullHelp()

	for _, row := range binds {
		for _, b := range row {
			desc := strings.ToLower(b.Help().Desc)
			require.False(t, desc == "commands", "commands binding should not appear in sidebar FullHelp")
			require.False(t, desc == "models", "models binding should not appear in sidebar FullHelp")
			require.False(t, desc == "more" || desc == "less", "help toggle should not appear in sidebar FullHelp")
		}
	}
}
