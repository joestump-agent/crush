package a2a

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/session"
)

// indexConn returns the factory's own index connection, failing the test
// before the host is up.
func indexConn(t *testing.T, f *ServerFactory) AgentIndexConn {
	t.Helper()
	conn, ok := f.AgentIndexConn()
	require.True(t, ok, "the host is not up")
	return conn
}

// nextUpsert reads index events until one upserts a descriptor matching
// match, failing on a closed stream or after a bound.
func nextUpsert(t *testing.T, events <-chan AgentIndexEvent, match func(AgentDescriptor) bool) AgentDescriptor {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "the index stream closed")
			if ev.Upsert != nil && match(*ev.Upsert) {
				return *ev.Upsert
			}
		case <-timeout:
			t.Fatal("no matching index event")
		}
	}
}

// A listed dispatch is on the index from the moment its route is up, and
// its descriptor follows its task — working, todo progress and usage,
// then terminal — and stays listed, no longer served, after its route
// goes down (#421).
func TestAgentIndexFollowsListedDispatch(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	runner := newPacedRunner("done")
	source := newPipeTodoSource()
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID:      "dispatch-1",
		Runner:          runner,
		SessionID:       "dispatch-session",
		ContextID:       "dispatch-session",
		Todos:           source,
		Name:            "tester",
		Description:     "writes tests",
		Listed:          true,
		ParentSessionID: "parent-session",
		Usage: func(context.Context) (agent.Usage, error) {
			return agent.Usage{PromptTokens: 120, CompletionTokens: 30, Cost: 0.25}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	events, err := indexConn(t, factory).WatchAgents(t.Context())
	require.NoError(t, err)
	first := <-events
	require.Len(t, first.Snapshot, 1, "the stream opens with the snapshot")
	listed := first.Snapshot[0]
	require.Equal(t, "dispatch-1", listed.ID)
	require.Equal(t, "tester", listed.Handle)
	require.Equal(t, "writes tests", listed.Role)
	require.Equal(t, "dispatch-session", listed.ContextID)
	require.Equal(t, "parent-session", listed.ParentSessionID)
	require.Equal(t, server.Endpoint, listed.Endpoint)
	require.NotNil(t, listed.Card)
	require.True(t, listed.Served)
	require.Empty(t, listed.State, "no task before the dispatch's first message")

	streamDone := make(chan error, 1)
	go func() {
		_, err := factory.StreamDispatch(context.Background(), agent.DispatchTransportParams{
			Endpoint: server.Endpoint, Card: server.Card, Prompt: "fix the bug", ContextID: "dispatch-session",
		})
		streamDone <- err
	}()
	<-runner.started
	working := nextUpsert(t, events, func(d AgentDescriptor) bool { return d.State == DispatchStatusWorking })
	require.NotEmpty(t, working.TaskID)

	source.ch <- todoSnapshot(runTodos(
		session.Todo{Content: "wiring form validation", Status: session.TodoStatusInProgress, ActiveForm: "wiring form validation"},
	))
	progress := nextUpsert(t, events, func(d AgentDescriptor) bool { return d.Progress != nil })
	require.Equal(t, "wiring form validation", progress.Progress.Current)
	require.NotNil(t, progress.Usage, "usage rides the progress event")
	require.Equal(t, int64(120), progress.Usage.PromptTokens)

	close(runner.release)
	require.NoError(t, <-streamDone)
	done := nextUpsert(t, events, func(d AgentDescriptor) bool { return d.Terminal() })
	require.Equal(t, DispatchStatusCompleted, done.State)
	require.Equal(t, "done", done.StatusText)
	require.NotNil(t, done.FinishedAt)
	require.Equal(t, working.TaskID, done.TaskID)

	require.NoError(t, server.Stop(context.Background()))
	stopped := nextUpsert(t, events, func(d AgentDescriptor) bool { return !d.Served })
	require.Equal(t, DispatchStatusCompleted, stopped.State, "a stopped dispatch keeps its last state")

	listedAgain, err := indexConn(t, factory).ListAgents(t.Context())
	require.NoError(t, err)
	require.Len(t, listedAgain, 1, "a finished dispatch stays on the index")
	require.False(t, listedAgain[0].Served)
}

// A steer opens its own task on the dispatch's context; the descriptor
// keeps following the dispatch's task (#421).
func TestAgentIndexIgnoresSteerTasks(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	runner := newPacedRunner("done")
	runner.enqueueAccepted = true
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1", Runner: runner, SessionID: "dispatch-session", ContextID: "dispatch-session",
		Name: "tester", Listed: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	events, err := indexConn(t, factory).WatchAgents(t.Context())
	require.NoError(t, err)
	<-events

	go func() {
		_, _ = factory.StreamDispatch(context.Background(), agent.DispatchTransportParams{
			Endpoint: server.Endpoint, Card: server.Card, Prompt: "fix the bug", ContextID: "dispatch-session",
		})
	}()
	<-runner.started
	working := nextUpsert(t, events, func(d AgentDescriptor) bool { return d.State == DispatchStatusWorking })

	steer, err := factory.SteerDispatch(t.Context(), agent.DispatchSteerParams{
		Endpoint: server.Endpoint, Card: server.Card, ContextID: "dispatch-session", Text: "also the docs",
	})
	require.NoError(t, err)
	require.Equal(t, SteerStatusWorking, steer.Status)
	runner.consume(true)

	snapshot, err := indexConn(t, factory).ListAgents(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	require.Equal(t, working.TaskID, snapshot[0].TaskID, "the steer's task never replaces the dispatch's")
	require.Equal(t, DispatchStatusWorking, snapshot[0].State)

	close(runner.release)
}

// Only listed routes are on the index: a sub-agent turn is not (#421).
func TestAgentIndexListsOnlyListedRoutes(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "agent-subturn", Runner: &fakeRunner{result: textResult("x")}, SessionID: "sub", ContextID: "sub",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	listed, err := indexConn(t, factory).ListAgents(t.Context())
	require.NoError(t, err)
	require.Empty(t, listed)
}

// The index takes the host's checks: the bearer token, no Origin, the
// host's Host, and GET only (#421).
func TestAgentIndexRequiresHostChecks(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1", Runner: &fakeRunner{result: textResult("x")}, SessionID: "s", ContextID: "s", Listed: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	conn := indexConn(t, factory)

	do := func(t *testing.T, mutate func(*http.Request)) int {
		t.Helper()
		req, err := conn.request(t.Context(), "application/json")
		require.NoError(t, err)
		mutate(req)
		resp, err := conn.HTTP.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, do(t, func(*http.Request) {}))
	require.Equal(t, http.StatusUnauthorized, do(t, func(r *http.Request) { r.Header.Del(bearerAuthorizationHeader) }))
	require.Equal(t, http.StatusUnauthorized, do(t, func(r *http.Request) { r.Header.Set(bearerAuthorizationHeader, "Bearer wrong") }))
	require.Equal(t, http.StatusForbidden, do(t, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }))
	require.Equal(t, http.StatusBadRequest, do(t, func(r *http.Request) { r.Host = "evil.example" }))
	require.Equal(t, http.StatusMethodNotAllowed, do(t, func(r *http.Request) { r.Method = http.MethodPost }))
}

// A stream that falls too far behind is closed rather than blocking the
// task it tracks; the watcher reconnects to a fresh snapshot (#421).
func TestAgentIndexDropsAStalledStream(t *testing.T) {
	t.Parallel()
	x := newAgentIndex()
	events, unsubscribe := x.subscribe()
	defer unsubscribe()

	// The snapshot took one slot; one more change than the rest of the
	// buffer holds closes the stream.
	for i := range indexEventBuffer {
		x.add(AgentDescriptor{ID: "d" + strconv.Itoa(i)})
	}
	n := 0
	for range events {
		n++
	}
	require.Equal(t, indexEventBuffer, n, "the stalled stream got what fit, then was closed")
}

// Closing the host ends its index streams instead of waiting on them
// (#421).
func TestAgentIndexStreamsEndWithTheHost(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1", Runner: &fakeRunner{result: textResult("x")}, SessionID: "s", ContextID: "s", Listed: true,
	})
	require.NoError(t, err)
	events, err := indexConn(t, factory).WatchAgents(t.Context())
	require.NoError(t, err)
	<-events

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, factory.Close(ctx), "Close must not wait out its deadline on an open index stream")
	for range events {
	}
}

