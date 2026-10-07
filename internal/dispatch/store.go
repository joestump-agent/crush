package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// DispatchRecords is the durable record of dispatches (#355): one row
// in the a2a_dispatches table per dispatch, written at start, updated
// with the terminal DispatchResult, and stamped delivered when the
// parent's delivery turn succeeds. It is what lets a restart fail
// orphaned runs and re-deliver finished-but-undelivered work, and what
// backs the UI seed for a block whose run never reached its terminal
// write.
//
// The agent package holds this interface — never the concrete store —
// so the dependency stays one-way: internal/dispatch may import
// internal/db (the store is its only reason to), internal/agent may
// import internal/dispatch, and every write is best-effort at the call
// sites: a failed record must never break a live dispatch.
type DispatchRecords interface {
	// RecordStarted writes the running row at dispatch start,
	// upserting on the dispatch ID. parentSessionID is the session the
	// dispatch was created from; delivery and reconcile are keyed on it.
	RecordStarted(result DispatchResult, parentSessionID string) error
	// RecordTask stamps the A2A task ID the served stream assigned
	// (#349) onto the row.
	RecordTask(dispatchID, taskID string) error
	// RecordTerminal writes the terminal result and status. Rows
	// reconcile already failed (status no longer running) are left
	// alone.
	RecordTerminal(result DispatchResult) error
	// RecordDelivered stamps the parent-delivery timestamp, proving the
	// terminal payload reached the parent session exactly once (#355).
	RecordDelivered(dispatchID string) error
	// UndeliveredTerminal returns every terminal record whose payload
	// was never delivered to its parent — the restart redelivery list.
	UndeliveredTerminal() ([]UndeliveredDispatch, error)
	// SnapshotRecord returns the row for the dispatched agent running
	// on sessionID, whatever its state. The bool is false when no row
	// exists or it cannot be read.
	SnapshotRecord(sessionID string) (DispatchResult, Status, bool)
}

// UndeliveredDispatch is one terminal record whose payload never
// reached its parent session (#355): the restart redelivery unit.
type UndeliveredDispatch struct {
	// ParentSessionID is the session the dispatch was created from —
	// the delivery target.
	ParentSessionID string
	// Result is the terminal payload to deliver.
	Result DispatchResult
}

// SQLiteRecordStore is the production [DispatchRecords] over the
// session database's a2a_dispatches table. hostID is stamped on every
// write so startup reconcile (#355) can tell this process's rows from
// a dead process's.
type SQLiteRecordStore struct {
	q      *db.Queries
	hostID string
}

var _ DispatchRecords = (*SQLiteRecordStore)(nil)

// NewSQLiteRecordStore builds the record store over an open session
// database.
func NewSQLiteRecordStore(database *sql.DB, hostID string) *SQLiteRecordStore {
	return &SQLiteRecordStore{q: db.New(database), hostID: hostID}
}

// RecordStarted implements [DispatchRecords].
func (s *SQLiteRecordStore) RecordStarted(result DispatchResult, parentSessionID string) error {
	ctx := context.WithoutCancel(context.Background())
	return s.q.UpsertA2ADispatch(ctx, db.UpsertA2ADispatchParams{
		DispatchID:      result.DispatchID,
		SessionID:       result.SessionID,
		ParentSessionID: parentSessionID,
		Handle:          result.Handle,
		Branch:          result.Branch,
		WorkspacePath:   result.WorkspacePath,
		Status:          string(result.Status),
		HostID:          s.hostID,
		UpdatedAt:       time.Now().UnixNano(),
	})
}

// RecordTask implements [DispatchRecords].
func (s *SQLiteRecordStore) RecordTask(dispatchID, taskID string) error {
	ctx := context.WithoutCancel(context.Background())
	return s.q.SetA2ADispatchTask(ctx, db.SetA2ADispatchTaskParams{
		DispatchID: dispatchID,
		TaskID:     taskID,
		UpdatedAt:  time.Now().UnixNano(),
	})
}

// RecordTerminal implements [DispatchRecords].
func (s *SQLiteRecordStore) RecordTerminal(result DispatchResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("record terminal encode: %w", err)
	}
	ctx := context.WithoutCancel(context.Background())
	rows, err := s.q.FinishA2ADispatch(ctx, db.FinishA2ADispatchParams{
		DispatchID: result.DispatchID,
		Status:     string(result.Status),
		ResultJson: string(data),
		UpdatedAt:  time.Now().UnixNano(),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("record terminal: no running row for dispatch %s", result.DispatchID)
	}
	return nil
}

// RecordDelivered implements [DispatchRecords].
func (s *SQLiteRecordStore) RecordDelivered(dispatchID string) error {
	ctx := context.WithoutCancel(context.Background())
	return s.q.MarkA2ADispatchDelivered(ctx, db.MarkA2ADispatchDeliveredParams{
		DispatchID:  dispatchID,
		DeliveredAt: time.Now().UnixNano(),
	})
}

// UndeliveredTerminal implements [DispatchRecords].
func (s *SQLiteRecordStore) UndeliveredTerminal() ([]UndeliveredDispatch, error) {
	ctx := context.WithoutCancel(context.Background())
	rows, err := s.q.ListUndeliveredA2ADispatches(ctx)
	if err != nil {
		return nil, err
	}
	undelivered := make([]UndeliveredDispatch, 0, len(rows))
	for _, row := range rows {
		if row.ResultJson == "" {
			continue
		}
		var result DispatchResult
		if err := json.Unmarshal([]byte(row.ResultJson), &result); err != nil {
			return nil, fmt.Errorf("record decode %s: %w", row.DispatchID, err)
		}
		undelivered = append(undelivered, UndeliveredDispatch{
			ParentSessionID: row.ParentSessionID,
			Result:          result,
		})
	}
	return undelivered, nil
}

// SnapshotRecord implements [DispatchRecords].
func (s *SQLiteRecordStore) SnapshotRecord(sessionID string) (DispatchResult, Status, bool) {
	ctx := context.WithoutCancel(context.Background())
	row, err := s.q.GetA2ADispatchBySession(ctx, sessionID)
	if err != nil {
		return DispatchResult{}, "", false
	}
	if row.ResultJson == "" {
		return DispatchResult{}, Status(row.Status), true
	}
	var result DispatchResult
	if err := json.Unmarshal([]byte(row.ResultJson), &result); err != nil {
		return DispatchResult{}, "", false
	}
	return result, Status(row.Status), true
}
