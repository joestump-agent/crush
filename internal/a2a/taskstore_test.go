package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/db"
)

// newTestStore opens a fresh session database in a temp dir and returns
// a store over it owned by the given user identity.
func newTestStore(t *testing.T, user string) *SQLiteStore {
	t.Helper()
	database, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(db.ResetPool)
	return NewSQLiteStore(database, "test-host", &SQLiteStoreConfig{
		Authenticator: staticAuthenticator(user),
	})
}

// staticAuthenticator is a taskstore.Authenticator pinned to one user.
func staticAuthenticator(user string) taskstore.Authenticator {
	return func(context.Context) (string, error) { return user, nil }
}

// steppedClock hands out monotonically increasing timestamps one
// millisecond apart, making update ordering deterministic.
func steppedClock(start time.Time) func() time.Time {
	nanos := start.UnixNano()
	return func() time.Time {
		nanos += int64(time.Millisecond)
		return time.Unix(0, nanos)
	}
}

// testTask builds a minimal task with the given identity.
func testTask(id, contextID string, state a2aspec.TaskState) *a2aspec.Task {
	return &a2aspec.Task{
		ID:        a2aspec.TaskID(id),
		ContextID: contextID,
		Status:    a2aspec.TaskStatus{State: state},
	}
}

// A created task reads back with its identity, version 1, and owner;
// the copy handed out is private — mutating it never touches the store.
func TestSQLiteStoreCreateGetRoundTrip(t *testing.T) {
	store := newTestStore(t, "alice")
	task := testTask("task-1", "ctx-1", a2aspec.TaskStateSubmitted)

	version, err := store.Create(t.Context(), task)
	require.NoError(t, err)
	require.Equal(t, taskstore.TaskVersion(1), version)

	stored, err := store.Get(t.Context(), "task-1")
	require.NoError(t, err)
	require.Equal(t, "task-1", string(stored.Task.ID))
	require.Equal(t, "ctx-1", stored.Task.ContextID)
	require.Equal(t, a2aspec.TaskStateSubmitted, stored.Task.Status.State)
	require.Equal(t, taskstore.TaskVersion(1), stored.Version)
	require.Equal(t, "alice", stored.User)

	stored.Task.Status.State = a2aspec.TaskStateCompleted
	stored.Task.History = append(stored.Task.History, a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("mutated")))

	again, err := store.Get(t.Context(), "task-1")
	require.NoError(t, err)
	require.Equal(t, a2aspec.TaskStateSubmitted, again.Task.Status.State)
	require.Empty(t, again.Task.History)
}

// A duplicate create is rejected; missing tasks read as not found.
func TestSQLiteStoreCreateDuplicateAndGetMissing(t *testing.T) {
	store := newTestStore(t, "alice")

	_, err := store.Create(t.Context(), testTask("task-1", "ctx-1", a2aspec.TaskStateSubmitted))
	require.NoError(t, err)

	_, err = store.Create(t.Context(), testTask("task-1", "ctx-1", a2aspec.TaskStateWorking))
	require.ErrorIs(t, err, taskstore.ErrTaskAlreadyExists)

	_, err = store.Get(t.Context(), "nope")
	require.ErrorIs(t, err, a2aspec.ErrTaskNotFound)
}

// Updates increment the version, guard on PrevVersion, and mask a
// missing task as not found.
func TestSQLiteStoreUpdateVersioning(t *testing.T) {
	store := newTestStore(t, "alice")
	_, err := store.Create(t.Context(), testTask("task-1", "ctx-1", a2aspec.TaskStateSubmitted))
	require.NoError(t, err)

	working := testTask("task-1", "ctx-1", a2aspec.TaskStateWorking)
	version, err := store.Update(t.Context(), &taskstore.UpdateRequest{Task: working, PrevVersion: 1})
	require.NoError(t, err)
	require.Equal(t, taskstore.TaskVersion(2), version)

	// An untracked PrevVersion updates regardless of the stored version.
	version, err = store.Update(t.Context(), &taskstore.UpdateRequest{
		Task: testTask("task-1", "ctx-1", a2aspec.TaskStateCompleted),
	})
	require.NoError(t, err)
	require.Equal(t, taskstore.TaskVersion(3), version)

	// A stale PrevVersion is a lost update race.
	_, err = store.Update(t.Context(), &taskstore.UpdateRequest{Task: working, PrevVersion: 2})
	require.ErrorIs(t, err, taskstore.ErrConcurrentModification)

	_, err = store.Update(t.Context(), &taskstore.UpdateRequest{
		Task: testTask("nope", "ctx-1", a2aspec.TaskStateFailed),
	})
	require.ErrorIs(t, err, a2aspec.ErrTaskNotFound)

	stored, err := store.Get(t.Context(), "task-1")
	require.NoError(t, err)
	require.Equal(t, a2aspec.TaskStateCompleted, stored.Task.Status.State)
	require.Equal(t, taskstore.TaskVersion(3), stored.Version)
}

