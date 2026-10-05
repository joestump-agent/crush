package model

// Tests for the sessions picker's async sub-agent tree load (#409):
// ctrl+s opens the picker with zero child-session fetches on the Update
// loop, the returned command makes exactly one bulk ListAllChildSessions
// call, a stale generation is dropped, and a failed load warns once
// while leaving the picker usable.

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/scheduler"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/crush/internal/workspace"
)

// sessionsTreeLoadWorkspace serves a fixed session layout for the tree
// load and counts every child-session probe, so tests can pin exactly
// which fetches ran and when.
type sessionsTreeLoadWorkspace struct {
	workspace.Workspace

	parents  []session.Session
	children []session.Session
	allErr   error

	listChildCalls    int
	listAllChildCalls int
}

func (w *sessionsTreeLoadWorkspace) ListSessions(context.Context) ([]session.Session, error) {
	return w.parents, nil
}

func (w *sessionsTreeLoadWorkspace) Config() *config.Config { return nil }

func (w *sessionsTreeLoadWorkspace) AgentIsReady() bool { return true }

func (w *sessionsTreeLoadWorkspace) AgentIsBusy() bool { return false }

func (w *sessionsTreeLoadWorkspace) AgentIsSessionBusy(string) bool             { return false }
func (w *sessionsTreeLoadWorkspace) AgentReadyErr() error                       { return nil }
func (w *sessionsTreeLoadWorkspace) AgentQueuedPrompts(string) int              { return 0 }
func (w *sessionsTreeLoadWorkspace) AgentQueuedPromptsList(string) []string     { return nil }
func (w *sessionsTreeLoadWorkspace) AgentClearQueue(string)                     {}
func (w *sessionsTreeLoadWorkspace) AgentCancel(string)                         {}
func (w *sessionsTreeLoadWorkspace) AgentListCronTasks(string) []scheduler.Task { return nil }
func (w *sessionsTreeLoadWorkspace) AgentModel() workspace.AgentModel           { return workspace.AgentModel{} }

func (w *sessionsTreeLoadWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *sessionsTreeLoadWorkspace) PermissionSetSkipRequests(bool)                   {}
func (w *sessionsTreeLoadWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }
func (w *sessionsTreeLoadWorkspace) LSPGetDiagnosticCounts(string) lsp.DiagnosticCounts {
	return lsp.DiagnosticCounts{}
}
func (w *sessionsTreeLoadWorkspace) LSPStart(context.Context, string) {}
func (w *sessionsTreeLoadWorkspace) ListMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func (w *sessionsTreeLoadWorkspace) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}
func (w *sessionsTreeLoadWorkspace) WorkingDir() string { return "" }

func (w *sessionsTreeLoadWorkspace) ListChildSessions(context.Context, string) ([]session.Session, error) {
	w.listChildCalls++
	return nil, nil
}

func (w *sessionsTreeLoadWorkspace) ListAllChildSessions(context.Context) ([]session.Session, error) {
	w.listAllChildCalls++
	return w.children, w.allErr
}

func (w *sessionsTreeLoadWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	for i := 0; i+1 < len(sessionID); i++ {
		if sessionID[i] == '$' && sessionID[i+1] == '$' {
			return sessionID[:i], sessionID[i+2:], true
		}
	}
	return "", "", false
}

func newSessionsTreeLoadUI(t *testing.T, ws *sessionsTreeLoadWorkspace) *UI {
	t.Helper()
	m := newTestUI()
	m.com.Workspace = ws
	m.keyMap = DefaultKeyMap()
	m.dialog = dialog.NewOverlay()
	m.attachments = attachments.New(
		attachments.NewRenderer(
			lipgloss.NewStyle(), lipgloss.NewStyle(),
			lipgloss.NewStyle(), lipgloss.NewStyle(),
			lipgloss.NewStyle(), lipgloss.NewStyle(),
			lipgloss.NewStyle(),
		),
		attachments.Keymap{},
	)
	return m
}

func sessionsTreeDialog(m *UI) *dialog.Session {
	sd, _ := m.dialog.Dialog(dialog.SessionsID).(*dialog.Session)
	return sd
}

