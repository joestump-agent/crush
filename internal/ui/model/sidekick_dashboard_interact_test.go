package model

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/joestump-agent/a2tea/event"
	"github.com/stretchr/testify/require"
)

// mainAgentSidekickWorkspace extends the Sidekick stub with the main-agent
// methods the dashboard submission path (#56/#45) is allowed to reach:
// pressing a button on the agent-pushed dashboard starts a MAIN agent turn.
// Everything else still panics through the embedded stub's nil interface.
type mainAgentSidekickWorkspace struct {
	*sidekickTestWorkspace

	agentPrompts []string
}

func (w *mainAgentSidekickWorkspace) AgentIsReady() bool { return true }

func (w *mainAgentSidekickWorkspace) AgentRun(_ context.Context, _, prompt string, _ ...message.Attachment) error {
	w.agentPrompts = append(w.agentPrompts, prompt)
	return nil
}

// dashboardFormSurface is a dashboard push carrying an interactive form:
// a pre-filled TextField plus submit and cancel buttons, on surface "dash".
const dashboardFormSurface = `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"dash","components":[` +
	`{"component":"Card","id":"root","child":"col"},` +
	`{"component":"Column","id":"col","children":["name","btn-send","btn-cancel"]},` +
	`{"component":"TextField","id":"name","label":"Name","value":"Joe"},` +
	`{"component":"Button","id":"btn-send","child":"btn-send-t"},` +
	`{"component":"Text","id":"btn-send-t","text":"Send"},` +
	`{"component":"Button","id":"btn-cancel","child":"btn-cancel-t"},` +
	`{"component":"Text","id":"btn-cancel-t","text":"Cancel"}` +
	`]}}</a2ui-json>`

// newDashboardInteractUI builds a Sidekick-pane-focused UI with the form
// dashboard pinned and a workspace that records main-agent turns.
func newDashboardInteractUI(t *testing.T) (*UI, *mainAgentSidekickWorkspace) {
	t.Helper()
	m, base := newSidekickPanelTestUI()
	// Keep the subscribe cmds nil: drainCmd executes every cmd in a batch
	// synchronously, and a live await-event cmd would block on the stub's
	// channel forever.
	base.subscribeNil = true
	ws := &mainAgentSidekickWorkspace{sidekickTestWorkspace: base}
	m.com.Workspace = ws
	pushDashboardContent(m, dashboardFormSurface)
	return m, ws
}

func pushDashboardContent(m *UI, content string) {
	m.applySidekickDashboard(pubsub.Event[agenttools.SidekickSurface]{
		Type:    pubsub.CreatedEvent,
		Payload: agenttools.SidekickSurface{Content: content},
	})
}

func TestSidekickDashboardHoldsLiveSurface(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)

	sk := &m.sidekick
	require.NotNil(t, sk.dashboardModel, "a push must build a live a2tea model, not a frozen string")
	require.Equal(t, "dash", sk.dashboardSurfaceID)
	require.False(t, sk.dashboardRetired)
	require.False(t, sk.dashboardModel.Focused(), "the surface must start blurred; the prompt input holds focus")
}

func TestSidekickDashboardShiftTabTogglesFocus(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)
	sk := &m.sidekick

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.True(t, sk.dashboardFocused, "shift+tab must move focus onto the dashboard surface")
	require.True(t, sk.dashboardModel.Focused(), "the a2tea focus ring must engage")
	require.False(t, sk.input.Focused(), "the prompt input must yield focus")

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.False(t, sk.dashboardFocused, "shift+tab must toggle focus back to the prompt")
	require.False(t, sk.dashboardModel.Focused())
	require.True(t, sk.input.Focused())
}

func TestSidekickDashboardFocusedSurfaceConsumesTab(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)
	m.focusSidekickDashboard()

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, sidebarTabSidekick, m.sidebarTab,
		"Tab must cycle the focused surface's focus ring, not the sidebar tabs (#44 semantics)")
	require.True(t, m.sidekick.dashboardFocused, "the surface keeps focus across Tab")
}

func TestSidekickDashboardTypingStillReachesPromptWhenUnfocused(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: 'h', Text: "h"})
	require.Equal(t, "h", m.sidekick.input.Value(),
		"with the surface unfocused, typing must feed the prompt input as before")
}

func TestSidekickDashboardEscStepsBackToPrompt(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)
	m.focusSidekickDashboard()

	cmd := m.handleSidekickEscape()
	require.False(t, m.sidekick.dashboardFocused, "esc must step focus back from the surface to the prompt")
	require.True(t, m.sidekick.input.Focused())
	require.Equal(t, uiFocusSidebar, m.focus, "esc from the surface must not leave the pane")
	_ = cmd
}

