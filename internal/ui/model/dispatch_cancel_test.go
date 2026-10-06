package model

// Tests for ctrl+x, the cancel-a-dispatched-agent binding (#373). The
// target rules: a live dispatch card selected in the chat, or the dispatch
// being viewed in inspect mode. Everything else — plain agent blocks,
// finished dispatches, sidebar focus — is inert, and esc in inspect mode
// still only exits.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/textarea"
	"github.com/charmbracelet/crush/internal/workspace"
)

// cancelDispatchWorkspace records CancelDispatch calls so tests can pin
// which session the binding targeted. Unimplemented methods panic through
// the embedded interface, like the other model-test workspaces.
type cancelDispatchWorkspace struct {
	slashCommandWorkspace

	cancelCalls []string
	cancelErr   error
}

func (w *cancelDispatchWorkspace) CancelDispatch(_ context.Context, sessionID string) error {
	w.cancelCalls = append(w.cancelCalls, sessionID)
	return w.cancelErr
}

var _ workspace.Workspace = (*cancelDispatchWorkspace)(nil)

func newCancelDispatchUI(ws *cancelDispatchWorkspace) *UI {
	com := common.DefaultCommon(ws)
	m := &UI{
		com:         com,
		status:      NewStatus(com, nil),
		chat:        NewChat(com, config.ScrollbarDefault),
		textarea:    textarea.New(),
		state:       uiChat,
		focus:       uiFocusMain,
		width:       140,
		height:      45,
		keyMap:      DefaultKeyMap(),
		dialog:      dialog.NewOverlay(),
		attachments: attachments.New(nil, attachments.Keymap{}),
	}
	m.session = &session.Session{ID: "parent-1"}
	return m
}

// dispatchCardItem builds a live dispatch card carrying the given child
// session ID, the same running-handle payload the dispatch_agent tool
// returns.
func dispatchCardItem(t *testing.T, childSessionID string, status dispatch.Status) *chat.DispatchToolMessageItem {
	t.Helper()
	handle := dispatch.DispatchResult{
		DispatchID:    "dispatch-1",
		Branch:        "crush-dispatch-dispatch-1",
		WorkspacePath: "/tmp/ws",
		SessionID:     childSessionID,
		Status:        status,
	}
	b, err := json.Marshal(handle)
	require.NoError(t, err)
	sty := styles.CharmtonePantera()
	item := chat.NewDispatchToolMessageItem(&sty, message.ToolCall{
		ID:       "call-dispatch-1",
		Name:     "dispatch_agent",
		Input:    `{"prompt":"implement the login form with validation"}`,
		Finished: true,
	}, &message.ToolResult{ToolCallID: "call-dispatch-1", Content: string(b)}, false)
	if !status.IsTerminal() {
		// A card is only "live" from a registry snapshot: the fallback
		// running handle alone is static by definition.
		item.SetDispatchSnapshot(dispatch.TodoSnapshot{
			Entry: dispatch.Entry{
				ID:        handle.DispatchID,
				SessionID: childSessionID,
				Status:    status,
			},
		})
	}
	return item
}

