package dialog

// Tests for the sessions picker tree (#314): inspectable sub-agent task
// sessions nest under their parent through the sub-menu pattern (ctrl+]),
// and selecting one hands the task session to the caller (which opens it
// in inspect mode).
//
// Assertions are on dialog state (item counts, IDs, returned actions),
// never on rendered strings, so they hold on Windows terminals too.

import (
	"context"
	"fmt"
	"image"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/common"

	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/workspace"
)

// sessionsTreeWorkspace serves a fixed parent/child session layout.
type sessionsTreeWorkspace struct {
	workspace.Workspace

	parents  []session.Session
	children map[string][]session.Session
}

func (w *sessionsTreeWorkspace) ListSessions(context.Context) ([]session.Session, error) {
	return w.parents, nil
}

func (w *sessionsTreeWorkspace) ListChildSessions(_ context.Context, parentID string) ([]session.Session, error) {
	return w.children[parentID], nil
}

func (w *sessionsTreeWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	for i := 0; i+1 < len(sessionID); i++ {
		if sessionID[i] == '$' && sessionID[i+1] == '$' {
			return sessionID[:i], sessionID[i+2:], true
		}
	}
	return "", "", false
}

func newSessionsTreeDialog(t *testing.T, ws *sessionsTreeWorkspace) *Session {
	t.Helper()
	sty := styles.CharmtonePantera()
	dialog, err := NewSessions(&common.Common{
		Workspace: ws,
		Styles:    &sty,
	}, "")
	require.NoError(t, err)
	return dialog
}

func treeTestWorkspace() *sessionsTreeWorkspace {
	ws := &sessionsTreeWorkspace{
		parents: []session.Session{
			{ID: "p1", Title: "Parent One"},
			{ID: "p2", Title: "Parent Two"},
		},
		children: map[string][]session.Session{
			"p1": {
				{ID: "m1$$t1", ParentSessionID: "p1", Title: "Dispatched Agent"},
				// A title-generation helper session: never inspectable.
				{ID: "title-p1", ParentSessionID: "p1", Title: "Generate a title"},
			},
		},
	}
	return ws
}

// TestSessionsTreeNestsInspectableChildren pins the tree: only
// agent-tool task sessions nest under a parent, the parent row advertises
// the count, and parents without children stay plain rows.
func TestSessionsTreeNestsInspectableChildren(t *testing.T) {
	ws := treeTestWorkspace()
	dialog := newSessionsTreeDialog(t, ws)

	require.Len(t, dialog.children["p1"], 1,
		"only the agent-tool task session is nestable; title sessions are excluded")
	require.Empty(t, dialog.children["p2"])

	items := dialog.list.FilteredItems()
	require.Len(t, items, 2)
	p1, ok := items[0].(*SessionItem)
	require.True(t, ok)
	require.Equal(t, "p1", p1.ID())
	require.Equal(t, 1, p1.agentCount, "the parent row advertises its sub-agent count")

	p2, ok := items[1].(*SessionItem)
	require.True(t, ok)
	require.Zero(t, p2.agentCount)
	require.False(t, p2.child)
}

// TestSessionsTreeSubMenu pins the navigation: enter opens a session
// whether or not it has children; ctrl+] on a parent with children
// pushes a sub-menu of its task sessions; enter on a child returns the
// child for inspect mode; esc pops back to the parent list.
func TestSessionsTreeSubMenu(t *testing.T) {
	ws := treeTestWorkspace()
	dialog := newSessionsTreeDialog(t, ws)

	// Enter on a parent with children still opens the parent: a session
	// that ran sub-agents must stay one keypress away.
	dialog.list.SetSelected(0)
	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	sel, ok := action.(ActionSelectSession)
	require.True(t, ok, "enter on a parent with children must open the parent")
	require.Equal(t, "p1", sel.Session.ID)
	require.False(t, dialog.inSubMenu())

	// ctrl+] pushes its sub-menu.
	action = dialog.HandleMsg(ctrlKey(t, ']'))
	require.Nil(t, action)
	require.True(t, dialog.inSubMenu())
	require.Equal(t, []string{"Parent One"}, dialog.breadcrumb)

	kids := dialog.list.FilteredItems()
	require.Len(t, kids, 1)
	child, ok := kids[0].(*SessionItem)
	require.True(t, ok)
	require.Equal(t, "m1$$t1", child.ID())
	require.True(t, child.child, "child rows are marked for nested rendering")

	// Enter on the child returns it; the caller decides the mode and a
	// task session can only ever reach inspect.
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, action)
	sel, ok = action.(ActionSelectSession)
	require.True(t, ok)
	require.Equal(t, "m1$$t1", sel.Session.ID)
	require.Equal(t, "p1", sel.Session.ParentSessionID)

	// Esc pops back to the parent list.
	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, dialog.inSubMenu())
	require.Len(t, dialog.list.FilteredItems(), 2)

	// ctrl+] on a childless parent reports instead of pushing an empty
	// sub-menu.
	dialog.list.SetSelected(1)
	action = dialog.HandleMsg(ctrlKey(t, ']'))
	_, ok = action.(ActionCmd)
	require.True(t, ok)
	require.False(t, dialog.inSubMenu())
}