func TestSidekickDashboardButtonSubmitsToMainAgent(t *testing.T) {
	t.Parallel()
	m, ws := newDashboardInteractUI(t)
	m.focusSidekickDashboard()

	cmd := m.handleA2UIButtonClicked(event.ButtonClicked{
		Source: event.Source{ComponentID: "btn-send", SurfaceID: "dash"},
		ID:     "btn-send",
	})
	require.NotNil(t, cmd, "a submit button on the dashboard must start a turn")
	drainCmd(t, cmd)

	require.Len(t, ws.agentPrompts, 1, "the submission must reach the MAIN agent (the dashboard is its channel)")
	prompt := ws.agentPrompts[0]
	require.Contains(t, prompt, `"btn-send"`, "the prompt must name the pressed button")
	require.Contains(t, prompt, `"dash"`, "the prompt must name the surface")
	require.True(t, strings.Contains(prompt, `name: "Joe"`),
		"the prompt must carry the surface's field values, got: %q", prompt)
	require.Empty(t, ws.runPrompts, "the Sidekick agent must never see dashboard submissions")

	// The new main-agent turn clears the pinned dashboard (#56) and focus
	// is back on the prompt input.
	require.Empty(t, m.sidekick.dashboard)
	require.Nil(t, m.sidekick.dashboardModel)
	require.False(t, m.sidekick.dashboardFocused)
	require.True(t, m.sidekick.input.Focused())
}

func TestSidekickDashboardCancelDismissesWithoutTurn(t *testing.T) {
	t.Parallel()
	m, ws := newDashboardInteractUI(t)
	m.focusSidekickDashboard()

	cmd := m.handleA2UIButtonClicked(event.ButtonClicked{
		Source: event.Source{ComponentID: "btn-cancel", SurfaceID: "dash"},
		ID:     "btn-cancel",
	})
	drainCmd(t, cmd)

	require.Empty(t, ws.agentPrompts, "a cancel button must not start a turn")
	require.Empty(t, m.sidekick.dashboard, "cancel must unpin the dashboard")
	require.True(t, m.sidekick.input.Focused(), "focus must return to the prompt input")
}

func TestSidekickDashboardClickIgnoredWhenUnfocused(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)

	// Only a focused surface emits button events; an event arriving while
	// the dashboard is blurred belongs to a main-chat surface and must not
	// retire (or read values from) the dashboard.
	values, ok := m.retireSidekickDashboardSurface("dash")
	require.False(t, ok)
	require.Nil(t, values)
	require.False(t, m.sidekick.dashboardRetired)
}

func TestSidekickDashboardReplacePreservesFocusAndClearsRetire(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)
	m.focusSidekickDashboard()

	pushDashboardContent(m, dashboardFormSurface)
	sk := &m.sidekick
	require.True(t, sk.dashboardFocused, "a replacement push must not steal focus mid-interaction")
	require.True(t, sk.dashboardModel.Focused())

	// Retired form + replacement push: the fresh surface is interactive
	// again.
	sk.dashboardFocused = true
	_, ok := m.retireSidekickDashboardSurface("dash")
	require.True(t, ok)
	require.True(t, sk.dashboardRetired)
	pushDashboardContent(m, dashboardFormSurface)
	require.False(t, sk.dashboardRetired, "an agent update supersedes the retired form")
}

func TestSidekickDashboardEnterEmitsButtonClicked(t *testing.T) {
	t.Parallel()
	m, _ := newDashboardInteractUI(t)
	m.focusSidekickDashboard()

	// Walk the focus ring far enough to land on a button, pressing Enter at
	// each stop: a live, wired surface must eventually emit ButtonClicked
	// through the key path (field → send → cancel ring).
	var clicked *event.ButtonClicked
	for range 4 {
		cmd := m.handleSidekickKey(tea.KeyPressMsg{Code: tea.KeyEnter})
		for _, msg := range drainCmd(t, cmd) {
			if bc, ok := msg.(event.ButtonClicked); ok {
				clicked = &bc
				break
			}
		}
		if clicked != nil {
			break
		}
		drainCmd(t, m.handleSidekickKey(tea.KeyPressMsg{Code: tea.KeyTab}))
	}
	require.NotNil(t, clicked, "Enter on a focused button must emit ButtonClicked")
	require.Equal(t, "dash", clicked.SurfaceID)
}
