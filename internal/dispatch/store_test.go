package dispatch

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/db"
)

// newTestRecordStore opens a fresh session database in a temp dir and
// returns a record store over it owned by the given host ID.
func newTestRecordStore(t *testing.T, hostID string) *SQLiteRecordStore {
	t.Helper()
	database, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(db.ResetPool)
	return NewSQLiteRecordStore(database, hostID)
}

func TestRecordStoreLifecycle(t *testing.T) {
	store := newTestRecordStore(t, "host|1|abc")

	started := DispatchResult{
		DispatchID:    "dispatch-1",
		Handle:        "tester",
		Branch:        "crush-dispatch-dispatch-1",
		WorkspacePath: "/tmp/crush-dispatch-dispatch-1",
		SessionID:     "agent$$session-1",
		Status:        StatusRunning,
	}
	require.NoError(t, store.RecordStarted(started, "parent-1"))

	// No task yet; snapshotting by session returns the running row.
	_, status, ok := store.SnapshotRecord("agent$$session-1")
	require.True(t, ok)
	require.Equal(t, StatusRunning, status)

	require.NoError(t, store.RecordTask("dispatch-1", "task-1"))

	terminal := started
	terminal.Status = StatusCompleted
	terminal.KeyFindings = "Added validation and two tests."
	require.NoError(t, store.RecordTerminal(terminal))

	// The record is terminal and undelivered before the parent's
	// delivery turn lands.
	undelivered, err := store.UndeliveredTerminal()
	require.NoError(t, err)
	require.Len(t, undelivered, 1)
	require.Equal(t, "dispatch-1", undelivered[0].Result.DispatchID)
	require.Equal(t, StatusCompleted, undelivered[0].Result.Status)
	require.Equal(t, "Added validation and two tests.", undelivered[0].Result.KeyFindings)
	require.Equal(t, "parent-1", undelivered[0].ParentSessionID)

	require.NoError(t, store.RecordDelivered("dispatch-1"))
	undelivered, err = store.UndeliveredTerminal()
	require.NoError(t, err)
	require.Empty(t, undelivered)

	// The session snapshot now carries the terminal payload.
	result, status, ok := store.SnapshotRecord("agent$$session-1")
	require.True(t, ok)
	require.Equal(t, StatusCompleted, status)
	require.Equal(t, "Added validation and two tests.", result.KeyFindings)
}

func TestRecordStoreTerminalOnlyFromRunning(t *testing.T) {
	store := newTestRecordStore(t, "host|1|abc")

	started := DispatchResult{
		DispatchID: "dispatch-1",
		SessionID:  "agent$$session-1",
		Status:     StatusRunning,
	}
	require.NoError(t, store.RecordStarted(started, "parent-1"))

	// A failed delivery is re-pended in memory, never re-terminalized:
	// the row's status does not change once terminal.
	terminal := started
	terminal.Status = StatusKilled
	terminal.KilledReason = ReasonCanceled
	require.NoError(t, store.RecordTerminal(terminal))
	require.Error(t, store.RecordTerminal(terminal))

	undelivered, err := store.UndeliveredTerminal()
	require.NoError(t, err)
	require.Len(t, undelivered, 1)
	require.Equal(t, ReasonCanceled, undelivered[0].Result.KilledReason)
}

func TestRecordStoreUnknownSession(t *testing.T) {
	store := newTestRecordStore(t, "host|1|abc")

	_, _, ok := store.SnapshotRecord("agent$$missing")
	require.False(t, ok)

	undelivered, err := store.UndeliveredTerminal()
	require.NoError(t, err)
	require.Empty(t, undelivered)
}

func TestRecordStoreDeliveredStampVisibleInRecord(t *testing.T) {
	store := newTestRecordStore(t, "host|1|abc")

	started := DispatchResult{
		DispatchID: "dispatch-1",
		SessionID:  "agent$$session-1",
		Status:     StatusRunning,
	}
	require.NoError(t, store.RecordStarted(started, "parent-1"))
	require.NoError(t, store.RecordDelivered("dispatch-1"))

	// A delivered stamp on a non-terminal row changes nothing the
	// redelivery list cares about, and the row stays non-terminal.
	undelivered, err := store.UndeliveredTerminal()
	require.NoError(t, err)
	require.Empty(t, undelivered)
}
