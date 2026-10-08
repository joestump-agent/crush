package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/dispatch"
)

// aliveHosts is a ReconcileOptions.Alive stub: only the named host's
// process is running, so reconcile does not probe real pids.
func aliveHosts(alive ...string) func(string) bool {
	live := make(map[string]bool, len(alive))
	for _, host := range alive {
		live[host] = true
	}
	return func(hostID string) bool { return live[hostID] }
}

// seedTask writes one a2a_tasks row with full control over its host and
// state, bypassing the store's ownership checks exactly the way
// reconcile itself writes.
func seedTask(t *testing.T, q *db.Queries, id, hostID, state string) a2aspec.Task {
	t.Helper()
	task := &a2aspec.Task{
		ID:        a2aspec.TaskID(id),
		ContextID: "ctx-" + id,
		Status:    a2aspec.TaskStatus{State: a2aspec.TaskState(state)},
	}
	data, err := json.Marshal(task)
	require.NoError(t, err)
	require.NoError(t, q.CreateA2ATask(context.Background(), db.CreateA2ATaskParams{
		ID:        id,
		ContextID: task.ContextID,
		User:      "user",
		HostID:    hostID,
		State:     state,
		TaskJson:  string(data),
	}))
	return *task
}

// seedDispatch writes one a2a_dispatches row.
func seedDispatch(t *testing.T, q *db.Queries, p db.UpsertA2ADispatchParams) {
	t.Helper()
	require.NoError(t, q.UpsertA2ADispatch(context.Background(), p))
}

func TestReconcileOrphanedTasks(t *testing.T) {
	database, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(db.ResetPool)
	q := db.New(database)

	working := string(a2aspec.TaskStateWorking)
	seedTask(t, q, "task-dead", "dead-host|1|a", working)
	seedTask(t, q, "task-live", "live-host|2|b", working)
	seedTask(t, q, "task-done", "dead-host|1|a", string(a2aspec.TaskStateCompleted))
	seedTask(t, q, "task-malformed", "nonsense", working)

	seedDispatch(t, q, db.UpsertA2ADispatchParams{
		DispatchID:      "dispatch-dead",
		SessionID:       "agent$$dead",
		ParentSessionID: "parent-1",
		Branch:          "crush-dispatch-dispatch-dead",
		WorkspacePath:   "/ws/dead",
		Status:          "running",
		HostID:          "dead-host|1|a",
	})
	seedDispatch(t, q, db.UpsertA2ADispatchParams{
		DispatchID:    "dispatch-live",
		SessionID:     "agent$$live",
		WorkspacePath: "/ws/live",
		Status:        "running",
		HostID:        "live-host|2|b",
	})
	seedDispatch(t, q, db.UpsertA2ADispatchParams{
		DispatchID:    "dispatch-completed",
		SessionID:     "agent$$done",
		WorkspacePath: "/ws/done",
		Status:        "completed",
		HostID:        "dead-host|1|a",
	})

	report, err := ReconcileOrphanedTasks(t.Context(), database, "live-host|2|b", &ReconcileOptions{
		Alive: aliveHosts("live-host|2|b"),
	})
	require.NoError(t, err)
	require.Equal(t, 2, report.FailedTasks, "the dead-host working tasks plus the malformed-host one should fail")
	require.Equal(t, 1, report.FailedDispatches)

	// The dead-host working task is Failed with the restart message,
	// version bumped.
	row, err := q.GetA2ATask(context.Background(), "task-dead")
	require.NoError(t, err)
	require.Equal(t, string(a2aspec.TaskStateFailed), row.State)
	var failed a2aspec.Task
	require.NoError(t, json.Unmarshal([]byte(row.TaskJson), &failed))
	require.NotNil(t, failed.Status.Message)
	require.Equal(t, reconcileRestartMessage, messageText(failed.Status.Message))
	require.Equal(t, int64(2), row.Version)

	// The malformed-host task is failed too: no live process writes one.
	row, err = q.GetA2ATask(context.Background(), "task-malformed")
	require.NoError(t, err)
	require.Equal(t, string(a2aspec.TaskStateFailed), row.State)

	// The live host's task and the terminal task are untouched.
	row, err = q.GetA2ATask(context.Background(), "task-live")
	require.NoError(t, err)
	require.Equal(t, working, row.State)
	require.Equal(t, int64(1), row.Version)
	row, err = q.GetA2ATask(context.Background(), "task-done")
	require.NoError(t, err)
	require.Equal(t, string(a2aspec.TaskStateCompleted), row.State)

	// The dead host's running dispatch record failed with the
	// workspace-preserving error.
	drow, err := q.GetA2ADispatch(context.Background(), "dispatch-dead")
	require.NoError(t, err)
	require.Equal(t, "failed", drow.Status)
	var result dispatch.DispatchResult
	require.NoError(t, json.Unmarshal([]byte(drow.ResultJson), &result))
	require.Equal(t, dispatch.StatusFailed, result.Status)
	require.Equal(t, "crush restarted; workspace preserved at /ws/dead", result.Error)
	require.Equal(t, "dispatch-dead", result.DispatchID)

	// The live host's record and the terminal record are untouched.
	drow, err = q.GetA2ADispatch(context.Background(), "dispatch-live")
	require.NoError(t, err)
	require.Equal(t, "running", drow.Status)
	drow, err = q.GetA2ADispatch(context.Background(), "dispatch-completed")
	require.NoError(t, err)
	require.Equal(t, "completed", drow.Status)
	require.Equal(t, int64(0), drow.DeliveredAt, "reconcile never stamps delivery")

	// Reconcile is idempotent: a second pass finds nothing to do.
	report, err = ReconcileOrphanedTasks(t.Context(), database, "live-host|2|b", &ReconcileOptions{
		Alive: aliveHosts("live-host|2|b"),
	})
	require.NoError(t, err)
	require.Zero(t, report.FailedTasks)
	require.Zero(t, report.FailedDispatches)
}

// An orphaned external dispatch (#434) has no branch or workspace to
// preserve: reconcile fails it as external, says nothing was written to
// disk, and never points at an empty workspace path.
func TestReconcileOrphanedExternalDispatch(t *testing.T) {
	database, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(db.ResetPool)
	q := db.New(database)

	seedDispatch(t, q, db.UpsertA2ADispatchParams{
		DispatchID:      "dispatch-external",
		SessionID:       "agent$$external",
		ParentSessionID: "parent-1",
		Status:          "running",
		HostID:          "dead-host|1|a",
	})

	report, err := ReconcileOrphanedTasks(t.Context(), database, "live-host|2|b", &ReconcileOptions{
		Alive: aliveHosts("live-host|2|b"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.FailedDispatches)

	drow, err := q.GetA2ADispatch(context.Background(), "dispatch-external")
	require.NoError(t, err)
	var result dispatch.DispatchResult
	require.NoError(t, json.Unmarshal([]byte(drow.ResultJson), &result))
	require.Equal(t, dispatch.StatusFailed, result.Status)
	require.Equal(t, dispatch.SourceExternalUnknown, result.Source)
	require.Empty(t, result.WorkspacePath)
	require.NotContains(t, result.Error, "workspace preserved")
	require.Contains(t, result.Error, "external agent")

	msg := result.TerminalMessage()
	require.True(t, strings.HasPrefix(msg, dispatch.ExternalResultNotice+" the result below came from an external agent, not from crush."), msg)
}