// ctrl+x on a selected live dispatch card in the chat cancels that
// dispatch and nothing else.
func TestCancelAgentKeyOnSelectedLiveDispatchCard(t *testing.T) {
	ws := &cancelDispatchWorkspace{}
	m := newCancelDispatchUI(ws)
	m.chat.SetMessages(dispatchCardItem(t, "child-dispatch-1", dispatch.StatusRunning))
	m.chat.SetSelected(0)

	handled, cmd := m.handleInspectKeys(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	require.True(t, handled, "a live dispatch card is a ctrl+x target")
	require.NotNil(t, cmd)
	require.Equal(t, []string{"child-dispatch-1"}, ws.cancelCalls)
}

// ctrl+x while inspecting a live dispatch cancels the dispatch being
// viewed, and ctrl+x while inspecting a plain agent-tool session is
// inert: the kill is dispatch-only.
func TestCancelAgentKeyWhileInspecting(t *testing.T) {
	t.Run("inspecting a live dispatch", func(t *testing.T) {
		ws := &cancelDispatchWorkspace{}
		m := newCancelDispatchUI(ws)
		m.inspecting = &session.Session{ID: "child-dispatch-1"}
		m.inspectRing = []string{"child-dispatch-1"}
		m.inspectDispatchTargets = map[string]bool{"child-dispatch-1": true}

		handled, _ := m.handleInspectKeys(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
		require.True(t, handled)
		require.Equal(t, []string{"child-dispatch-1"}, ws.cancelCalls)
	})

	t.Run("inspecting a plain agent tool", func(t *testing.T) {
		ws := &cancelDispatchWorkspace{}
		m := newCancelDispatchUI(ws)
		m.inspecting = &session.Session{ID: "msg-1$$call-1"}
		m.inspectRing = []string{"msg-1$$call-1"}
		m.inspectDispatchTargets = nil

		handled, _ := m.handleInspectKeys(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
		require.False(t, handled, "a plain agent tool is not a dispatch cancel target")
		require.Empty(t, ws.cancelCalls)
	})
}

// Non-targets never reach the workspace: a finished dispatch card, a
// non-dispatch item, editor focus, and an empty chat are all inert.
func TestCancelAgentKeyNonTargets(t *testing.T) {
	tests := []struct {
		name string
		set  func(t *testing.T, m *UI)
	}{
		{
			name: "finished dispatch card selected",
			set: func(t *testing.T, m *UI) {
				m.chat.SetMessages(dispatchCardItem(t, "child-dispatch-1", dispatch.StatusCompleted))
				m.chat.SetSelected(0)
			},
		},
		{
			name: "non-dispatch item selected",
			set: func(t *testing.T, m *UI) {
				sty := styles.CharmtonePantera()
				user := chat.NewUserMessageItem(&sty, &message.Message{ID: "u1", Role: message.User}, nil)
				m.chat.SetMessages(user)
				m.chat.SetSelected(0)
			},
		},
		{
			name: "editor focus",
			set: func(t *testing.T, m *UI) {
				m.focus = uiFocusEditor
				m.chat.SetMessages(dispatchCardItem(t, "child-dispatch-1", dispatch.StatusRunning))
				m.chat.SetSelected(0)
			},
		},
		{
			name: "empty chat",
			set:  func(t *testing.T, m *UI) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := &cancelDispatchWorkspace{}
			m := newCancelDispatchUI(ws)
			tt.set(t, m)

			handled, _ := m.handleInspectKeys(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
			require.False(t, handled, "no live dispatch targeted: the key must fall through")
			require.Empty(t, ws.cancelCalls)
		})
	}
}

// esc while inspecting a live dispatch still only exits inspect mode and
// never cancels anything (#373 decision: Esc behaviour in inspect mode is
// unchanged).
func TestCancelAgentKeyEscStillOnlyExits(t *testing.T) {
	ws := &cancelDispatchWorkspace{}
	m := newCancelDispatchUI(ws)
	m.inspecting = &session.Session{ID: "child-dispatch-1"}
	m.inspectRing = []string{"child-dispatch-1"}
	m.inspectDispatchTargets = map[string]bool{"child-dispatch-1": true}

	handled, _ := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	require.True(t, handled)
	require.False(t, m.isInspecting())
	require.Empty(t, ws.cancelCalls)
}

// A workspace refusal surfaces as an error report, not a success toast.
func TestCancelAgentKeyWorkspaceErrorIsReported(t *testing.T) {
	ws := &cancelDispatchWorkspace{cancelErr: errors.New("dispatch dispatch-1 already finished (completed)")}
	m := newCancelDispatchUI(ws)
	m.chat.SetMessages(dispatchCardItem(t, "child-dispatch-1", dispatch.StatusRunning))
	m.chat.SetSelected(0)

	handled, cmd := m.handleInspectKeys(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	require.True(t, handled)
	require.NotNil(t, cmd, "the error is reported through a command")
	require.Equal(t, []string{"child-dispatch-1"}, ws.cancelCalls)
}
