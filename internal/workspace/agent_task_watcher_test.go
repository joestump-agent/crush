package workspace

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// heldRunner is a dispatched agent whose turn holds until released or
// canceled; it accepts steers while the turn is held.
type heldRunner struct {
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}

	once       sync.Once
	cancelOnce sync.Once
	mu         sync.Mutex
	steers     []string
	running    bool
}

func newHeldRunner() *heldRunner {
	return &heldRunner{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
}

func (r *heldRunner) Run(ctx context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	r.mu.Lock()
	r.running = true
	r.mu.Unlock()
	r.once.Do(func() { close(r.started) })
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()
	select {
	case <-r.release:
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}}, nil
	case <-r.canceled:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *heldRunner) Cancel(string) { r.cancelOnce.Do(func() { close(r.canceled) }) }

func (r *heldRunner) EnqueueWhenBusy(call agent.SessionAgentCall) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return false
	}
	r.steers = append(r.steers, call.Prompt)
	if call.OnConsumed != nil {
		go call.OnConsumed(true)
	}
	return true
}

func (r *heldRunner) steered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steers...)
}

// watcherEnv is an A2A host serving one listed dispatch, and a watcher
// following its index the way a workspace does.
type watcherEnv struct {
	host    *a2a.ServerFactory
	server  *a2a.Server
	runner  *heldRunner
	watcher *agentTaskWatcher
	events  chan AgentTask
}

func newWatcherEnv(t *testing.T) *watcherEnv {
	t.Helper()
	host := a2a.NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	env := &watcherEnv{host: host, runner: newHeldRunner(), events: make(chan AgentTask, 64)}
	env.watcher = newAgentTaskWatcher(
		func() (a2a.AgentIndexConn, bool, <-chan struct{}) {
			conn, ok := host.AgentIndexConn()
			return conn, ok, host.Started()
		},
		func() (agentClient, bool) { return host, true },
	)
	env.watcher.start(func(msg tea.Msg) {
		if ev, ok := msg.(pubsub.Event[AgentTask]); ok {
			env.events <- ev.Payload
		}
	})
	t.Cleanup(env.watcher.stop)
	return env
}

// serve stands the dispatch up on the host, listed under parent.
func (e *watcherEnv) serve(t *testing.T, parent, handle string) {
	t.Helper()
	server, err := e.host.StartServer(t.Context(), a2a.ServerParams{
		DispatchID:      "dispatch-1",
		Runner:          e.runner,
		SessionID:       "child-session",
		ContextID:       "child-session",
		Name:            handle,
		Description:     "writes tests",
		Listed:          true,
		ParentSessionID: parent,
	})
	require.NoError(t, err)
	e.server = server
}

// run starts the dispatch's turn the way the coordinator does and waits
// for the agent to be in it.
func (e *watcherEnv) run(t *testing.T) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := e.host.StreamDispatch(context.Background(), agent.DispatchTransportParams{
			Endpoint: e.server.Endpoint, Card: e.server.Card, Prompt: "fix the bug", ContextID: "child-session",
		})
		done <- err
	}()
	<-e.runner.started
	return done
}

// waitFor reads watcher events until one matches.
func (e *watcherEnv) waitFor(t *testing.T, match func(AgentTask) bool) AgentTask {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case task := <-e.events:
			if match(task) {
				return task
			}
		case <-timeout:
			t.Fatal("no matching agent task event")
		}
	}
}

