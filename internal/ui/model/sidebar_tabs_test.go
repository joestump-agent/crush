package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// newSidekickTestUI builds a non-compact uiChat model with a session, sized so
// the sidebar is visible, for exercising the Ctrl+A jump and tab cycling
// through the real key handler.
func newSidekickTestUI() *UI {
	m := newTestUI() // 140x45 is a non-compact chat layout
	m.dialog = dialog.NewOverlay()
	m.keyMap = DefaultKeyMap()
	m.attachments = attachments.New(nil, attachments.Keymap{})
	m.session = &session.Session{ID: "s1", Title: "Test Session"}
	m.updateLayoutAndSize()
	return m
}

func TestCycleSidebarTabWraps(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()
	require.Equal(t, sidebarTabInfo, m.sidebarTab)

	m.cycleSidebarTab()
	require.Equal(t, sidebarTabSidekick, m.sidebarTab)

	m.cycleSidebarTab()
	require.Equal(t, sidebarTabInfo, m.sidebarTab, "cycle should wrap back to Info")
}

// TestSwitchingTabsResetsScroll verifies the sidebar scroll offset resets when
// the active tab changes — the tabs have independent content heights.
func TestSwitchingTabsResetsScroll(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()
	m.sidebarScroll = 7

	m.setSidebarTab(sidebarTabSidekick)
	require.Equal(t, 0, m.sidebarScroll)

	// Re-activating the already-active tab keeps the scroll.
	m.sidebarScroll = 3
	m.setSidebarTab(sidebarTabSidekick)
	require.Equal(t, 3, m.sidebarScroll)
}

// TestCtrlAJumpsToSidekick exercises the real handleKeyPressMsg to verify the
// Ctrl+A binding jumps straight to the Sidekick tab from every focus state.
func TestCtrlAJumpsToSidekick(t *testing.T) {
	t.Parallel()

	for name, setFocus := range map[string]func(m *UI){
		"editor":  func(m *UI) { m.focus = uiFocusEditor },
		"main":    func(m *UI) { m.focus = uiFocusMain },
		"sidebar": func(m *UI) { m.focusSidebar() },
	} {
		t.Run("from "+name, func(t *testing.T) {
			t.Parallel()
			m := newSidekickTestUI()
			setFocus(m)

			m.handleKeyPressMsg(ctrl('a'))
			require.Equal(t, uiFocusSidebar, m.focus, "ctrl+a must focus the sidebar")
			require.Equal(t, sidebarTabSidekick, m.sidebarTab, "ctrl+a must activate the Sidekick tab")
		})
	}
}

// TestCtrlAClearsUnread verifies that jumping to the Sidekick tab counts as
// viewing it, clearing the unread badge.
func TestCtrlAClearsUnread(t *testing.T) {
	t.Parallel()
	m := newSidekickTestUI()
	m.sidekickUnread = 4

	m.handleKeyPressMsg(ctrl('a'))
	require.Equal(t, sidebarTabSidekick, m.sidebarTab)
	require.Equal(t, 0, m.sidekickUnread, "viewing the Sidekick tab must clear the badge")
}

// TestCtrlAIgnoredWhenSidebarHidden verifies Ctrl+A is inert when the sidebar
// is not drawn: compact mode, or no session.
func TestCtrlAIgnoredWhenSidebarHidden(t *testing.T) {
	t.Parallel()

	t.Run("compact mode", func(t *testing.T) {
		t.Parallel()
		m := newSidekickTestUI()
		m.forceCompactMode = true
		m.updateLayoutAndSize()
		require.True(t, m.isCompact)

		m.handleKeyPressMsg(ctrl('a'))
		require.Equal(t, uiFocusEditor, m.focus, "ctrl+a must not focus a hidden sidebar")
	})

	t.Run("no session", func(t *testing.T) {
		t.Parallel()
		m := newSidekickTestUI()
		m.session = nil

		m.handleKeyPressMsg(ctrl('a'))
		require.Equal(t, uiFocusEditor, m.focus, "ctrl+a must be inert without a session")
	})
}

// TestBumpSidekickUnread verifies the badge only accumulates while the
// Sidekick tab is not in view: content that arrives on-screen counts as seen.
func TestBumpSidekickUnread(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI() // uiChat, non-compact

	m.bumpSidekickUnread()
	m.bumpSidekickUnread()
	require.Equal(t, 2, m.sidekickUnread, "badge accumulates while the Info tab is active")

	m.setSidebarTab(sidebarTabSidekick)
	require.Equal(t, 0, m.sidekickUnread, "viewing the tab clears the badge")

	m.bumpSidekickUnread()
	require.Equal(t, 0, m.sidekickUnread, "content arriving on the visible Sidekick tab is already seen")

	// Hidden sidebar (compact mode) accumulates even with the tab active.
	m.isCompact = true
	m.bumpSidekickUnread()
	require.Equal(t, 1, m.sidekickUnread)
}

// TestSidebarTabBarRendering verifies the tab bar shows both tab titles and
// the unread badge only when there is unseen Sidekick content.
func TestSidebarTabBarRendering(t *testing.T) {
	t.Parallel()
	m := newSidebarTestUI()

	out := ansi.Strip(m.renderSidebarTabBar(40))
	require.Contains(t, out, "[Info]")
	require.Contains(t, out, "[Sidekick]")
	require.NotContains(t, out, "●", "no badge without unread content")

	m.sidekickUnread = 2
	out = ansi.Strip(m.renderSidebarTabBar(40))
	require.Contains(t, out, "● 2", "badge must show the unread count")
}

// TestDrawSidebarInfoTabShowsTabBar verifies the Info tab still renders its
// original content (pinned by TestSidebarAllSectionTitlesVisibleAtTightHeight)
// with the tab bar above it.
func TestDrawSidebarInfoTabShowsTabBar(t *testing.T) {
	t.Parallel()
	m := newSidebarHeightTestUI(t)

	const width, height = 32, 40
	m.layout.sidebar = uv.Rect(0, 0, width, height)
	scr := uv.NewScreenBuffer(width, height)
	m.drawSidebar(scr, m.layout.sidebar)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "[Info]")
	require.Contains(t, out, "[Sidekick]")
	require.Contains(t, out, "Modified Files", "Info tab content must be unchanged")
}

// TestDrawSidebarSidekickTab verifies the Sidekick tab renders the tab bar
// plus the shell placeholder, and that drawing it clears the unread badge.
func TestDrawSidebarSidekickTab(t *testing.T) {
	t.Parallel()
	m := newSidebarHeightTestUI(t)
	m.sidebarTab = sidebarTabSidekick
	m.sidekickUnread = 3

	const width, height = 32, 40
	m.layout.sidebar = uv.Rect(0, 0, width, height)
	scr := uv.NewScreenBuffer(width, height)
	m.drawSidebar(scr, m.layout.sidebar)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "[Sidekick]")
	require.Contains(t, out, "Nothing here yet.")
	require.NotContains(t, out, "Modified Files", "Info content must not bleed into the Sidekick tab")
	require.Equal(t, 0, m.sidekickUnread, "drawing the Sidekick tab counts as viewing it")
}
