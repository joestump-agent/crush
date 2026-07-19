package model

import (
	"fmt"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// sidebarTab identifies which tab of the sidebar tab bar is active.
type sidebarTab uint8

// Sidebar tabs, in tab-bar (and Tab-cycle) order.
const (
	sidebarTabInfo sidebarTab = iota
	sidebarTabSidekick

	sidebarTabCount // number of tabs; keep last
)

// title returns the tab's label as shown in the sidebar tab bar.
func (t sidebarTab) title() string {
	switch t {
	case sidebarTabSidekick:
		return "Sidekick"
	default:
		return "Info"
	}
}

// setSidebarTab activates the given sidebar tab. Switching tabs resets the
// sidebar scroll (the tabs have independent content heights), and viewing the
// Sidekick tab clears its unread badge.
func (m *UI) setSidebarTab(tab sidebarTab) {
	if tab != m.sidebarTab {
		m.sidebarScroll = 0
	}
	m.sidebarTab = tab
	if tab == sidebarTabSidekick {
		m.sidekickUnread = 0
	}
}

// cycleSidebarTab advances to the next sidebar tab, wrapping around. Bound to
// Tab while the sidebar is focused.
func (m *UI) cycleSidebarTab() {
	m.setSidebarTab((m.sidebarTab + 1) % sidebarTabCount)
}

// focusSidekick focuses the sidebar and jumps straight to the Sidekick tab.
// Bound globally to ctrl+a; only meaningful when the sidebar is visible
// (chat state, non-compact layout).
func (m *UI) focusSidekick() {
	m.focusSidebar()
	m.setSidebarTab(sidebarTabSidekick)
}

// sidekickTabInView reports whether the Sidekick tab is currently being
// displayed: chat state, non-compact layout (the sidebar is hidden in compact
// mode), and the Sidekick tab active.
func (m *UI) sidekickTabInView() bool {
	return m.state == uiChat && !m.isCompact && m.sidebarTab == sidebarTabSidekick
}

// bumpSidekickUnread increments the unread badge on the Sidekick tab. Content
// that arrives while the Sidekick tab is already in view counts as seen, so
// the badge only accumulates when the tab is hidden. The badge source (the
// agent dashboard push) arrives with the dashboard surface slot; this is the
// display plumbing.
func (m *UI) bumpSidekickUnread() {
	if m.sidekickTabInView() {
		return
	}
	m.sidekickUnread++
}

// renderSidebarTabBar renders the sidebar's tab bar: `[Info] [Sidekick]`,
// with the active tab highlighted and an unread badge (● N) after the
// Sidekick tab while unseen dashboard content is pending.
func (m *UI) renderSidebarTabBar(width int) string {
	t := m.com.Styles

	parts := make([]string, 0, sidebarTabCount+1)
	for tab := sidebarTab(0); tab < sidebarTabCount; tab++ {
		style := t.Sidebar.TabInactive
		if tab == m.sidebarTab {
			style = t.Sidebar.TabActive
		}
		parts = append(parts, style.Render("["+tab.title()+"]"))
	}
	if m.sidekickUnread > 0 {
		parts = append(parts, t.Sidebar.TabBadge.Render(fmt.Sprintf("● %d", m.sidekickUnread)))
	}

	bar := ""
	for i, p := range parts {
		if i > 0 {
			bar += " "
		}
		bar += p
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(bar)
}

// renderSidekickPanel renders the Sidekick tab's content. This is the tab
// shell only: the chat panel (message list, input, streaming) and the pinned
// dashboard surface land in follow-ups, so for now it shows a placeholder.
func (m *UI) renderSidekickPanel(width int) string {
	t := m.com.Styles
	return t.Sidebar.TabPlaceholder.Width(width).Render("Nothing here yet.")
}

// drawSidekickTab draws the Sidekick tab: the tab bar on top, panel content
// below. Like drawSidebar's Info path it reserves the right pad and scrollbar
// gutter columns (as blanks — the placeholder never overflows) so nothing
// shifts when switching tabs.
func (m *UI) drawSidekickTab(scr uv.Screen, area uv.Rectangle, tabBar string, contentWidth, height int) {
	// The tab is on screen, so any pending unread content has been viewed.
	m.sidekickUnread = 0

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		tabBar,
		"",
		m.renderSidekickPanel(contentWidth),
	)
	rendered := lipgloss.NewStyle().
		MaxWidth(contentWidth).
		MaxHeight(height).
		Render(content)

	pad := blankSidebarColumn(height)
	gutter := blankSidebarColumn(height)
	rendered = lipgloss.JoinHorizontal(lipgloss.Top, rendered, pad, gutter)

	uv.NewStyledString(rendered).Draw(scr, area)
}