// Another user's tasks are invisible: both Get and Update read as not
// found, never as forbidden (A2A spec §3.3.2).
func TestSQLiteStoreOwnershipMasking(t *testing.T) {
	database, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(db.ResetPool)
	alice := NewSQLiteStore(database, "test-host", &SQLiteStoreConfig{Authenticator: staticAuthenticator("alice")})
	bob := NewSQLiteStore(database, "test-host", &SQLiteStoreConfig{Authenticator: staticAuthenticator("bob")})

	_, err = alice.Create(t.Context(), testTask("task-1", "ctx-1", a2aspec.TaskStateSubmitted))
	require.NoError(t, err)

	_, err = bob.Get(t.Context(), "task-1")
	require.ErrorIs(t, err, a2aspec.ErrTaskNotFound)

	_, err = bob.Update(t.Context(), &taskstore.UpdateRequest{
		Task: testTask("task-1", "ctx-1", a2aspec.TaskStateFailed),
	})
	require.ErrorIs(t, err, a2aspec.ErrTaskNotFound)

	// The owner still sees it, and bob's rejected update changed nothing.
	stored, err := alice.Get(t.Context(), "task-1")
	require.NoError(t, err)
	require.Equal(t, a2aspec.TaskStateSubmitted, stored.Task.Status.State)
}

func TestSQLiteStoreList(t *testing.T) {
	store := newTestStore(t, "alice")
	store.config.TimeProvider = steppedClock(time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))

	seed := []*a2aspec.Task{
		testTask("t1", "ctx-2", a2aspec.TaskStateWorking),
		testTask("t2", "ctx-2", a2aspec.TaskStateCompleted),
		testTask("t3", "ctx-3", a2aspec.TaskStateCompleted),
	}
	for _, task := range seed {
		_, err := store.Create(t.Context(), task)
		require.NoError(t, err)
	}

	resp, err := store.List(t.Context(), &a2aspec.ListTasksRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Tasks, 3)
	require.Equal(t, 3, resp.TotalSize)
	require.Equal(t, 50, resp.PageSize)
	// Newest update first: creation order reversed.
	require.Equal(t, []string{"t3", "t2", "t1"}, taskIDs(resp.Tasks))
	require.Empty(t, resp.NextPageToken)

	resp, err = store.List(t.Context(), &a2aspec.ListTasksRequest{ContextID: "ctx-2"})
	require.NoError(t, err)
	require.Equal(t, []string{"t2", "t1"}, taskIDs(resp.Tasks))
	require.Equal(t, 2, resp.TotalSize)

	resp, err = store.List(t.Context(), &a2aspec.ListTasksRequest{Status: a2aspec.TaskStateCompleted})
	require.NoError(t, err)
	require.Equal(t, []string{"t3", "t2"}, taskIDs(resp.Tasks))
	require.Equal(t, 2, resp.TotalSize)

	// StatusTimestampAfter filters the row's updated_at column: everything
	// created before t2's update instant falls away.
	after := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC).Add(2 * time.Millisecond)
	resp, err = store.List(t.Context(), &a2aspec.ListTasksRequest{StatusTimestampAfter: &after})
	require.NoError(t, err)
	require.Equal(t, []string{"t3", "t2"}, taskIDs(resp.Tasks))

	// Another user's tasks are invisible: the user filter empties the page.
	bobStore := newTestStore(t, "bob")
	resp, err = bobStore.List(t.Context(), &a2aspec.ListTasksRequest{})
	require.NoError(t, err)
	require.Empty(t, resp.Tasks)
	require.Zero(t, resp.TotalSize)
}

