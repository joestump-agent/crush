package a2a

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
// served dispatch over the loopback wire — prompt out, SSE events back —
// and returns the terminal outcome with the agent's text, the artifact
// diff, and the observed Working progress count.
func TestStreamDispatchCompletedWithArtifactAndProgress(t *testing.T) {
	runner := &fakeRunner{result: textResult("done, two files changed")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
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
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	factory := NewServerFactory()
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
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{err: errors.New("provider exploded")},
		SessionID: "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
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
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{panicValue: errors.New("adapter blew up")},
		SessionID: "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
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
	runner := &blockingCancelRunner{started: make(chan struct{}), kill: make(chan struct{})}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		CancelReason: func() string {
			return "wander kill: hard timeout"
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	go func() {
		<-runner.started
		close(runner.kill)
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	outcome, err := NewServerFactory().StreamDispatch(ctx, agent.DispatchTransportParams{
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
	factory := NewServerFactory()

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

// An 11 MB diff survives the wire end to end (#361): the executor's
// chunks each stay under the SDK's SSE line cap, and the client
// reassembles them byte for byte onto the outcome.
func TestStreamDispatchLargeDiffRoundTrips(t *testing.T) {
	line := strings.Repeat("-", 4096) + "\n"
	var b strings.Builder
	for len(b.String()) < 11*1024*1024 {
		b.WriteString(line)
	}
	diff := b.String()

	runner := &fakeRunner{result: textResult("big change")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return diff, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "change everything",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Len(t, outcome.Diff, len(diff))
	require.Equal(t, diff, outcome.Diff, "the reassembled diff matches byte for byte")
	require.False(t, outcome.DiffTruncated)
	require.Empty(t, outcome.DiffError)
}

// A diff-capture error crosses the wire on the dispatch-result artifact:
// the run still completes, and the outcome carries the error instead of
// a diff (#361).
func TestStreamDispatchDiffErrorStillCompletes(t *testing.T) {
	runner := &fakeRunner{result: textResult("done, but the diff blew up")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return "", errors.New("not a git repo")
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "not a git repo", outcome.DiffError)
	require.Empty(t, outcome.Diff)
}

// The production dispatch client carries no total Timeout (#344): the
// SDK's default three-minute http.Client.Timeout bounds the whole
// exchange including the SSE body, killing every served dispatch that
// runs longer. The per-phase bounds stay, so a dead server still fails
// fast.
func TestDispatchClientHasNoTotalTimeout(t *testing.T) {
	client := dispatchHTTPClient()
	require.Zero(t, client.Timeout, "a total Timeout would re-create the three-minute kill")

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Greater(t, transport.ResponseHeaderTimeout, time.Duration(0))
	require.Greater(t, transport.TLSHandshakeTimeout, time.Duration(0))
}

// A served run outlives a short injected client deadline (#344): the
// response headers arrive inside the deadline, the SSE body streams
// past it, and the terminal outcome still lands.
func TestStreamDispatchOutlivesShortClientDeadline(t *testing.T) {
	runner := &fakeRunner{result: textResult("eventually done"), delay: time.Second}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Todos:     &fakeTodoSource{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	factory := NewServerFactory(WithHTTPClient(&http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 200 * time.Millisecond,
		},
	}))
	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "eventually done", outcome.Text)
}