// The SSE reader skips comments and joins multi-line data fields.
func TestReadIndexEvents(t *testing.T) {
	t.Parallel()
	body := ": keep-alive\n\ndata: {\"upsert\":\ndata: {\"id\":\"d1\",\"handle\":\"x\",\"context_id\":\"c\",\"endpoint\":\"e\",\"served\":true,\"started_at\":\"2026-10-07T00:00:00Z\",\"updated_at\":\"2026-10-07T00:00:00Z\"}}\n\n"
	var got []AgentIndexEvent
	require.NoError(t, readIndexEvents(strings.NewReader(body), func(ev AgentIndexEvent) bool {
		got = append(got, ev)
		return true
	}))
	require.Len(t, got, 1)
	require.Equal(t, "d1", got[0].Upsert.ID)
}

// The descriptor follows only the task the executor names as the
// dispatch's own, on the dispatch's context: writes before it is named,
// and writes to a task on another context, never move it (#421).
func TestAgentIndexFollowsOnlyTheNamedTask(t *testing.T) {
	t.Parallel()
	x := newAgentIndex()
	x.add(AgentDescriptor{ID: "d1", ContextID: "ctx-1"})
	working := func(id, contextID string) *a2aspec.Task {
		return &a2aspec.Task{ID: a2aspec.TaskID(id), ContextID: contextID, Status: a2aspec.TaskStatus{State: a2aspec.TaskStateWorking}}
	}

	x.track("d1", working("steer-first", "ctx-1"), nil)
	require.Empty(t, x.snapshot()[0].State, "nothing is followed before the executor names the task")

	x.setTask("d1", "own")
	x.setTask("d1", "later")
	require.Equal(t, "own", x.snapshot()[0].TaskID, "the first named task wins")

	x.track("d1", working("own", "ctx-other"), nil)
	require.Empty(t, x.snapshot()[0].State, "a task on another context is not the dispatch's")
	x.track("d1", working("own", "ctx-1"), nil)
	require.Equal(t, DispatchStatusWorking, x.snapshot()[0].State)
}

