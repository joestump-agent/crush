package model

import (
	"testing"

	"charm.land/bubbles/v2/textarea"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/workspace"
)

// busyWorkspace is a workspace.Workspace stub that reports readiness and
// per-session busy state. Only the methods isCurrentSessionBusy touches are
// implemented; the embedded interface panics on anything else.
type busyWorkspace struct {
	workspace.Workspace
	ready    bool
	busy     map[string]bool
	busyCall []string
}

func (w *busyWorkspace) AgentIsReady() bool { return w.ready }

func (w *busyWorkspace) AgentIsSessionBusy(sessionID string) bool {
	w.busyCall = append(w.busyCall, sessionID)
	return w.busy[sessionID]
}

func (w *busyWorkspace) Config() *config.Config { return nil }

func newBusyUI(ws *busyWorkspace) *UI {
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

// TestIsCurrentSessionBusyIsSessionScoped is the core of #28/#29: the current
// session's busy state must reflect only its own activity, never another
// session's, so a paused To-Do is not shown as actively executing.
func TestIsCurrentSessionBusyIsSessionScoped(t *testing.T) {
	t.Parallel()

	t.Run("current session busy", func(t *testing.T) {
		t.Parallel()
		ws := &busyWorkspace{ready: true, busy: map[string]bool{"sess-1": true}}
		m := newBusyUI(ws)
		m.session = &session.Session{ID: "sess-1"}
		if !m.isCurrentSessionBusy() {
			t.Fatal("expected the viewed session's own activity to read as busy")
		}
	})

	t.Run("another session busy does not affect current", func(t *testing.T) {
		t.Parallel()
		ws := &busyWorkspace{ready: true, busy: map[string]bool{"other": true}}
		m := newBusyUI(ws)
		m.session = &session.Session{ID: "sess-1"}
		if m.isCurrentSessionBusy() {
			t.Fatal("another session's activity must not mark the current session busy")
		}
		// The check must be scoped to the current session ID.
		for _, id := range ws.busyCall {
			if id != "sess-1" {
				t.Fatalf("AgentIsSessionBusy queried %q, want only sess-1", id)
			}
		}
	})

	t.Run("agent not ready", func(t *testing.T) {
		t.Parallel()
		ws := &busyWorkspace{ready: false, busy: map[string]bool{"sess-1": true}}
		m := newBusyUI(ws)
		m.session = &session.Session{ID: "sess-1"}
		if m.isCurrentSessionBusy() {
			t.Fatal("a not-ready agent must not read as busy")
		}
	})

	t.Run("no active session", func(t *testing.T) {
		t.Parallel()
		ws := &busyWorkspace{ready: true, busy: map[string]bool{"sess-1": true}}
		m := newBusyUI(ws)
		if m.isCurrentSessionBusy() {
			t.Fatal("no active session must not read as busy")
		}
	})
}

// countingWorkspace counts the workspace probes that are synchronous HTTP
// round-trips in client/server mode.
type countingWorkspace struct {
	workspace.Workspace
	queuedCalls  int
	busyCalls    int
	readyCalls   int
	permCalls    int
	queuedReturn int
}

func (w *countingWorkspace) AgentQueuedPrompts(string) int {
	w.queuedCalls++
	return w.queuedReturn
}
func (w *countingWorkspace) AgentIsSessionBusy(string) bool { w.busyCalls++; return false }
func (w *countingWorkspace) AgentIsBusy() bool              { w.busyCalls++; return false }
func (w *countingWorkspace) AgentIsReady() bool             { w.readyCalls++; return true }
func (w *countingWorkspace) PermissionSkipRequests() bool   { w.permCalls++; return false }
func (w *countingWorkspace) Config() *config.Config         { return nil }

// plainMsg is an arbitrary tea.Msg standing in for keystroke/mouse/tick
// traffic through Update.
type plainMsg struct{}

// TestUpdateDoesNotProbeWorkspacePerMessage pins the hot-path fix: Update
// used to call AgentQueuedPrompts (a synchronous HTTP GET in client/server
// mode) at the top of EVERY message — every keystroke blocked the single
// Update goroutine on a network round-trip. The queue count is now
// event-driven (refreshPromptQueue) and busy probes are briefly memoized.
func TestUpdateDoesNotProbeWorkspacePerMessage(t *testing.T) {
	t.Parallel()

	ws := &countingWorkspace{}
	com := common.DefaultCommon(ws)
	m := &UI{
		com:         com,
		status:      NewStatus(com, nil),
		chat:        NewChat(com, config.ScrollbarDefault),
		textarea:    textarea.New(),
		state:       uiChat,
		focus:       uiFocusEditor,
		width:       140,
		height:      45,
		session:     &session.Session{ID: "s1"},
		keyMap:      DefaultKeyMap(),
		dialog:      dialog.NewOverlay(),
		attachments: attachments.New(nil, attachments.Keymap{}),
	}

	for range 25 {
		m.Update(plainMsg{})
	}
	require.Zero(t, ws.queuedCalls,
		"Update must not call AgentQueuedPrompts per message (HTTP per keystroke in client mode)")
	// The placeholder path may probe busy/yolo once, then memoization holds
	// for the TTL; 25 rapid messages must not produce 25 probes.
	require.LessOrEqual(t, ws.busyCalls, 1,
		"busy probes must be memoized, not once per message")
	require.LessOrEqual(t, ws.permCalls, 1,
		"permission probes must be memoized, not once per message")
}

// TestRefreshPromptQueueUpdatesCachedCount pins the event-driven refresh: a
// single explicit refresh reads the count once and re-layouts on change.
func TestRefreshPromptQueueUpdatesCachedCount(t *testing.T) {
	t.Parallel()

	ws := &countingWorkspace{queuedReturn: 3}
	com := common.DefaultCommon(ws)
	m := &UI{
		com:      com,
		status:   NewStatus(com, nil),
		chat:     NewChat(com, config.ScrollbarDefault),
		textarea: textarea.New(),
		state:    uiChat,
		focus:    uiFocusEditor,
		width:    140,
		height:   45,
		session:  &session.Session{ID: "s1"},
	}

	m.refreshPromptQueue()
	require.Equal(t, 1, ws.queuedCalls)
	require.Equal(t, 3, m.promptQueue)
}

// TestIsCurrentSessionBusyMemoized pins the busy-probe cache: repeated calls
// within the TTL hit the workspace once; invalidateBusyCache forces a fresh
// probe.
func TestIsCurrentSessionBusyMemoized(t *testing.T) {
	t.Parallel()

	ws := &countingWorkspace{}
	com := common.DefaultCommon(ws)
	m := &UI{com: com, session: &session.Session{ID: "s1"}}

	for range 10 {
		m.isCurrentSessionBusy()
	}
	require.Equal(t, 1, ws.busyCalls, "probes within the TTL must be memoized")

	m.invalidateBusyCache()
	m.isCurrentSessionBusy()
	require.Equal(t, 2, ws.busyCalls, "invalidation must force a fresh probe")
}
