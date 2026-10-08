package a2a

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/dispatch"
)

// reconcileRestartMessage is the status message an orphaned served
// task is failed with, and the error text an orphaned dispatch record
// carries. The workspace is never removed by reconcile (#355): the
// pointer in the message is where the work still lives.
const reconcileRestartMessage = "crush restarted"

// ReconcileReport counts what one startup reconcile pass did.
type ReconcileReport struct {
	// FailedTasks is the number of served A2A tasks moved to Failed
	// because their host process is gone.
	FailedTasks int
	// FailedDispatches is the number of dispatch records failed with a
	// restart error because their host process is gone.
	FailedDispatches int
}

// ReconcileOptions customizes [ReconcileOrphanedTasks]. Zero fields
// fall back to the production defaults.
type ReconcileOptions struct {
	// Alive decides whether a host ID's process is still running.
	// Defaults to [ProcessAlive]; tests inject a stub so reconcile does
	// not probe real pids.
	Alive func(hostID string) bool
	// Now sources the updated_at clock. Defaults to time.Now.
	Now func() time.Time
}

// ReconcileOrphanedTasks closes out the durable A2A rows a crashed
// process left behind (#355): every non-terminal a2a_tasks row and
// every running a2a_dispatches row whose host_id names a process that
// is not alive is failed in place — the task with a "crush restarted"
// status message, the dispatch record with an error naming the
// preserved workspace — before the UI loads any session. Rows owned by
// the current process, and rows whose host is still alive, are left
// untouched. Workspaces are never removed here.
//
// It is best-effort by design: a row that cannot be decoded or written
// is logged and skipped, because reconcile runs inside startup and a
// single bad row must not keep crush from starting.
func ReconcileOrphanedTasks(ctx context.Context, database *sql.DB, currentHostID string, opts *ReconcileOptions) (ReconcileReport, error) {
	if opts == nil {
		opts = &ReconcileOptions{}
	}
	alive := opts.Alive
	if alive == nil {
		alive = ProcessAlive
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	report := ReconcileReport{}
	q := db.New(database)
	// Detached: reconcile runs during startup; a canceling parent must
	// not leave a half-reconciled store behind.
	ctx = context.WithoutCancel(ctx)

	tasks, err := q.ListNonTerminalA2ATasks(ctx)
	if err != nil {
		return report, fmt.Errorf("a2a reconcile: list non-terminal tasks: %w", err)
	}
	for _, row := range tasks {
		if row.HostID == currentHostID || alive(row.HostID) {
			continue
		}
		task, err := decodeStoredTask(row.TaskJson)
		if err != nil {
			slog.Warn("A2A reconcile skipped an undecodable orphaned task", "task_id", row.ID, "error", err)
			continue
		}
		task.Status.State = a2aspec.TaskStateFailed
		msg := reconcileRestartMessage
		task.Status.Message = a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, task, a2aspec.NewTextPart(msg))
		data, err := json.Marshal(task)
		if err != nil {
			slog.Warn("A2A reconcile skipped an orphaned task that failed to encode", "task_id", row.ID, "error", err)
			continue
		}
		if _, err := q.UpdateA2ATask(ctx, db.UpdateA2ATaskParams{
			ID:        row.ID,
			ContextID: task.ContextID,
			State:     string(a2aspec.TaskStateFailed),
			TaskJson:  string(data),
			UpdatedAt: now().UnixNano(),
		}); err != nil {
			slog.Warn("A2A reconcile failed to persist an orphaned task's terminal state", "task_id", row.ID, "error", err)
			continue
		}
		report.FailedTasks++
	}

	dispatches, err := q.ListNonTerminalA2ADispatches(ctx)
	if err != nil {
		return report, fmt.Errorf("a2a reconcile: list non-terminal dispatches: %w", err)
	}
	for _, row := range dispatches {
		if row.HostID == currentHostID || alive(row.HostID) {
			continue
		}
		result := dispatch.DispatchResult{
			DispatchID:    row.DispatchID,
			Handle:        row.Handle,
			Branch:        row.Branch,
			WorkspacePath: row.WorkspacePath,
			SessionID:     row.SessionID,
			Status:        dispatch.StatusFailed,
			Error:         fmt.Sprintf("%s; workspace preserved at %s", reconcileRestartMessage, row.WorkspacePath),
		}
		if row.Branch == "" && row.WorkspacePath == "" {
			// An external dispatch (#434) records neither: nothing is
			// preserved on disk, and the row does not keep the card URL,
			// so the result is marked external without naming it.
			result.Source = dispatch.SourceExternalUnknown
			result.Error = reconcileRestartMessage + "; the external agent's run was abandoned with the process, and nothing was written to disk"
		}
		data, err := json.Marshal(result)
		if err != nil {
			slog.Warn("A2A reconcile skipped an orphaned dispatch that failed to encode", "dispatch_id", row.DispatchID, "error", err)
			continue
		}
		rows, err := q.FailA2ADispatch(ctx, db.FailA2ADispatchParams{
			DispatchID: row.DispatchID,
			ResultJson: string(data),
			UpdatedAt:  now().UnixNano(),
		})
		if err != nil {
			slog.Warn("A2A reconcile failed to persist an orphaned dispatch's terminal state", "dispatch_id", row.DispatchID, "error", err)
			continue
		}
		if rows > 0 {
			report.FailedDispatches++
		}
	}
	return report, nil
}