// A write that changes nothing a reader sees publishes nothing, and every
// published change carries a newer revision (#421).
func TestAgentIndexPublishesOnlyChanges(t *testing.T) {
	t.Parallel()
	x := newAgentIndex()
	x.add(AgentDescriptor{ID: "d1", ContextID: "ctx-1"})
	x.setTask("d1", "own")
	events, unsubscribe := x.subscribe()
	defer unsubscribe()
	snapshot := <-events
	rev := snapshot.Snapshot[0].Revision

	task := &a2aspec.Task{ID: "own", ContextID: "ctx-1", Status: a2aspec.TaskStatus{State: a2aspec.TaskStateWorking}}
	x.track("d1", task, nil)
	first := <-events
	require.Greater(t, first.Upsert.Revision, rev)

	x.track("d1", task, nil)
	select {
	case ev := <-events:
		t.Fatalf("an unchanged write was published: %+v", ev.Upsert)
	default:
	}
}

// Ended dispatches beyond maxFinishedDescriptors are dropped oldest first;
// served ones are never dropped (#421).
func TestAgentIndexEvictsOldestEnded(t *testing.T) {
	t.Parallel()
	x := newAgentIndex()
	x.add(AgentDescriptor{ID: "live"})
	for i := range maxFinishedDescriptors + 2 {
		id := "ended-" + strconv.Itoa(i)
		x.add(AgentDescriptor{ID: id})
		x.unserve(id)
	}
	snapshot := x.snapshot()
	require.Len(t, snapshot, maxFinishedDescriptors+1)
	require.Equal(t, "live", snapshot[0].ID, "a served dispatch is never evicted")
	require.Equal(t, "ended-2", snapshot[1].ID, "the two oldest ended dispatches went first")
}

// A stream opened after the index closed is already closed, so a request
// that raced the host's shutdown cannot hold it (#421).
func TestAgentIndexSubscribeAfterClose(t *testing.T) {
	t.Parallel()
	x := newAgentIndex()
	x.closeAll()
	events, unsubscribe := x.subscribe()
	defer unsubscribe()
	_, open := <-events
	require.False(t, open)
}

// Progress events carry the usage so far for watchers (#421), but only a
// terminal status's usage is charged to the parent (#364): a run the
// executor's own Cancel ends, after progress carried a partial reading,
// leaves the outcome's usage unset.
func TestStreamDispatchChargesOnlyTerminalUsage(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	runner := newPacedRunner("done")
	source := newPipeTodoSource()
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1", Runner: runner, SessionID: "dispatch-session", ContextID: "dispatch-session",
		Todos: source, Listed: true,
		Usage: func(context.Context) (agent.Usage, error) {
			return agent.Usage{PromptTokens: 50, CompletionTokens: 5, Cost: 0.1}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	var taskID string
	gotTask := make(chan struct{})
	type result struct {
		outcome agent.DispatchTransportOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := factory.StreamDispatch(context.Background(), agent.DispatchTransportParams{
			Endpoint: server.Endpoint, Card: server.Card, Prompt: "work", ContextID: "dispatch-session",
			OnTask: func(id string) { taskID = id; close(gotTask) },
		})
		done <- result{outcome, err}
	}()
	<-runner.started
	<-gotTask
	source.ch <- todoSnapshot(runTodos(session.Todo{Content: "step one", Status: session.TodoStatusInProgress}))

	conn := indexConn(t, factory)
	require.Eventually(t, func() bool {
		descriptors, err := conn.ListAgents(t.Context())
		return err == nil && len(descriptors) == 1 && descriptors[0].Usage != nil
	}, 10*time.Second, 10*time.Millisecond, "the progress event carried usage to the index")

	require.NoError(t, factory.CancelDispatch(t.Context(), agent.DispatchCancelParams{
		Endpoint: server.Endpoint, Card: server.Card, TaskID: taskID, Reason: "canceled by user",
	}))
	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, DispatchStatusCanceled, got.outcome.Status)
	require.Nil(t, got.outcome.Usage, "a partial progress reading is never charged to the parent")
}
