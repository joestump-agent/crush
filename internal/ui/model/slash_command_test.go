package model

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/textarea"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/workspace"
)

// slashCommandWorkspace is a minimal workspace stub for slash command tests.
type slashCommandWorkspace struct {
	workspace.Workspace
	ready    bool
	busy     map[string]bool
	summErr  error
	summCall string
}

func (w *slashCommandWorkspace) AgentIsReady() bool { return w.ready }

func (w *slashCommandWorkspace) AgentIsBusy() bool {
	for _, b := range w.busy {
		if b {
			return true
		}
	}
	return false
}

func (w *slashCommandWorkspace) AgentIsSessionBusy(sessionID string) bool {
	return w.busy[sessionID]
}

func (w *slashCommandWorkspace) Config() *config.Config { return nil }

func (w *slashCommandWorkspace) AgentSummarize(_ context.Context, sessionID string) error {
	w.summCall = sessionID
	return w.summErr
}

func newSlashCommandUI(ws *slashCommandWorkspace) *UI {
	com := common.DefaultCommon(ws)
	return &UI{
		com:      com,
		status:   NewStatus(com, nil),
		chat:     NewChat(com, config.ScrollbarDefault),
		textarea: textarea.New(),
		state:    uiChat,
		focus:    uiFocusEditor,
		width:    140,
		height:   45,
	}
}

// TestSlashCommandNotMatched verifies that non-slash input returns false so
// normal message processing continues.
func TestSlashCommandNotMatched(t *testing.T) {
	t.Parallel()

	m := newSlashCommandUI(&slashCommandWorkspace{ready: true})
	m.session = &session.Session{ID: "s1"}

	cases := []string{"hello", "/unknown", "  /clear extra", "/compact now", ""}
	for _, tc := range cases {
		_, ok := m.handleSlashCommand(tc)
		if ok {
			t.Fatalf("handleSlashCommand(%q) should not match", tc)
		}
	}
}

// TestSlashClearStartsNewSession verifies /clear resets to landing state.
func TestSlashClearStartsNewSession(t *testing.T) {
	t.Parallel()

	m := newSlashCommandUI(&slashCommandWorkspace{ready: true})
	m.session = &session.Session{ID: "s1"}

	_, ok := m.handleSlashCommand("/clear")
	if !ok {
		t.Fatal("handleSlashCommand(/clear) should match")
	}
	if m.session != nil {
		t.Fatal("expected session to be nil after /clear")
	}
	if m.state != uiLanding {
		t.Fatal("expected state to be uiLanding after /clear")
	}
}

// TestSlashClearBlockedWhenAgentBusy verifies /clear warns when busy, keeps
// the session, and still resets the prompt history.
func TestSlashClearBlockedWhenAgentBusy(t *testing.T) {
	t.Parallel()

	m := newSlashCommandUI(&slashCommandWorkspace{ready: true, busy: map[string]bool{"s1": true}})
	m.session = &session.Session{ID: "s1"}
	// isAgentBusy is a pure cache read (workspace probes happen off-thread);
	// seed the memoized busy state the way a refresh would.
	m.agentBusyCache.set(true)
	m.promptHistory.index = 3
	m.promptHistory.draft = "wip"

	_, ok := m.handleSlashCommand("/clear")
	if !ok {
		t.Fatal("handleSlashCommand(/clear) should still match when busy")
	}
	if m.session == nil {
		t.Fatal("session should not be cleared when agent is busy")
	}
	if m.promptHistory.index != -1 || m.promptHistory.draft != "" {
		t.Fatalf("prompt history not reset on busy /clear: index=%d draft=%q", m.promptHistory.index, m.promptHistory.draft)
	}
}

// TestSlashClearNoSessionResetsHistory verifies /clear with no active session
// is a no-op for session state but still resets the prompt history/draft —
// newSession early-returns without resetting, so /clear handles it directly.
func TestSlashClearNoSessionResetsHistory(t *testing.T) {
	t.Parallel()

	m := newSlashCommandUI(&slashCommandWorkspace{ready: true})
	m.promptHistory.index = 3
	m.promptHistory.draft = "wip"

	cmd, ok := m.handleSlashCommand("/clear")
	if !ok {
		t.Fatal("handleSlashCommand(/clear) should match without a session")
	}
	if cmd != nil {
		t.Fatal("expected no command when there is no session to clear")
	}
	if m.promptHistory.index != -1 || m.promptHistory.draft != "" {
		t.Fatalf("prompt history not reset on no-session /clear: index=%d draft=%q", m.promptHistory.index, m.promptHistory.draft)
	}
}

// TestSlashCompactTriggersSummarize verifies /compact calls AgentSummarize.
func TestSlashCompactTriggersSummarize(t *testing.T) {
	t.Parallel()

	ws := &slashCommandWorkspace{ready: true}
	m := newSlashCommandUI(ws)
	m.session = &session.Session{ID: "s1"}

	cmd, ok := m.handleSlashCommand("/compact")
	if !ok {
		t.Fatal("handleSlashCommand(/compact) should match")
	}
	if cmd == nil {
		t.Fatal("expected a command to be returned")
	}
	cmd()
	if ws.summCall != "s1" {
		t.Fatalf("expected AgentSummarize called with s1, got %q", ws.summCall)
	}
}

// TestSlashCompactNoSession verifies /compact is a no-op without a session.
func TestSlashCompactNoSession(t *testing.T) {
	t.Parallel()

	m := newSlashCommandUI(&slashCommandWorkspace{ready: true})

	_, ok := m.handleSlashCommand("/compact")
	if !ok {
		t.Fatal("handleSlashCommand(/compact) should still match without session")
	}
}

// TestSlashCompactBlockedWhenAgentBusy verifies /compact warns when busy and
// does not call AgentSummarize even if the returned cmd is executed.
func TestSlashCompactBlockedWhenAgentBusy(t *testing.T) {
	t.Parallel()

	ws := &slashCommandWorkspace{ready: true, busy: map[string]bool{"s1": true}}
	m := newSlashCommandUI(ws)
	m.session = &session.Session{ID: "s1"}
	// isAgentBusy is a pure cache read (workspace probes happen off-thread);
	// seed the memoized busy state the way a refresh would.
	m.agentBusyCache.set(true)

	cmd, ok := m.handleSlashCommand("/compact")
	if !ok {
		t.Fatal("handleSlashCommand(/compact) should still match when busy")
	}
	if cmd != nil {
		cmd()
	}
	if ws.summCall != "" {
		t.Fatal("AgentSummarize should not be called when agent is busy")
	}
}