// TestSessionsTreeRenameDeleteGuardedAtTopLevel pins that rename and
// delete stay top-level operations: inside a sub-menu they do nothing
// rather than acting on a task session row.
func TestSessionsTreeRenameDeleteGuardedAtTopLevel(t *testing.T) {
	ws := treeTestWorkspace()
	dialog := newSessionsTreeDialog(t, ws)

	dialog.list.SetSelected(0)
	dialog.HandleMsg(ctrlKey(t, ']'))
	require.True(t, dialog.inSubMenu())

	dialog.HandleMsg(ctrlKey(t, 'r'))
	require.False(t, dialog.sessionsMode == sessionsModeUpdating,
		"rename must not engage inside a sub-menu")
	dialog.HandleMsg(ctrlKey(t, 'x'))
	require.False(t, dialog.sessionsMode == sessionsModeDeleting,
		"delete must not engage inside a sub-menu")

	// Esc pops the sub-menu; at the top level the same keys engage.
	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, dialog.inSubMenu())
	dialog.HandleMsg(ctrlKey(t, 'r'))
	require.True(t, dialog.sessionsMode == sessionsModeUpdating)
}

func ctrlKey(t *testing.T, r rune) tea.KeyPressMsg {
	t.Helper()
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

// TestSessionsTreeKeepsInfoColumn pins that advertising a sub-agent count
// does not cost the picker its timestamps: the info column hides for every
// row once its widest entry crowds the title, so the count must stay
// compact enough to fit beside an old timestamp at the default width.
func TestSessionsTreeKeepsInfoColumn(t *testing.T) {
	ws := treeTestWorkspace()
	old := time.Now().AddDate(0, -11, 0).Unix()
	ws.parents[0].UpdatedAt = old
	ws.parents[1].UpdatedAt = old
	kids := make([]session.Session, 0, 120)
	for i := range 120 {
		kids = append(kids, session.Session{ID: fmt.Sprintf("m%d$$t", i), ParentSessionID: "p1"})
	}
	ws.children["p1"] = kids
	dialog := newSessionsTreeDialog(t, ws)

	scr := uv.NewScreenBuffer(120, 40)
	dialog.Draw(scr, image.Rect(0, 0, 120, 40))

	for _, item := range dialog.list.FilteredItems() {
		row, ok := item.(*SessionItem)
		require.True(t, ok)
		require.False(t, row.hideInfo, "row %s lost its info column", row.ID())
	}
}

// TestSessionsTreeHelpShowsAgentsWhenRelevant pins the ctrl+] hint: the
// short help line truncates, so the hint leads the optional bindings
// only while the selected row has sub-agents, and is absent otherwise.
func TestSessionsTreeHelpShowsAgentsWhenRelevant(t *testing.T) {
	ws := treeTestWorkspace()
	dialog := newSessionsTreeDialog(t, ws)

	dialog.list.SetSelected(0)
	help := dialog.ShortHelp()
	require.Contains(t, help[:3], dialog.keyMap.Agents,
		"a parent with sub-agents shows ctrl+] ahead of the truncation point")

	dialog.list.SetSelected(1)
	require.NotContains(t, dialog.ShortHelp(), dialog.keyMap.Agents)
}
