package session

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// newChildrenTestService opens a throwaway DB in its own temp dir and
// returns a session service on it. Not parallel: this test and the
// other pool tests share the global db pool, and ResetPool in one
// test's cleanup would close another running test's database. Release
// only this test's entry.
func newChildrenTestService(t *testing.T) Service {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return NewService(db.New(conn), conn)
}

// TestListChildrenReturnsDirectChildrenOnly pins the per-parent child
// listing the sessions picker feeds from (#314, #427): exactly the
// parent's direct children, title sessions included, with neither
// grandchildren nor unrelated top-level sessions. Set membership only;
// ordering belongs to #417.
func TestListChildrenReturnsDirectChildrenOnly(t *testing.T) {
	sessions := newChildrenTestService(t)
	ctx := t.Context()

	parent, err := sessions.Create(ctx, "parent")
	require.NoError(t, err)
	other, err := sessions.Create(ctx, "unrelated")
	require.NoError(t, err)
	task1, err := sessions.CreateTaskSession(ctx, "call-1", parent.ID, "task 1")
	require.NoError(t, err)
	task2, err := sessions.CreateTaskSession(ctx, "call-2", parent.ID, "task 2")
	require.NoError(t, err)
	title, err := sessions.CreateTitleSession(ctx, parent.ID)
	require.NoError(t, err)
	grandchild, err := sessions.CreateTaskSession(ctx, "call-grand", task1.ID, "grandchild")
	require.NoError(t, err)

	kids, err := sessions.ListChildren(ctx, parent.ID)
	require.NoError(t, err)
	got := map[string]Session{}
	for _, k := range kids {
		got[k.ID] = k
	}
	require.Len(t, kids, 3, "exactly the direct children, including the title child")
	for _, id := range []string{task1.ID, task2.ID, title.ID} {
		k, ok := got[id]
		require.True(t, ok, "direct child %s is missing", id)
		require.Equal(t, parent.ID, k.ParentSessionID)
	}
	_, isGrandchild := got[grandchild.ID]
	require.False(t, isGrandchild, "grandchildren are not direct children")
	_, isOther := got[other.ID]
	require.False(t, isOther, "unrelated top-level sessions are not children")
}

// TestListChildrenUnknownAndEmptyParentReturnEmpty pins the unhappy
// paths (#427): an unknown parent and the empty string both return no
// children with a nil error. The empty string maps to a NULL
// parent_session_id, which must not fall back to listing top-level
// sessions.
func TestListChildrenUnknownAndEmptyParentReturnEmpty(t *testing.T) {
	sessions := newChildrenTestService(t)
	ctx := t.Context()

	parent, err := sessions.Create(ctx, "parent")
	require.NoError(t, err)
	task, err := sessions.CreateTaskSession(ctx, "call-1", parent.ID, "task 1")
	require.NoError(t, err)
	_, err = sessions.Create(ctx, "top-level")
	require.NoError(t, err)
	require.NotEmpty(t, task.ID)

	kids, err := sessions.ListChildren(ctx, "no-such-session")
	require.NoError(t, err)
	require.Empty(t, kids)

	kids, err = sessions.ListChildren(ctx, "")
	require.NoError(t, err)
	require.Empty(t, kids, "an empty parent must not return top-level sessions")
}