// The watcher waits for the host to come up, then delivers each change
// of a listed dispatch as an AgentTask event and serves reads from what
// it has seen: by parent session, by child session, and by handle
// (#421).
func TestAgentTaskWatcherFollowsTheIndex(t *testing.T) {
	env := newWatcherEnv(t)
	env.serve(t, "parent-1", "tester")

	listed := env.waitFor(t, func(task AgentTask) bool { return task.DispatchID == "dispatch-1" })
	require.Equal(t, dispatch.StatusProvisioned, listed.Status)
	require.Equal(t, "tester", listed.Handle)
	require.Equal(t, "child-session", listed.SessionID)
	require.True(t, listed.Served)

	done := env.run(t)
	running := env.waitFor(t, func(task AgentTask) bool { return task.Status == dispatch.StatusRunning })
	require.Equal(t, "parent-1", running.ParentSessionID)

	require.Len(t, env.watcher.list("parent-1"), 1)
	require.Empty(t, env.watcher.list("parent-2"), "another session's agents are not listed (#399)")
	bySession, ok := env.watcher.bySession("child-session")
	require.True(t, ok)
	require.Equal(t, "dispatch-1", bySession.DispatchID)
	byHandle, ok := env.watcher.byHandle("parent-1", "@Tester")
	require.True(t, ok, "handles resolve in their addressed form too")
	require.Equal(t, "dispatch-1", byHandle.DispatchID)
	_, ok = env.watcher.byHandle("parent-2", "tester")
	require.False(t, ok)

	close(env.runner.release)
	require.NoError(t, <-done)
	completed := env.waitFor(t, func(task AgentTask) bool { return task.Terminal() })
	require.Equal(t, dispatch.StatusCompleted, completed.Status)
	require.False(t, completed.FinishedAt.IsZero())
}

// Steering goes over A2A to the running agent; another session's agent
// and a finished agent are refused with the coordinator's wording
// (#313, #399, #421).
func TestAgentTaskWatcherSteers(t *testing.T) {
	env := newWatcherEnv(t)
	env.serve(t, "parent-1", "tester")
	done := env.run(t)
	env.waitFor(t, func(task AgentTask) bool { return task.Status == dispatch.StatusRunning })

	require.NoError(t, env.watcher.steer(t.Context(), "parent-1", "tester", "also the docs", nil))
	require.Equal(t, []string{"also the docs"}, env.runner.steered())

	err := env.watcher.steer(t.Context(), "parent-2", "tester", "hello", nil)
	require.EqualError(t, err, "no agent with handle @tester in this session; dispatch one first")

	close(env.runner.release)
	require.NoError(t, <-done)
	env.waitFor(t, func(task AgentTask) bool { return task.Terminal() })
	err = env.watcher.steer(t.Context(), "parent-1", "tester", "too late", nil)
	require.ErrorContains(t, err, "finished (completed); task sessions are never continuable")
}

// Cancel goes through tasks/cancel with the user's reason, and the agent
// ends killed (#348, #373, #421).
func TestAgentTaskWatcherCancels(t *testing.T) {
	env := newWatcherEnv(t)
	env.serve(t, "parent-1", "tester")
	done := env.run(t)
	env.waitFor(t, func(task AgentTask) bool { return task.Status == dispatch.StatusRunning })

	require.NoError(t, env.watcher.cancelTask(t.Context(), "@tester"))
	<-env.runner.canceled
	<-done
	killed := env.waitFor(t, func(task AgentTask) bool { return task.Terminal() })
	require.Equal(t, dispatch.StatusKilled, killed.Status)
	require.Equal(t, dispatch.ReasonCanceled, killed.StatusText)

	err := env.watcher.cancelTask(t.Context(), "dispatch-1")
	require.ErrorContains(t, err, "already finished")
}

// A lookup that misses before the stream has delivered its snapshot
// reads the index once instead of refusing (#421).
func TestAgentTaskWatcherRefreshesOnMiss(t *testing.T) {
	host := a2a.NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	runner := newHeldRunner()
	server, err := host.StartServer(t.Context(), a2a.ServerParams{
		DispatchID: "dispatch-1", Runner: runner, SessionID: "child-session", ContextID: "child-session",
		Name: "tester", Listed: true, ParentSessionID: "parent-1",
	})
	require.NoError(t, err)
	go func() {
		_, _ = host.StreamDispatch(context.Background(), agent.DispatchTransportParams{
			Endpoint: server.Endpoint, Card: server.Card, Prompt: "go", ContextID: "child-session",
		})
	}()
	<-runner.started
	t.Cleanup(func() { close(runner.release) })

	// Never started: the cache is empty until a lookup misses.
	w := newAgentTaskWatcher(
		func() (a2a.AgentIndexConn, bool, <-chan struct{}) {
			conn, ok := host.AgentIndexConn()
			return conn, ok, host.Started()
		},
		func() (agentClient, bool) { return host, true },
	)
	require.Empty(t, w.list("parent-1"))
	require.NoError(t, w.steer(t.Context(), "parent-1", "tester", "hello", nil))
	require.Equal(t, []string{"hello"}, runner.steered())
}

