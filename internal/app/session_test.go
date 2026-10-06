package app

import (
	"testing"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// liveDispatchCoordinator is a minimal agent.Coordinator whose DispatchLive
// reports a running dispatch only for the sessions in liveFor (#418).
type liveDispatchCoordinator struct {
	agent.Coordinator
	liveFor map[string][]dispatch.TodoSnapshot
}

func (c *liveDispatchCoordinator) DispatchLive(sessionID string) []dispatch.TodoSnapshot {
	return c.liveFor[sessionID]
}

// newDeleteSessionApp builds an App with a real session service over a
// temporary database and the given coordinator, ready for DeleteSession
// tests (#418).
func newDeleteSessionApp(t *testing.T, liveFor map[string][]dispatch.TodoSnapshot) *App {
	t.Helper()
	app := NewForTest(t.Context())
	t.Cleanup(app.ShutdownForTest)

	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	app.Sessions = session.NewService(db.New(conn), conn)
	app.AgentCoordinator = &liveDispatchCoordinator{liveFor: liveFor}
	return app
}

func newDeleteSessionTree(t *testing.T, app *App) (parent session.Session, child, grand session.Session) {
	t.Helper()
	ctx := t.Context()
	var err error
	parent, err = app.Sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err = app.Sessions.CreateTaskSession(ctx, "call-1", parent.ID, "task 1")
	require.NoError(t, err)
	grand, err = app.Sessions.CreateTaskSession(ctx, "call-2", child.ID, "grandchild")
	require.NoError(t, err)
	return parent, child, grand
}

func TestDeleteSessionRefusesWhileDispatchRunning(t *testing.T) {
	// A dispatch created from the parent, still running on the child's
	// task session: deleting the parent must be refused and delete
	// nothing (#418).
	app := newDeleteSessionApp(t, nil)
	parent, child, _ := newDeleteSessionTree(t, app)
	app.AgentCoordinator = &liveDispatchCoordinator{liveFor: map[string][]dispatch.TodoSnapshot{
		parent.ID: {{Entry: dispatch.Entry{
			SessionID:       child.ID,
			ParentSessionID: parent.ID,
		}}},
	}}

	err := app.DeleteSession(t.Context(), parent.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "an agent dispatched from this session is still running; cancel it first")

	for _, id := range []string{parent.ID, child.ID} {
		_, err := app.Sessions.Get(t.Context(), id)
		require.NoError(t, err, "session %s must survive a refused delete", id)
	}
}

func TestDeleteSessionRefusesWhileDescendantDispatchRunning(t *testing.T) {
	// A dispatch created from a descendant, still running on the
	// grandchild's task session: the guard must reach every depth
	// (#418).
	app := newDeleteSessionApp(t, nil)
	parent, child, grand := newDeleteSessionTree(t, app)
	app.AgentCoordinator = &liveDispatchCoordinator{liveFor: map[string][]dispatch.TodoSnapshot{
		child.ID: {{Entry: dispatch.Entry{
			SessionID:       grand.ID,
			ParentSessionID: child.ID,
		}}},
	}}

	err := app.DeleteSession(t.Context(), parent.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "an agent dispatched from this session is still running; cancel it first")

	for _, id := range []string{parent.ID, child.ID, grand.ID} {
		_, err := app.Sessions.Get(t.Context(), id)
		require.NoError(t, err, "session %s must survive a refused delete", id)
	}
}

func TestDeleteSessionDeletesTreeWithoutLiveDispatch(t *testing.T) {
	app := newDeleteSessionApp(t, nil)
	parent, child, grand := newDeleteSessionTree(t, app)

	require.NoError(t, app.DeleteSession(t.Context(), parent.ID))

	for _, id := range []string{parent.ID, child.ID, grand.ID} {
		_, err := app.Sessions.Get(t.Context(), id)
		require.Error(t, err, "session %s must be deleted", id)
	}
}
