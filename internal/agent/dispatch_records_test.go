package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/dispatch"
)

// fakeRecordStore stands in for the coordinator's durable dispatch
// records (#355): it answers the redelivery list and the session
// snapshots from seeded memory, and records the stamps it is given.
type fakeRecordStore struct {
	mu sync.Mutex
	// undelivered is what UndeliveredTerminal answers.
	undelivered []dispatch.UndeliveredDispatch
	// delivered records every RecordDelivered dispatch ID in order.
	delivered []string
	// snapshots answers SnapshotRecord by dispatched session ID.
	snapshots map[string]dispatch.DispatchResult
	// errors, when set, fails every read with it.
	err error
}

func (f *fakeRecordStore) RecordStarted(dispatch.DispatchResult, string) error { return nil }

func (f *fakeRecordStore) RecordTask(_, _ string) error { return nil }

func (f *fakeRecordStore) RecordTerminal(dispatch.DispatchResult) error { return nil }

func (f *fakeRecordStore) RecordDelivered(dispatchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, dispatchID)
	return nil
}

func (f *fakeRecordStore) UndeliveredTerminal() ([]dispatch.UndeliveredDispatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]dispatch.UndeliveredDispatch(nil), f.undelivered...), nil
}

func (f *fakeRecordStore) SnapshotRecord(sessionID string) (dispatch.DispatchResult, dispatch.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result, ok := f.snapshots[sessionID]
	if !ok {
		return dispatch.DispatchResult{}, "", false
	}
	return result, result.Status, true
}

func TestReconcileDispatchDeliveries(t *testing.T) {
	c, main, parentID := newDeliveryEnv(t)
	records := &fakeRecordStore{
		undelivered: []dispatch.UndeliveredDispatch{
			{
				ParentSessionID: parentID,
				Result: dispatch.DispatchResult{
					DispatchID:  "d-alive",
					Branch:      "crush-dispatch-d-alive",
					SessionID:   "s-alive",
					Status:      dispatch.StatusCompleted,
					KeyFindings: "finished before the crash",
				},
			},
			{
				ParentSessionID: "gone-parent",
				Result: dispatch.DispatchResult{
					DispatchID: "d-gone",
					Status:     dispatch.StatusCompleted,
				},
			},
		},
		snapshots: map[string]dispatch.DispatchResult{},
	}
	c.dispatchRecords = records

	c.ReconcileDispatchDeliveries(t.Context())

	// The surviving parent got exactly one delivery turn carrying the
	// undelivered payload; the gone parent's payload was dropped for
	// good, stamped so no restart retries it.
	require.Eventually(t, func() bool {
		return main.runCount() == 1
	}, 10*time.Second, 50*time.Millisecond)
	run := main.lastRun()
	require.Equal(t, parentID, run.SessionID)
	require.True(t, run.HiddenUserMessage)
	require.Contains(t, run.Prompt, `"dispatch_id": "d-alive"`)
	require.Contains(t, run.Prompt, `"key_findings": "finished before the crash"`)

	require.Eventually(t, func() bool {
		records.mu.Lock()
		defer records.mu.Unlock()
		return len(records.delivered) == 2
	}, 10*time.Second, 50*time.Millisecond)
	records.mu.Lock()
	defer records.mu.Unlock()
	require.ElementsMatch(t, []string{"d-alive", "d-gone"}, records.delivered)
}

func TestReconcileDispatchDeliveriesNilRecords(t *testing.T) {
	c, main, _ := newDeliveryEnv(t)
	c.dispatchRecords = nil

	c.ReconcileDispatchDeliveries(t.Context())
	require.Zero(t, main.runCount())
}

// The UI seed (#355): with no collector snapshot — a restart emptied
// the in-memory registry — DispatchStatus falls back to the stored
// terminal record, so the block renders failed or completed instead of
// a forever-working stale handle. A session with no record answers
// not-ok, keeping the static stale-handle behavior.
func TestDispatchStatusFallsBackToStoredRecord(t *testing.T) {
	c, _, _ := newDeliveryEnv(t)
	c.dispatchRecords = &fakeRecordStore{
		snapshots: map[string]dispatch.DispatchResult{
			"s-crashed": {
				DispatchID:    "d-crashed",
				Branch:        "crush-dispatch-d-crashed",
				WorkspacePath: "/ws/crashed",
				SessionID:     "s-crashed",
				Status:        dispatch.StatusFailed,
				Error:         "crush restarted; workspace preserved at /ws/crashed",
			},
		},
	}

	snapshot, ok := c.DispatchStatus("s-crashed")
	require.True(t, ok)
	require.Equal(t, dispatch.StatusFailed, snapshot.Entry.Status)
	require.NotNil(t, snapshot.Entry.Result)
	require.Equal(t, "d-crashed", snapshot.Entry.Result.DispatchID)
	require.Contains(t, snapshot.Entry.Result.Error, "crush restarted")

	_, ok = c.DispatchStatus("s-unknown")
	require.False(t, ok)
}
