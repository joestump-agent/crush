package a2a

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/charmbracelet/crush/internal/db"
)

// SQLiteStoreConfig configures a [SQLiteStore]. Zero fields fall back to
// the SDK defaults: the a2asrv call-context authenticator and time.Now.
type SQLiteStoreConfig struct {
	// Authenticator resolves the task owner's identity from the request
	// context. Defaults to a2asrv.NewTaskStoreAuthenticator().
	Authenticator taskstore.Authenticator
	// TimeProvider sources the updated_at clock. Defaults to time.Now;
	// tests pin it for deterministic ordering and cursor pagination.
	TimeProvider func() time.Time
}

// SQLiteStore is the durable taskstore.Store (#354): tasks live in the
// a2a_tasks table of the session database, so served tasks and their
// histories survive process restarts instead of dying with the
// in-process default the SDK falls back to. Task payloads are stored as
// JSON, which doubles as the deep copy the store contract requires —
// callers never observe a task object this store also holds. The
// production wiring lands with #355; ServerParams.TaskStore takes one
// today.
type SQLiteStore struct {
	q      *db.Queries
	hostID string
	config SQLiteStoreConfig
}

var _ taskstore.Store = (*SQLiteStore)(nil)

// NewSQLiteStore builds a store over an open session database. hostID
// is stamped on every created row so a future multi-host deployment
// (#355) can attribute tasks; the queries do not filter on it yet.
func NewSQLiteStore(database *sql.DB, hostID string, config *SQLiteStoreConfig) *SQLiteStore {
	s := &SQLiteStore{q: db.New(database), hostID: hostID}
	if config != nil {
		s.config = *config
	}
	if s.config.TimeProvider == nil {
		s.config.TimeProvider = time.Now
	}
	if s.config.Authenticator == nil {
		s.config.Authenticator = a2asrv.NewTaskStoreAuthenticator()
	}
	return s
}

// Create implements [taskstore.Store]. The first version of a task is
// always 1; a duplicate ID is ErrTaskAlreadyExists.
func (s *SQLiteStore) Create(ctx context.Context, task *a2aspec.Task) (taskstore.TaskVersion, error) {
	user, err := s.config.Authenticator(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}
	data, err := json.Marshal(task)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore encode failed: %w", err)
	}
	now := s.config.TimeProvider()
	err = s.q.CreateA2ATask(writeContext(ctx), db.CreateA2ATaskParams{
		ID:        string(task.ID),
		ContextID: task.ContextID,
		User:      user,
		HostID:    s.hostID,
		State:     string(task.Status.State),
		TaskJson:  string(data),
		CreatedAt: now.UnixNano(),
		UpdatedAt: now.UnixNano(),
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
		}
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore create failed: %w", err)
	}
	return taskstore.TaskVersion(1), nil
}

// Update implements [taskstore.Store]. Ownership mismatches are masked
// as ErrTaskNotFound per the A2A spec (§3.3.2), and a PrevVersion that
// no longer matches the stored row is ErrConcurrentModification. Both
// the read and the write run on a context detached from cancellation so
// a task's terminal Failed state is persisted even when the client that
// triggered it is gone.
func (s *SQLiteStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	user, err := s.config.Authenticator(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}
	data, err := json.Marshal(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore encode failed: %w", err)
	}

	ctx = writeContext(ctx)
	prev, err := s.q.GetA2ATask(ctx, string(req.Task.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return taskstore.TaskVersionMissing, a2aspec.ErrTaskNotFound
	}
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore update failed: %w", err)
	}
	if prev.User != user {
		return taskstore.TaskVersionMissing, a2aspec.ErrTaskNotFound
	}
	if req.PrevVersion != taskstore.TaskVersionMissing && taskstore.TaskVersion(prev.Version) != req.PrevVersion {
		return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
	}

	now := s.config.TimeProvider()
	params := db.UpdateA2ATaskParams{
		ContextID: req.Task.ContextID,
		State:     string(req.Task.Status.State),
		TaskJson:  string(data),
		UpdatedAt: now.UnixNano(),
		ID:        string(req.Task.ID),
	}
	var rows int64
	if req.PrevVersion == taskstore.TaskVersionMissing {
		rows, err = s.q.UpdateA2ATask(ctx, params)
	} else {
		rows, err = s.q.UpdateA2ATaskIfVersion(ctx, db.UpdateA2ATaskIfVersionParams{
			ContextID: params.ContextID,
			State:     params.State,
			TaskJson:  params.TaskJson,
			UpdatedAt: params.UpdatedAt,
			ID:        params.ID,
			Version:   int64(req.PrevVersion),
		})
	}
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore update failed: %w", err)
	}
	if rows == 0 {
		if req.PrevVersion != taskstore.TaskVersionMissing {
			return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
		}
		return taskstore.TaskVersionMissing, a2aspec.ErrTaskNotFound
	}
	return taskstore.TaskVersion(prev.Version + 1), nil
}