// pressAgents drives ctrl+] into the open sessions dialog: a nil action
// means the sub-menu pushed, an ActionCmd carries a report.
func pressAgents(t *testing.T, m *UI) dialog.Action {
	t.Helper()
	sd := sessionsTreeDialog(m)
	require.NotNil(t, sd, "the sessions dialog must be open")
	return sd.HandleMsg(tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})
}

// treeLoadFrom runs the command chain Update returned for ctrl+s and
// extracts the tree-load message: the fetch itself must happen inside
// that command, not inside Update.
func treeLoadFrom(t *testing.T, cmd tea.Cmd) sessionChildrenLoadedMsg {
	t.Helper()
	for _, msg := range runCmdTree(cmd) {
		if loaded, ok := msg.(sessionChildrenLoadedMsg); ok {
			return loaded
		}
	}
	t.Fatal("ctrl+s must return the async tree load")
	return sessionChildrenLoadedMsg{}
}

func TestSessionsTreeLoadHappensOffTheUpdateLoop(t *testing.T) {
	t.Parallel()

	ws := &sessionsTreeLoadWorkspace{
		parents:  []session.Session{{ID: "p1", Title: "Parent One"}},
		children: []session.Session{{ID: "m1$$t1", ParentSessionID: "p1", Title: "Dispatched Agent"}},
	}
	m := newSessionsTreeLoadUI(t, ws)

	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.True(t, m.dialog.ContainsDialog(dialog.SessionsID))
	require.Zero(t, ws.listChildCalls,
		"opening the picker must not fetch child sessions per parent")
	require.Zero(t, ws.listAllChildCalls,
		"the bulk fetch must run in the command, not inside Update")

	// Before the load lands, ctrl+] reports loading rather than absence.
	require.IsType(t, dialog.ActionCmd{}, pressAgents(t, m))

	loaded := treeLoadFrom(t, cmd)
	require.Equal(t, m.sessionsTreeGen, loaded.gen)
	require.Equal(t, 1, ws.listAllChildCalls,
		"one bulk fetch replaces the per-parent loads")

	m.Update(loaded)

	// The tree landed: the same chord now pushes the sub-menu.
	require.Nil(t, pressAgents(t, m))
}

func TestSessionsTreeLoadStaleGenerationDropped(t *testing.T) {
	t.Parallel()

	ws := &sessionsTreeLoadWorkspace{
		parents:  []session.Session{{ID: "p1", Title: "Parent One"}},
		children: []session.Session{{ID: "m1$$t1", ParentSessionID: "p1", Title: "Dispatched Agent"}},
	}
	m := newSessionsTreeLoadUI(t, ws)

	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	loaded := treeLoadFrom(t, cmd)

	// A close or reopen supersedes the in-flight load.
	m.sessionsTreeGen++

	m.Update(loaded)

	require.IsType(t, dialog.ActionCmd{}, pressAgents(t, m),
		"a stale load must change nothing: the dialog is still waiting")
}

func TestSessionsTreeLoadErrorWarnsAndStaysUsable(t *testing.T) {
	t.Parallel()

	ws := &sessionsTreeLoadWorkspace{
		parents: []session.Session{{ID: "p1", Title: "Parent One"}},
		allErr:  errors.New("socket closed"),
	}
	m := newSessionsTreeLoadUI(t, ws)

	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	loaded := treeLoadFrom(t, cmd)

	_, cmd = m.Update(loaded)
	msgs := runCmdTree(cmd)
	warned := false
	for _, msg := range msgs {
		if info, ok := msg.(util.InfoMsg); ok &&
			info.Type == util.InfoTypeWarn &&
			strings.Contains(info.Msg, "Couldn't load sub-agent sessions") {
			warned = true
		}
	}
	require.True(t, warned, "a failed load must warn exactly through the status message")

	// The picker stays usable: ctrl+] reports absence instead of
	// "loading" forever.
	action, ok := pressAgents(t, m).(dialog.ActionCmd)
	require.True(t, ok)
	reports := runCmdTree(action.Cmd)
	reported := false
	for _, msg := range reports {
		if info, ok := msg.(util.InfoMsg); ok &&
			info.Type == util.InfoTypeInfo &&
			strings.Contains(info.Msg, "No sub-agent sessions to inspect") {
			reported = true
		}
	}
	require.True(t, reported, "after a failed load ctrl+] must report absence, not endless loading")
}
