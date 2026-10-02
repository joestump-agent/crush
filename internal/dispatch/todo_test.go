package dispatch

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// recordingSink records every snapshot it receives, with a buffered
// channel so tests can await delivery deterministically.
type recordingSink struct {
	snapshots chan TodoSnapshot
}

func newRecordingSink() *recordingSink {
	return &recordingSink{snapshots: make(chan TodoSnapshot, 64)}
}

func (s *recordingSink) DispatchTodos(snap TodoSnapshot) {
	select {
	case s.snapshots <- snap:
	default:
	}
}

// next returns the next snapshot or fails the test after a timeout.
func (s *recordingSink) next(t *testing.T) TodoSnapshot {
	t.Helper()
	select {
	case snap := <-s.snapshots:
		return snap
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a snapshot")
		return TodoSnapshot{}
	}
}

// until returns the first snapshot matching cond, skipping snapshots
// the collector emitted for earlier transitions.
func (s *recordingSink) until(t *testing.T, cond func(TodoSnapshot) bool) TodoSnapshot {
	t.Helper()
	for {
		snap := s.next(t)
		if cond(snap) {
			return snap
		}
	}
}

func testTodos() []session.Todo {
	return []session.Todo{
		{Content: "read the code", Status: session.TodoStatusCompleted},
		{Content: "write the fix", ActiveForm: "writing the fix", Status: session.TodoStatusInProgress},
		{Content: "add tests", Status: session.TodoStatusPending},
	}
}

// Reduce must pick the in-progress todo as the current activity,
// preferring its active form, and carry the counts and usage.
func TestReduce(t *testing.T) {
	entry := Entry{
		ID:        "dispatch-1",
		SessionID: "msg$$call",
		Status:    StatusRunning,
		StartedAt: time.Now().Add(-2 * time.Minute),
	}
	sess := session.Session{
		ID:               "msg$$call",
		Todos:            testTodos(),
		PromptTokens:     1500,
		CompletionTokens: 500,
		Cost:             0.25,
	}

	snap := Reduce(entry, sess)

	require.Equal(t, "writing the fix", snap.CurrentTodo)
	require.Equal(t, 1, snap.TodoCompleted)
	require.Equal(t, 3, snap.TodoTotal)
	require.Equal(t, int64(1500), snap.PromptTokens)
	require.Equal(t, int64(500), snap.CompletionTokens)
	require.Equal(t, 0.25, snap.Cost)
	require.Equal(t, entry, snap.Entry)
}

// Without an active form the current todo falls back to its content.
func TestReduceCurrentTodoFallsBackToContent(t *testing.T) {
	sess := session.Session{
		Todos: []session.Todo{
			{Content: "write the fix", Status: session.TodoStatusInProgress},
		},
	}
	snap := Reduce(Entry{Status: StatusRunning}, sess)
	require.Equal(t, "write the fix", snap.CurrentTodo)
}

// A session with no in-progress todo has no current activity.
func TestReduceNoInProgressTodo(t *testing.T) {
	sess := session.Session{
		Todos: []session.Todo{
			{Content: "done", Status: session.TodoStatusCompleted},
		},
	}
	snap := Reduce(Entry{Status: StatusRunning}, sess)
	require.Empty(t, snap.CurrentTodo)
}

