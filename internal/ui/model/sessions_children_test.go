package model

// Tests for the sessions picker's sub-agent tree loading (#409): opening
// the picker makes no child-session fetch on the Update goroutine, the
// returned command runs the batch fetch exactly once, a batch for a
// picker that was closed or reopened is dropped, and a failed fetch warns
// while leaving the picker usable.

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/dialog"
)

func sessionsChildrenFixture() (*countingWorkspace, *UI) {
	ws := &countingWorkspace{
		sessions:      []session.Session{{ID: "p1", Title: "Parent One"}, {ID: "p2", Title: "Parent Two"}},
		childSessions: []session.Session{{ID: "m1$$t1", ParentSessionID: "p1", Title: "Dispatched Agent"}},
	}
	return ws, newBusyUI(ws)
}

// openSessionsPicker presses ctrl+s the way the runtime delivers the key.
func openSessionsPicker(m *UI) tea.Cmd {
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	return cmd
}

// runSessionsFetch executes a command tree the way the runtime would and
// returns the first sessionChildrenLoadedMsg it produced, if any.
func runSessionsFetch(cmd tea.Cmd) (sessionChildrenLoadedMsg, bool) {
	if cmd == nil {
		return sessionChildrenLoadedMsg{}, false
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			if loaded, ok := runSessionsFetch(c); ok {
				return loaded, true
			}
		}
	case sessionChildrenLoadedMsg:
		return msg, true
	}
	return sessionChildrenLoadedMsg{}, false
}

func openSessionsDialog(t *testing.T, m *UI) sessionChildrenLoadedMsg {
	t.Helper()
	loaded, ok := runSessionsFetch(openSessionsPicker(m))
	require.True(t, ok, "opening the picker must return the batch-fetch command")
	return loaded
}

func TestCtrlSFetchesNoChildrenSynchronously(t *testing.T) {
	ws, m := sessionsChildrenFixture()

	cmd := openSessionsPicker(m)

	require.Zero(t, ws.listChildCalls,
		"ctrl+s must not fetch children per session on the Update goroutine")
	require.Zero(t, ws.listAllChildCalls,
		"ctrl+s must not run the batch fetch on the Update goroutine")
	require.NotNil(t, cmd, "opening the picker returns the batch-fetch command")

	loaded, ok := runSessionsFetch(cmd)
	require.True(t, ok, "the command returned with the picker delivers the batch")
	require.Equal(t, 1, ws.listAllChildCalls,
		"the command fetches the whole tree in exactly one call")
	require.Zero(t, ws.listChildCalls, "the per-parent probe stays gone")
	require.Equal(t, m.sessionsChildrenGen, loaded.gen)
}

func TestStaleSessionsChildBatchIsDropped(t *testing.T) {
	_, m := sessionsChildrenFixture()

	loaded := openSessionsDialog(t, m)
	_, cmd := m.Update(loaded)
	require.Nil(t, cmd)

	sessionsDialog, ok := m.dialog.Dialog(dialog.SessionsID).(*dialog.Session)
	require.True(t, ok)
	require.True(t, sessionsDialog.ChildrenLoaded(), "the live batch applies")

	// Closing and reopening bumps the generation: the first picker's
	// in-flight batch must not land on the second picker.
	m.dialog.CloseDialog(dialog.SessionsID)
	_ = openSessionsPicker(m)
	reopened, ok := m.dialog.Dialog(dialog.SessionsID).(*dialog.Session)
	require.True(t, ok)
	require.False(t, reopened.ChildrenLoaded())

	_, cmd = m.Update(loaded)
	require.Nil(t, cmd)
	require.False(t, reopened.ChildrenLoaded(),
		"a batch fetched for the closed picker must not apply to the reopened one")
}

func TestClosedSessionsChildBatchIsDropped(t *testing.T) {
	_, m := sessionsChildrenFixture()

	loaded := openSessionsDialog(t, m)
	m.dialog.CloseDialog(dialog.SessionsID)

	_, cmd := m.Update(loaded)
	require.Nil(t, cmd, "a batch for a picker that is gone applies nowhere")
}

func TestFailedSessionsChildBatchWarnsAndPickerStaysUsable(t *testing.T) {
	ws, m := sessionsChildrenFixture()
	ws.allChildErr = errors.New("connection refused")

	loaded := openSessionsDialog(t, m)
	_, cmd := m.Update(loaded)

	require.NotNil(t, cmd, "a failed fetch warns")
	sessionsDialog, ok := m.dialog.Dialog(dialog.SessionsID).(*dialog.Session)
	require.True(t, ok)
	require.False(t, sessionsDialog.ChildrenLoaded(), "no batch landed")

	// The picker keeps working: ctrl+] reports loading instead of
	// pushing an empty sub-menu, and a retry can still land the tree.
	_, next := m.Update(tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})
	require.NotNil(t, next,
		"ctrl+] with no batch reports Loading rather than pushing a sub-menu")
	require.False(t, sessionsDialog.ChildrenLoaded())
}
