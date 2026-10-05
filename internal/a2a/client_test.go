package a2a

import (
	"context"
	"errors"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
)

// fakeTodoSource emits one snapshot per subscription (#174 stand-in).
type fakeTodoSource struct {
	snapshot dispatch.TodoSnapshot
}

func (f *fakeTodoSource) SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot {
	ch := make(chan dispatch.TodoSnapshot, 1)
	ch <- f.snapshot
	return ch
}

// The client half of the protocol boundary (#71): StreamDispatch drives a
// served dispatch over the unix-socket wire — prompt out, SSE events
// back — and returns the terminal outcome with the agent's text, the
// artifact diff, and the observed Working progress count. The same
// factory instance serves and dials (#346): the socket path lives on it.
func TestStreamDispatchCompletedWithArtifactAndProgress(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	runner := &fakeRunner{result: textResult("done, two files changed")}
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return "--- a/x\n+++ b/x\n@@\n+changed", nil
		},
		Todos: &fakeTodoSource{snapshot: dispatch.TodoSnapshot{
			CurrentTodo: "wiring form validation",
			Todos:       []session.Todo{{Content: "wiring form validation", Status: session.TodoStatusInProgress}},
		}},
		Call: agent.SessionAgentCall{NonInteractive: true, MaxOutputTokens: 512},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	// The call template flowed through the wire (#71): the served turn
	// carries the dispatch's shaping, not a minimal test call.
	require.True(t, runner.gotCall.NonInteractive)
	require.Equal(t, int64(512), runner.gotCall.MaxOutputTokens)
	require.Equal(t, "dispatch-session", runner.gotCall.SessionID)
	require.Equal(t, "fix the bug", runner.gotCall.Prompt)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "done, two files changed", outcome.Text)
	require.Contains(t, outcome.Diff, "+++ b/x")
	require.Greater(t, outcome.WorkingEvents, 0, "the SSE stream carried Working progress events")
}

// A failed run maps to the failed outcome with the failure text from the
// terminal status message.
func TestStreamDispatchFailedRun(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{err: errors.New("provider exploded")},
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusFailed, outcome.Status)
	require.Contains(t, outcome.Text, "provider exploded")
}

// A panic in the dispatched runner must surface as a failed outcome
// across the wire, not crash the server process (#345).
func TestStreamDispatchRunnerPanicFails(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{panicValue: errors.New("adapter blew up")},
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusFailed, outcome.Status)
	require.Contains(t, outcome.Text, "panicked")
}

// An out-of-band kill — the ladder or watchdog canceling the agent
// directly, no A2A CancelTask, live server context — must end the SSE
// stream quickly with the canceled outcome and the kill reason, never at
// the caller's deadline (#342).
func TestStreamDispatchOutOfBandCancelEnds(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	runner := &blockingCancelRunner{started: make(chan struct{}), kill: make(chan struct{})}
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		CancelReason: func() string {
			return "wander kill: hard timeout"
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	go func() {
		<-runner.started
		close(runner.kill)
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	outcome, err := factory.StreamDispatch(ctx, agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "the stream must end well before the caller's deadline")
	require.Equal(t, DispatchStatusCanceled, outcome.Status)
	require.Equal(t, "wander kill: hard timeout", outcome.Text)
}

// An unreachable or bogus endpoint is a transport error before any
// terminal state, never a silent success.
func TestStreamDispatchTransportErrors(t *testing.T) {
	factory := NewServerFactory(t.TempDir())

	// No card to build a client from.
	_, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     "not a card",
		Prompt:   "x",
	})
	require.ErrorContains(t, err, "no resolvable agent card")

	// A card advertising an endpoint nothing serves.
	dead := BuildAgentCard(CardParams{Agent: config.Agent{Name: "dead-agent"}, Endpoint: "http://127.0.0.1:1", Transport: a2aspec.TransportProtocolJSONRPC})
	_, err = factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     dead,
		Prompt:   "x",
	})
	require.Error(t, err)

	// An empty prompt never leaves the client.
	_, err = factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     dead,
	})
	require.ErrorContains(t, err, "prompt is empty")
}