// Before the executor names the dispatch's own task, the first message
// on its context would start the dispatch's turn: a steer is refused
// rather than sent (#351, #421).
func TestAgentTaskWatcherRefusesSteerBeforeTheTurn(t *testing.T) {
	env := newWatcherEnv(t)
	env.serve(t, "parent-1", "tester")
	env.waitFor(t, func(task AgentTask) bool { return task.DispatchID == "dispatch-1" })

	err := env.watcher.steer(t.Context(), "parent-1", "tester", "too early", nil)
	require.EqualError(t, err, "no running agent for session child-session; dispatch one first")
	select {
	case <-env.runner.started:
		t.Fatal("a refused steer started the dispatch's turn")
	default:
	}
}

// Steering refuses empty text, and a handle without a session is in no
// session's scope (#399, #421).
func TestAgentTaskWatcherSteerChecks(t *testing.T) {
	env := newWatcherEnv(t)
	env.serve(t, "parent-1", "tester")
	env.run(t)
	env.waitFor(t, func(task AgentTask) bool { return task.Status == dispatch.StatusRunning })
	t.Cleanup(func() { close(env.runner.release) })

	require.EqualError(t, env.watcher.steer(t.Context(), "parent-1", "tester", "  \n", nil), "message text is required")
	_, ok := env.watcher.byHandle("", "tester")
	require.False(t, ok)
	require.Empty(t, env.runner.steered())
}

// An older copy of a descriptor never replaces a newer one, and a copy
// the cache already holds is not published again: a reconnect's snapshot
// repeats no card update (#421).
func TestAgentTaskWatcherKeepsTheNewestCopy(t *testing.T) {
	t.Parallel()
	var published []AgentTask
	w := newAgentTaskWatcher(nil, nil)
	w.send = func(msg tea.Msg) {
		published = append(published, msg.(pubsub.Event[AgentTask]).Payload)
	}

	newer := a2a.AgentDescriptor{ID: "d1", State: a2a.DispatchStatusCompleted, Served: true, Revision: 20}
	older := a2a.AgentDescriptor{ID: "d1", State: a2a.DispatchStatusWorking, Served: true, Revision: 10}
	w.apply(a2a.AgentIndexEvent{Upsert: &newer})
	w.apply(a2a.AgentIndexEvent{Snapshot: []a2a.AgentDescriptor{older}})
	w.apply(a2a.AgentIndexEvent{Snapshot: []a2a.AgentDescriptor{newer}})

	task, ok := w.bySession("")
	require.True(t, ok)
	require.Equal(t, dispatch.StatusCompleted, task.Status, "the stale snapshot did not regress the cache")
	require.Len(t, published, 1, "only the first copy was a change")
}

// A route that went down before its task ended shows as failed, not live
// forever (#421).
func TestAgentTaskUnservedWithoutTerminalIsFailed(t *testing.T) {
	t.Parallel()
	require.Equal(t, dispatch.StatusFailed, agentTaskFromDescriptor(a2a.AgentDescriptor{State: a2a.DispatchStatusWorking}).Status)
	require.Equal(t, dispatch.StatusFailed, agentTaskFromDescriptor(a2a.AgentDescriptor{}).Status)
	require.Equal(t, dispatch.StatusRunning, agentTaskFromDescriptor(a2a.AgentDescriptor{State: a2a.DispatchStatusWorking, Served: true}).Status)
	require.Equal(t, dispatch.StatusCompleted, agentTaskFromDescriptor(a2a.AgentDescriptor{State: a2a.DispatchStatusCompleted}).Status)
}