// Get implements [taskstore.Store]. A task owned by another user reads
// as ErrTaskNotFound, never as a forbidden.
func (s *SQLiteStore) Get(ctx context.Context, taskID a2aspec.TaskID) (*taskstore.StoredTask, error) {
	row, err := s.q.GetA2ATask(ctx, string(taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, a2aspec.ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("taskstore get failed: %w", err)
	}
	user, err := s.config.Authenticator(ctx)
	if err != nil {
		return nil, fmt.Errorf("taskstore auth failed: %w", err)
	}
	if row.User != user {
		return nil, a2aspec.ErrTaskNotFound
	}
	task, err := decodeStoredTask(row.TaskJson)
	if err != nil {
		return nil, err
	}
	return &taskstore.StoredTask{
		Task:    task,
		Version: taskstore.TaskVersion(row.Version),
		User:    row.User,
	}, nil
}

// List implements [taskstore.Store]. Ordering and pagination mirror the
// SDK's in-memory store: newest update first, ties broken by descending
// task ID, and an opaque cursor token resumes exactly where the last
// page stopped. One deliberate deviation, sanctioned by #354: the
// StatusTimestampAfter filter applies to the row's updated_at column
// rather than each task's embedded status timestamp, so the filter and
// the cursor ride the same indexed column.
func (s *SQLiteStore) List(ctx context.Context, req *a2aspec.ListTasksRequest) (*a2aspec.ListTasksResponse, error) {
	const defaultPageSize = 50
	user, err := s.config.Authenticator(ctx)
	if user == "" || err != nil {
		return nil, a2aspec.ErrUnauthenticated
	}
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	} else if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page size must be between 1 and 100 inclusive, got %d: %w", pageSize, a2aspec.ErrInvalidRequest)
	}

	var after int64
	if req.StatusTimestampAfter != nil {
		after = req.StatusTimestampAfter.UnixNano()
	}
	cursorAt, cursorID, err := decodePageToken(req.PageToken)
	if err != nil {
		return nil, err
	}

	filters := db.CountA2ATasksParams{
		User:      user,
		ContextID: req.ContextID,
		State:     listStateFilter(req.Status),
		After:     after,
	}
	total, err := s.q.CountA2ATasks(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("taskstore list failed: %w", err)
	}
	rows, err := s.q.ListA2ATasks(ctx, db.ListA2ATasksParams{
		User:      filters.User,
		ContextID: filters.ContextID,
		State:     filters.State,
		After:     filters.After,
		CursorAt:  cursorAt,
		CursorID:  cursorID,
		PageSize:  int64(pageSize + 1),
	})
	if err != nil {
		return nil, fmt.Errorf("taskstore list failed: %w", err)
	}

	var nextPageToken string
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		last := rows[len(rows)-1]
		nextPageToken = encodePageToken(last.UpdatedAt, last.ID)
	}

	var tasks []*a2aspec.Task
	for _, row := range rows {
		task, err := decodeStoredTask(row.TaskJson)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, trimListedTask(task, req))
	}
	return &a2aspec.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     int(total),
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}, nil
}

// writeContext detaches a context from cancellation for the duration of
// a store write: values flow through, but a client that hung up cannot
// stop the terminal state from reaching the database.
func writeContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// listStateFilter maps the request's status filter to its column value,
// with the empty string standing in for "no filter".
func listStateFilter(status a2aspec.TaskState) string {
	if status == a2aspec.TaskStateUnspecified {
		return ""
	}
	return string(status)
}

// decodeStoredTask rebuilds a task from its JSON row payload. The
// result is always a private copy, so callers may mutate it freely.
func decodeStoredTask(data string) (*a2aspec.Task, error) {
	var task a2aspec.Task
	if err := json.Unmarshal([]byte(data), &task); err != nil {
		return nil, fmt.Errorf("taskstore decode failed: %w", err)
	}
	return &task, nil
}

// trimListedTask applies the list request's per-task shaping: the
// history is capped at HistoryLength (default 100, non-positive drops
// it entirely) and artifacts are stripped unless asked for. The task is
// a fresh decode, so trimming in place is safe.
func trimListedTask(task *a2aspec.Task, req *a2aspec.ListTasksRequest) *a2aspec.Task {
	const defaultMaxHistoryLength = 100
	historyLength := defaultMaxHistoryLength
	if req.HistoryLength != nil {
		historyLength = *req.HistoryLength
	}
	if historyLength <= 0 {
		task.History = []*a2aspec.Message{}
	} else if len(task.History) > historyLength {
		task.History = task.History[len(task.History)-historyLength:]
	}
	if !req.IncludeArtifacts {
		task.Artifacts = nil
	}
	return task
}

// encodePageToken packs a cursor position (updated_at nanos plus task
// ID) into an opaque, URL-safe token.
func encodePageToken(updatedAtNanos int64, taskID string) string {
	return base64.URLEncoding.EncodeToString(fmt.Appendf(nil, "%d_%s", updatedAtNanos, taskID))
}

// decodePageToken unpacks a token from [encodePageToken]; anything
// malformed is a2aspec.ErrParseError, matching the SDK's behavior for
// unparseable page tokens.
func decodePageToken(token string) (updatedAtNanos int64, taskID string, err error) {
	if token == "" {
		return 0, "", nil
	}
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return 0, "", a2aspec.ErrParseError
	}
	parts := strings.SplitN(string(decoded), "_", 2)
	if len(parts) != 2 {
		return 0, "", a2aspec.ErrParseError
	}
	updatedAtNanos, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", a2aspec.ErrParseError
	}
	return updatedAtNanos, parts[1], nil
}