// Pagination walks every task exactly once, in the unpaginated order.
func TestSQLiteStoreListPagination(t *testing.T) {
	store := newTestStore(t, "alice")
	store.config.TimeProvider = steppedClock(time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))

	for i := range 5 {
		_, err := store.Create(t.Context(), testTask(fmt.Sprintf("t%d", i), "ctx-1", a2aspec.TaskStateWorking))
		require.NoError(t, err)
	}

	var seen []string
	token := ""
	pages := 0
	for {
		resp, err := store.List(t.Context(), &a2aspec.ListTasksRequest{PageSize: 2, PageToken: token})
		require.NoError(t, err)
		pages++
		seen = append(seen, taskIDs(resp.Tasks)...)
		require.Equal(t, 5, resp.TotalSize)
		require.Equal(t, 2, resp.PageSize)
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
		require.Less(t, pages, 10, "pagination did not terminate")
	}
	require.Equal(t, []string{"t4", "t3", "t2", "t1", "t0"}, seen)
	require.Equal(t, 3, pages)

	// A mid-stream delete does not derail the cursor: the last page may
	// be short, but nothing is skipped or duplicated.
	resp, err := store.List(t.Context(), &a2aspec.ListTasksRequest{PageSize: 4})
	require.NoError(t, err)
	require.Equal(t, []string{"t4", "t3", "t2", "t1"}, taskIDs(resp.Tasks))
	resp, err = store.List(t.Context(), &a2aspec.ListTasksRequest{PageSize: 4, PageToken: resp.NextPageToken})
	require.NoError(t, err)
	require.Equal(t, []string{"t0"}, taskIDs(resp.Tasks))
	require.Empty(t, resp.NextPageToken)
}

// List response shaping: history length (default 100, explicit, and
// non-positive) and artifact stripping — against the stored task, not
// just the response copy.
func TestSQLiteStoreListShaping(t *testing.T) {
	store := newTestStore(t, "alice")
	store.config.TimeProvider = steppedClock(time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))

	task := testTask("t1", "ctx-1", a2aspec.TaskStateCompleted)
	for range 150 {
		task.History = append(task.History, a2aspec.NewMessage(a2aspec.MessageRoleAgent, a2aspec.NewTextPart("line")))
	}
	task.Artifacts = []*a2aspec.Artifact{{
		ID:    "a1",
		Name:  "out",
		Parts: []*a2aspec.Part{a2aspec.NewTextPart("payload")},
	}}
	_, err := store.Create(t.Context(), task)
	require.NoError(t, err)

	list := func(req *a2aspec.ListTasksRequest) *a2aspec.Task {
		resp, err := store.List(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, resp.Tasks, 1)
		return resp.Tasks[0]
	}

	got := list(&a2aspec.ListTasksRequest{})
	require.Len(t, got.History, 100, "default history cap")
	require.Nil(t, got.Artifacts, "artifacts stripped by default")

	got = list(&a2aspec.ListTasksRequest{HistoryLength: ptr(3)})
	require.Len(t, got.History, 3)
	require.Equal(t, "line", got.History[2].Parts[0].Text())

	got = list(&a2aspec.ListTasksRequest{HistoryLength: ptr(0)})
	require.NotNil(t, got.History)
	require.Empty(t, got.History)

	got = list(&a2aspec.ListTasksRequest{IncludeArtifacts: true})
	require.NotNil(t, got.Artifacts)
	require.Len(t, got.Artifacts, 1)

	// Shaping never mutates stored state.
	stored, err := store.Get(t.Context(), "t1")
	require.NoError(t, err)
	require.Len(t, stored.Task.History, 150)
	require.NotNil(t, stored.Task.Artifacts)
}

// List rejects unauthenticated callers outright and page sizes out of
// bounds as invalid requests; malformed page tokens are parse errors.
func TestSQLiteStoreListValidation(t *testing.T) {
	anon := newTestStore(t, "")
	_, err := anon.List(t.Context(), &a2aspec.ListTasksRequest{})
	require.ErrorIs(t, err, a2aspec.ErrUnauthenticated)

	store := newTestStore(t, "alice")
	for _, size := range []int{-1, 101} {
		_, err := store.List(t.Context(), &a2aspec.ListTasksRequest{PageSize: size})
		require.ErrorIs(t, err, a2aspec.ErrInvalidRequest, "pageSize %d", size)
	}

	for _, token := range []string{"not-base64!!", "Z2FyYmFnZQ==", "YWJjXzE="} {
		_, err := store.List(t.Context(), &a2aspec.ListTasksRequest{PageToken: token})
		require.ErrorIs(t, err, a2aspec.ErrParseError, "token %q", token)
	}
}

