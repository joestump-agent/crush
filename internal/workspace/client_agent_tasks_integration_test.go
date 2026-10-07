package workspace_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/workspace"
)

// steeredRunner is a served agent that runs until it is canceled and
// takes every steer it is sent.
type steeredRunner struct {
	mu      sync.Mutex
	steers  []string
	started chan struct{}
	once    sync.Once
}

func (r *steeredRunner) Run(ctx context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *steeredRunner) Cancel(string) {}

func (r *steeredRunner) EnqueueWhenBusy(call agent.SessionAgentCall) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steers = append(r.steers, call.Prompt)
	return true
}

func (r *steeredRunner) steered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steers...)
}

// A client/server TUI follows, steers and cancels a dispatched agent
// through the real chain (#421): the client's workspace, the server's
// proxy routes, the backend's host lookup, and the workspace's A2A host.
func TestClientWorkspaceFollowsSteersAndCancelsThroughTheServer(t *testing.T) {
	xdgIsolate(t)
	rs := newRuntimeServer(t)

	factory := a2a.NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	ready := make(chan struct{})
	close(ready)
	ws := &backend.Workspace{ID: uuid.New().String(), Path: t.TempDir()}
	backend.SetWorkspaceA2AHostForTest(ws, func() (*a2a.ServerFactory, <-chan struct{}) { return factory, ready })
	backend.SetWorkspaceShutdownFnForTest(ws, func() {})
	backend.InsertWorkspaceForTest(rs.srv.Backend(), ws)

	runner := &steeredRunner{started: make(chan struct{})}
	server, err := factory.StartServer(t.Context(), a2a.ServerParams{
		DispatchID: "dispatch-1", Runner: runner, SessionID: "child-1", ContextID: "child-1",
		Name: "tester", Listed: true, ParentSessionID: "parent-1",
	})
	require.NoError(t, err)
	outcome := make(chan agent.DispatchTransportOutcome, 1)
	go func() {
		out, _ := factory.StreamDispatch(context.Background(), agent.DispatchTransportParams{
			Endpoint: server.Endpoint, Card: server.Card, Prompt: "fix the bug", ContextID: "child-1",
		})
		outcome <- out
	}()
	<-runner.started

	cw := workspace.NewClientWorkspace(rs.newClient(t, ws.Path), proto.Workspace{ID: ws.ID, Path: ws.Path})
	t.Cleanup(cw.Shutdown)

	require.Eventually(t, func() bool {
		task, ok := cw.AgentTaskByHandle("parent-1", "tester")
		return ok && task.Status == dispatch.StatusRunning && cw.SendAgentMessage(t.Context(), "parent-1", "tester", "also the docs", nil) == nil
	}, 10*time.Second, 25*time.Millisecond, "the running agent is listed and takes a steer through the server")
	require.Equal(t, []string{"also the docs"}, runner.steered())
	require.Len(t, cw.ListAgentTasks("parent-1"), 1)
	_, ok := cw.AgentTaskByHandle("other-session", "tester")
	require.False(t, ok, "another session's agent is not in scope")

	require.NoError(t, cw.CancelAgentTask(t.Context(), "child-1"), "the cancel key's child session ID resolves")
	select {
	case out := <-outcome:
		require.Equal(t, a2a.DispatchStatusCanceled, out.Status)
	case <-time.After(10 * time.Second):
		t.Fatal("the cancel never ended the dispatch")
	}
	require.Eventually(t, func() bool {
		task, ok := cw.AgentTask("child-1")
		return ok && task.Status == dispatch.StatusKilled
	}, 10*time.Second, 25*time.Millisecond, "the index follows the agent to its end")
}
