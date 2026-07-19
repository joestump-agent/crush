package model

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// dashboardSurface builds a wrapped A2UI surface whose card text is label,
// as the sidekick_update tool publishes it (#57).
func dashboardSurface(label string) string {
	return `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"dash","components":[` +
		`{"component":"Card","id":"root","child":"t"},` +
		`{"component":"Text","id":"t","text":"` + label + `"}` +
		`]}}</a2ui-json>`
}

func pushDashboard(m *UI, label string) {
	m.applySidekickDashboard(pubsub.Event[agenttools.SidekickSurface]{
		Type:    pubsub.CreatedEvent,
		Payload: agenttools.SidekickSurface{Content: dashboardSurface(label)},
	})
}

func TestSidekickDashboardReplaceInPlace(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()

	pushDashboard(m, "Progress 20%")
	pushDashboard(m, "Progress 40%")

	require.Contains(t, m.sidekick.dashboard, "40%", "a new push must replace the previous surface")
	require.NotContains(t, m.sidekick.dashboard, "20%", "surfaces must never stack")
}

func TestSidekickDashboardBumpsUnreadWhenHidden(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	m.setSidebarTab(sidebarTabInfo) // Sidekick tab hidden

	pushDashboard(m, "Progress 20%")
	require.Equal(t, 1, m.sidekickUnread, "each push must set the unread badge (#52)")
	pushDashboard(m, "Progress 40%")
	require.Equal(t, 2, m.sidekickUnread)

	m.setSidebarTab(sidebarTabSidekick)
	require.Zero(t, m.sidekickUnread, "viewing the tab clears the badge")
	pushDashboard(m, "Progress 60%")
	require.Zero(t, m.sidekickUnread, "content arriving on the visible tab is already seen")
}

func TestSidekickDashboardRendersPinnedAtTop(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	pushDashboard(m, "Build passing")

	out := ansi.Strip(m.renderSidekickPanel(sidekickSidebarWidth, 30))
	require.Contains(t, out, "Build passing", "the dashboard surface must render in the panel")
	require.NotContains(t, out, "a2ui-json", "the raw wire format must stay hidden")

	// Pinned at the top: the surface renders above the placeholder chat
	// content and the prompt input.
	dashIdx := strings.Index(out, "Build passing")
	inputIdx := strings.Index(out, sidekickPlaceholder)
	require.GreaterOrEqual(t, inputIdx, 0)
	require.Less(t, dashIdx, inputIdx, "the dashboard must be pinned above the chat input")
}

func TestSidekickDashboardClearedBySlashClear(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	pushDashboard(m, "Progress 20%")

	m.sidekick.input.SetValue("/clear")
	m.submitSidekickPrompt()
	require.Empty(t, m.sidekick.dashboard, "/clear must drop the pinned dashboard")
}

func TestSidekickDashboardDismissKey(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	pushDashboard(m, "Progress 20%")

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	require.Empty(t, m.sidekick.dashboard, "ctrl+x in the Sidekick pane must dismiss the dashboard")
}

func TestSidekickDashboardPersistsAcrossChatEvents(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	pushDashboard(m, "Progress 80%")

	// Sidekick chat traffic (a whole conversation reset, even) must not
	// touch the pinned dashboard: it belongs to the main agent's turn
	// lifecycle, not the Sidekick conversation.
	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "u1", SessionID: "old", Role: message.User},
	})
	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "u2", SessionID: "new", Role: message.User},
	})
	require.Contains(t, m.sidekick.dashboard, "80%")
}

func TestSidekickDashboardEmptyPushIgnored(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	pushDashboard(m, "Progress 20%")

	m.applySidekickDashboard(pubsub.Event[agenttools.SidekickSurface]{Type: pubsub.CreatedEvent})
	require.Contains(t, m.sidekick.dashboard, "20%", "an empty payload must not clear the dashboard")
}

func TestSidekickDashboardSubscribeOnce(t *testing.T) {
	t.Parallel()
	m := newSidekickTestUI()
	ws := &sidekickTestWorkspace{cfg: &config.Config{Options: &config.Options{}}}
	m.com.Workspace = ws

	require.NotNil(t, m.subscribeSidekickDashboard())
	require.NotNil(t, m.sidekick.dashboardEvents)
	require.Nil(t, m.subscribeSidekickDashboard(), "resubscribing must be a no-op")
}
