package session

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, fetched.EstimatedUsage)

	fetched.Todos = []Todo{{
		Content:    "Check estimate state",
		Status:     TodoStatusInProgress,
		ActiveForm: "Checking estimate state",
	}}

	updated, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.True(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, refetched.EstimatedUsage)
}

func TestAddCostIncrementsAndPublishesUpdate(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	ctx := t.Context()

	created, err := sessions.Create(ctx, "cost")
	require.NoError(t, err)

	events := sessions.Subscribe(ctx)

	require.NoError(t, sessions.AddCost(ctx, created.ID, 0.10))
	require.NoError(t, sessions.AddCost(ctx, created.ID, 0.05))

	fetched, err := sessions.Get(ctx, created.ID)
	require.NoError(t, err)
	require.InDelta(t, 0.15, fetched.Cost, 1e-9)
	require.Equal(t, "cost", fetched.Title)

	var updatedCost float64
	require.Eventually(t, func() bool {
		select {
		case ev := <-events:
			if ev.Type == pubsub.UpdatedEvent && ev.Payload.Cost > 0.14 {
				updatedCost = ev.Payload.Cost
				return true
			}
		default:
		}
		return false
	}, time.Second, time.Millisecond)
	require.InDelta(t, 0.15, updatedCost, 1e-9)
}

func TestSessionChannelPersists(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "channel")
	require.NoError(t, err)
	updated, err := sessions.SetChannel(t.Context(), created.ID, "signal")
	require.NoError(t, err)
	require.Equal(t, "signal", updated.Channel)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "signal", fetched.Channel)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	saved.EstimatedUsage = false
	updated, err := sessions.Save(t.Context(), saved)
	require.NoError(t, err)
	require.False(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, refetched.EstimatedUsage)
}

func TestListAllChildrenGroupsEveryParent(t *testing.T) {
	// Not parallel: this test and the other pool tests share the global
	// db pool, and ResetPool in one test's cleanup would close another
	// running test's database. Release only this test's entry.
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	ctx := t.Context()

	parentA, err := sessions.Create(ctx, "parent A")
	require.NoError(t, err)
	parentB, err := sessions.Create(ctx, "parent B")
	require.NoError(t, err)

	taskA1, err := sessions.CreateTaskSession(ctx, "call-a1", parentA.ID, "task A1")
	require.NoError(t, err)
	taskA2, err := sessions.CreateTaskSession(ctx, "call-a2", parentA.ID, "task A2")
	require.NoError(t, err)
	taskB1, err := sessions.CreateTaskSession(ctx, "call-b1", parentB.ID, "task B1")
	require.NoError(t, err)
	titleB, err := sessions.CreateTitleSession(ctx, parentB.ID)
	require.NoError(t, err)

	// CreateTaskSession and CreateTitleSession stamp created_at with
	// the wall clock, which is too coarse to pin the ordering, so
	// backdate the children through raw SQL. updated_at is rewritten
	// by the AFTER UPDATE trigger, but the child listing is ordered
	// by created_at, parent_session_id.
	for _, c := range []struct {
		id        string
		createdAt int64
	}{
		{taskA1.ID, 2000},
		{taskA2.ID, 1000},
		{titleB.ID, 8000},
		{taskB1.ID, 9000},
	} {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET created_at = ? WHERE id = ?", c.createdAt, c.id)
		require.NoError(t, err)
	}

	kids, err := sessions.ListAllChildren(ctx)
	require.NoError(t, err)
	require.Len(t, kids, 4, "every child of every parent, no top-level sessions")
	for _, k := range kids {
		require.NotEqual(t, parentA.ID, k.ID, "top-level sessions are not children")
		require.NotEqual(t, parentB.ID, k.ID, "top-level sessions are not children")
	}

	groups := map[string][]string{}
	sequence := []string{}
	for _, k := range kids {
		parent := k.ParentSessionID
		require.NotEmpty(t, parent)
		if len(sequence) == 0 || sequence[len(sequence)-1] != parent {
			sequence = append(sequence, parent)
		}
		groups[parent] = append(groups[parent], k.ID)
	}
	require.Len(t, sequence, 2, "each parent's children must be grouped together")

	require.Equal(t, []string{taskA2.ID, taskA1.ID}, groups[parentA.ID],
		"oldest-created first within the parent")
	require.Equal(t, []string{titleB.ID, taskB1.ID}, groups[parentB.ID],
		"oldest-created first within the parent, title sessions included")
}

// TestGetLastIgnoresChildSessions pins #413: a background dispatch keeps
// updating its task session, so the most recently updated session of any
// kind is usually a child. GetLast must return the most recently
// updated top-level session, or nothing at all.
//
// Not parallel: the other pool tests share the global db pool; Release
// only this test's entry.
func TestGetLastIgnoresChildSessions(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	ctx := t.Context()

	// Insert rows with explicit updated_at through raw SQL: the
	// AFTER UPDATE trigger rewrites updated_at on UPDATE, so an
	// UPDATE cannot pin the ordering.
	insert := func(id string, parent any, updatedAt int64) {
		t.Helper()
		_, err := conn.ExecContext(ctx,
			`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
			 VALUES (?, ?, 'pinned', ?, ?)`, id, parent, updatedAt, updatedAt-100)
		require.NoError(t, err)
	}

	// A parent, then a task session under it updated later.
	insert("parent-1", nil, 1000)
	insert("msg-1$$call-1", "parent-1", 2000)

	last, err := sessions.GetLast(ctx)
	require.NoError(t, err)
	require.Equal(t, "parent-1", last.ID,
		"the newest top-level session wins over a newer child")
}

func TestGetLastWithOnlyChildrenReportsNone(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	ctx := t.Context()

	_, err = conn.ExecContext(ctx,
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES ('msg-1$$call-1', 'parent-1', 'task', 2000, 1900)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`INSERT INTO sessions (id, parent_session_id, title, updated_at, created_at)
		 VALUES ('title-parent-1', 'parent-1', 'title', 3000, 2900)`)
	require.NoError(t, err)

	_, err = sessions.GetLast(ctx)
	require.Error(t, err, "children alone must not be continuable")
}