// The collector is the reduction both sinks consume: session events and
// registry transitions each produce snapshots, and every sink receives
// the same ones.
func TestTodoCollectorDualSink(t *testing.T) {
	ws, err := NewWorkspace(newTestRepo(t))
	require.NoError(t, err)

	sessions := pubsub.NewBroker[session.Session]()
	defer sessions.Shutdown()
	sinkA, sinkB := newRecordingSink(), newRecordingSink()
	collector := NewTodoCollector(ws, sessions, sinkA, sinkB)
	go collector.Run(t.Context())

	entry, err := ws.Provision(t.Context(), ProvisionOptions{})
	require.NoError(t, err)

	// The dispatched session is recorded before the run starts; the
	// registry transition alone must emit a snapshot with the state.
	ws.SetSession(entry.ID, "msg$$call")
	snap := sinkA.next(t)
	require.Equal(t, StatusProvisioned, snap.Entry.Status)
	require.Equal(t, "msg$$call", snap.Entry.SessionID)

	ws.SetStatus(entry.ID, StatusRunning)
	snap = sinkA.next(t)
	require.Equal(t, StatusRunning, snap.Entry.Status)
	require.False(t, snap.Entry.StartedAt.IsZero())

	// A session save carrying todos produces the reduced snapshot: same
	// delivery to the second sink.
	sessions.Publish(pubsub.UpdatedEvent, session.Session{
		ID:               "msg$$call",
		Todos:            testTodos(),
		PromptTokens:     1200,
		CompletionTokens: 300,
	})
	snap = sinkB.until(t, func(s TodoSnapshot) bool { return s.TodoTotal == 3 })
	require.Equal(t, StatusRunning, snap.Entry.Status)
	require.Equal(t, "writing the fix", snap.CurrentTodo)
	require.Equal(t, 3, snap.TodoTotal)
	require.Equal(t, int64(1200), snap.PromptTokens)
	require.Equal(t, int64(300), snap.CompletionTokens)

	// Terminal transition keeps the reduced todos and carries the
	// terminal payload.
	terminal := DispatchResult{
		DispatchID:  entry.ID,
		Status:      StatusCompleted,
		KeyFindings: "done",
	}
	ws.SetResult(entry.ID, terminal)
	ws.SetStatus(entry.ID, StatusCompleted)
	snap = sinkB.until(t, func(s TodoSnapshot) bool { return s.Entry.Status == StatusCompleted })
	require.Equal(t, StatusCompleted, snap.Entry.Status)
	require.False(t, snap.Entry.FinishedAt.IsZero())
	require.NotNil(t, snap.Entry.Result)
	require.Equal(t, "done", snap.Entry.Result.KeyFindings)
	require.Equal(t, "writing the fix", snap.CurrentTodo)
}

// Session events for sessions the registry does not know are not
// dispatched work and must not reach the sinks.
func TestTodoCollectorIgnoresUnknownSessions(t *testing.T) {
	ws, err := NewWorkspace(newTestRepo(t))
	require.NoError(t, err)

	sessions := pubsub.NewBroker[session.Session]()
	defer sessions.Shutdown()
	sink := newRecordingSink()
	collector := NewTodoCollector(ws, sessions, sink)

	sessions.Publish(pubsub.UpdatedEvent, session.Session{
		ID:    "not-a-dispatch",
		Todos: testTodos(),
	})
	collector.processSessionEvent(pubsub.Event[session.Session]{
		Type:    pubsub.UpdatedEvent,
		Payload: session.Session{ID: "not-a-dispatch", Todos: testTodos()},
	})
	require.Empty(t, sink.snapshots)
}

// Snapshot answers a pull for a dispatched session even when the last
// event was a registry transition, composing the entry with the last
// reduced session state.
func TestTodoCollectorSnapshotPull(t *testing.T) {
	ws, err := NewWorkspace(newTestRepo(t))
	require.NoError(t, err)

	sessions := pubsub.NewBroker[session.Session]()
	collector := NewTodoCollector(ws, sessions)

	entry, err := ws.Provision(t.Context(), ProvisionOptions{})
	require.NoError(t, err)
	ws.SetSession(entry.ID, "msg$$call")

	collector.processSessionEvent(pubsub.Event[session.Session]{
		Type: pubsub.UpdatedEvent,
		Payload: session.Session{
			ID:    "msg$$call",
			Todos: testTodos(),
		},
	})
	ws.SetStatus(entry.ID, StatusRunning)

	snap, ok := collector.Snapshot("msg$$call")
	require.True(t, ok)
	require.Equal(t, StatusRunning, snap.Entry.Status)
	require.Equal(t, "writing the fix", snap.CurrentTodo)
	require.Equal(t, 3, snap.TodoTotal)

	_, ok = collector.Snapshot("unknown")
	require.False(t, ok)
}