// Concurrent updates racing on the same PrevVersion produce exactly one
// winner and count-1 concurrent-modification rejections.
func TestSQLiteStoreConcurrentUpdates(t *testing.T) {
	store := newTestStore(t, "alice")
	_, err := store.Create(t.Context(), testTask("task-1", "ctx-1", a2aspec.TaskStateSubmitted))
	require.NoError(t, err)

	const racers = 8
	var wg sync.WaitGroup
	results := make(chan error, racers)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update(t.Context(), &taskstore.UpdateRequest{
				Task:        testTask("task-1", "ctx-1", a2aspec.TaskStateWorking),
				PrevVersion: 1,
			})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var wins, losses int
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, taskstore.ErrConcurrentModification):
			losses++
		default:
			t.Fatalf("unexpected racer error: %v", err)
		}
	}
	require.Equal(t, 1, wins)
	require.Equal(t, racers-1, losses)

	stored, err := store.Get(t.Context(), "task-1")
	require.NoError(t, err)
	require.Equal(t, taskstore.TaskVersion(2), stored.Version)
}

// An update on a canceled context still lands: the failure path must
// persist a task's terminal state even after the client is gone.
func TestSQLiteStoreUpdatePersistsOnCanceledContext(t *testing.T) {
	store := newTestStore(t, "alice")
	_, err := store.Create(t.Context(), testTask("task-1", "ctx-1", a2aspec.TaskStateWorking))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.Update(ctx, &taskstore.UpdateRequest{
		Task:        testTask("task-1", "ctx-1", a2aspec.TaskStateFailed),
		PrevVersion: 1,
	})
	require.NoError(t, err)

	stored, err := store.Get(t.Context(), "task-1")
	require.NoError(t, err)
	require.Equal(t, a2aspec.TaskStateFailed, stored.Task.Status.State)
	require.Equal(t, taskstore.TaskVersion(2), stored.Version)
}

// The migration's Down side really drops the table.
func TestA2ATasksMigrationDown(t *testing.T) {
	database, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(db.ResetPool)

	store := NewSQLiteStore(database, "test-host", &SQLiteStoreConfig{Authenticator: staticAuthenticator("alice")})
	_, err = store.Create(t.Context(), testTask("task-1", "ctx-1", a2aspec.TaskStateSubmitted))
	require.NoError(t, err, "the table must exist after migrations run")

	require.NoError(t, goose.Down(database, "migrations"))

	var count int
	err = database.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='a2a_tasks'").Scan(&count)
	require.NoError(t, err)
	require.Zero(t, count, "Down must drop a2a_tasks")
}

// The full served path: a message/send through StartServer with a
// SQLite store lands the task durably — a store opened on a freshly
// reopened database (the restart proxy) reads it back.
func TestServerPersistsTasksThroughSQLiteStore(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(db.ResetPool)

	runner := &fakeRunner{result: textResult("durable answer")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "durable-task",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
		TaskStore:  NewSQLiteStore(database, "test-host", nil),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("run the task"))
	msg.ContextID = "dispatch-session"
	params, err := json.Marshal(&a2aspec.SendMessageRequest{
		Message: msg,
	})
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  json.RawMessage(params),
	})
	require.NoError(t, err)

	resp, err := postJSONRPCAuthed(t, factory, unixDialClient(factory), server.Endpoint, body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpcResp struct {
		Result struct {
			Task *a2aspec.Task `json:"task"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
	require.Nil(t, rpcResp.Error, "JSON-RPC error on SendMessage")
	require.NotNil(t, rpcResp.Result.Task, "message/send must return a task")
	require.Equal(t, a2aspec.TaskStateCompleted, rpcResp.Result.Task.Status.State)
	taskID := rpcResp.Result.Task.ID
	require.NoError(t, server.Stop(context.Background()))

	// Simulate the restart: drop the pooled handle and reopen.
	require.NoError(t, db.Release(dataDir))
	reopened, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	// The host authenticates served calls as the socket peer's uid
	// (#357), and that is the name the task was stored under.
	store := NewSQLiteStore(reopened, "test-host", &SQLiteStoreConfig{
		Authenticator: staticAuthenticator(servedUserName()),
	})
	stored, err := store.Get(t.Context(), taskID)
	require.NoError(t, err, "the served task must survive a reopen")
	require.Equal(t, a2aspec.TaskStateCompleted, stored.Task.Status.State)
	require.Positive(t, int64(stored.Version))
}

func taskIDs(tasks []*a2aspec.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, string(task.ID))
	}
	return ids
}

func ptr[T any](v T) *T { return &v }
