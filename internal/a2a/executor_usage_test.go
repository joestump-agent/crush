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
)

// fixedUsage is the usage the closure reports in these tests; the values
// are distinct enough to catch a swap between fields.
var fixedUsage = agent.Usage{
	Model:            "test-model",
	Provider:         "test-provider",
	PromptTokens:     1200,
	CompletionTokens: 340,
	Cost:             0.042,
}

// executedTerminal runs the executor to its single terminal status event
// and returns it.
func executedTerminal(t *testing.T, exec *Executor) *a2aspec.TaskStatusUpdateEvent {
	t.Helper()
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
	var terminal *a2aspec.TaskStatusUpdateEvent
	for _, ev := range evs {
		if sue, ok := ev.(*a2aspec.TaskStatusUpdateEvent); ok && sue.Status.State != a2aspec.TaskStateSubmitted &&
			sue.Status.State != a2aspec.TaskStateWorking {
			terminal = sue
		}
	}
	require.NotNil(t, terminal, "the executor must emit exactly one terminal status")
	return terminal
}

// terminalUsage decodes the usage/v1 extension value off a terminal status
// event, reporting ok=false when the event carries none.
func terminalUsage(t *testing.T, ev *a2aspec.TaskStatusUpdateEvent) (agent.Usage, bool) {
	t.Helper()
	meta := ev.Meta()
	if _, ok := meta[UsageExt.URI]; !ok {
		return agent.Usage{}, false
	}
	usage, err := Decode[agent.Usage](meta, UsageExt)
	require.NoError(t, err)
	return usage, true
}

// Every post-run terminal status — Completed, both Failed branches, and
// the out-of-band Canceled — carries the usage/v1 payload equal to the
// closure's reading of the dispatch session's totals (#364). The mid-run
// Cancel status carries none, and nothing about a usage failure can fail
// the run.
func TestExecuteAttachesUsageToTerminalStates(t *testing.T) {
	t.Parallel()

	usage := func(context.Context) (agent.Usage, error) {
		return fixedUsage, nil
	}

	t.Run("completed", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(&fakeRunner{result: textResult("done")}, "sess-1", WithUsage(usage))
		got, ok := terminalUsage(t, executedTerminal(t, exec))
		require.True(t, ok, "the terminal Completed status carries usage metadata")
		require.Equal(t, fixedUsage, got)
	})

	t.Run("run failed", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(&fakeRunner{err: errors.New("provider exploded")}, "sess-1", WithUsage(usage))
		terminal := executedTerminal(t, exec)
		require.Equal(t, a2aspec.TaskStateFailed, terminal.Status.State)
		got, ok := terminalUsage(t, terminal)
		require.True(t, ok, "the terminal Failed status carries usage metadata")
		require.Equal(t, fixedUsage, got)
	})

	t.Run("no turn ran", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(&fakeRunner{}, "sess-1", WithUsage(usage))
		terminal := executedTerminal(t, exec)
		require.Equal(t, a2aspec.TaskStateFailed, terminal.Status.State)
		got, ok := terminalUsage(t, terminal)
		require.True(t, ok, "the busy-or-canceled Failed status carries usage metadata")
		require.Equal(t, fixedUsage, got)
	})

	t.Run("out of band cancel", func(t *testing.T) {
		t.Parallel()
		runner := &fakeRunner{err: context.Canceled}
		exec := NewExecutor(runner, "sess-1",
			WithUsage(usage),
			WithCancelReason(func() string { return "wander kill: hard timeout" }))
		terminal := executedTerminal(t, exec)
		require.Equal(t, a2aspec.TaskStateCanceled, terminal.Status.State)
		got, ok := terminalUsage(t, terminal)
		require.True(t, ok, "the out-of-band Canceled status carries usage metadata")
		require.Equal(t, fixedUsage, got)
	})

	t.Run("own cancel carries none", func(t *testing.T) {
		t.Parallel()
		runner := &blockingCancelRunner{started: make(chan struct{}), kill: make(chan struct{})}
		exec := NewExecutor(runner, "sess-1", WithUsage(usage))
		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
		evsCh := make(chan struct{}, 1)
		go func() {
			for range exec.Execute(context.Background(), newExecCtx(msg)) {
			}
			evsCh <- struct{}{}
		}()
		<-runner.started
		cancelEvs := collect(t, exec.Cancel(context.Background(), newExecCtx(nil)))
		select {
		case <-evsCh:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the canceled run to end")
		}
		require.Len(t, cancelEvs, 1)
		_, ok := terminalUsage(t, cancelEvs[0].(*a2aspec.TaskStatusUpdateEvent))
		require.False(t, ok, "the mid-run Cancel status never carries usage: the run's totals are not final")
	})
}

// A usage closure that fails, or an unset one, leaves the terminal status
// without usage metadata — the run's outcome is untouched either way
// (#364's missing-usage acceptance path).
func TestExecuteUsageFailureShipsNoMetadata(t *testing.T) {
	t.Parallel()

	t.Run("closure errors", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(&fakeRunner{result: textResult("done")}, "sess-1", WithUsage(
			func(context.Context) (agent.Usage, error) {
				return agent.Usage{}, errors.New("session row gone")
			}))
		_, ok := terminalUsage(t, executedTerminal(t, exec))
		require.False(t, ok, "a failing usage closure ships no usage metadata")
	})

	t.Run("no usage wired", func(t *testing.T) {
		t.Parallel()
		exec := NewExecutor(&fakeRunner{result: textResult("done")}, "sess-1")
		_, ok := terminalUsage(t, executedTerminal(t, exec))
		require.False(t, ok, "no usage func means no usage metadata")
	})
}

// The full traceparent round trip (#364): the parent puts a W3C
// traceparent on the dispatch call's context, the client interceptor
// sends it as the traceparent request header, the server propagator
// lifts it into the served request's context, and the executor stamps
// its trace-id segment onto the usage payload — which the client decodes
// off the terminal status, together with the usage the server closure
// reported.
func TestStreamDispatchCarriesUsageAndTraceparent(t *testing.T) {
	tp, err := agent.NewTraceparent()
	require.NoError(t, err)

	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{result: textResult("done")},
		SessionID: "dispatch-session",
		Usage: func(context.Context) (agent.Usage, error) {
			return fixedUsage, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	ctx := agent.WithTraceparent(t.Context(), tp)
	outcome, err := NewServerFactory().StreamDispatch(ctx, agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)

	require.NotNil(t, outcome.Usage, "the terminal status carried the usage/v1 payload")
	require.Equal(t, fixedUsage.Model, outcome.Usage.Model)
	require.Equal(t, fixedUsage.Provider, outcome.Usage.Provider)
	require.Equal(t, fixedUsage.PromptTokens, outcome.Usage.PromptTokens)
	require.Equal(t, fixedUsage.CompletionTokens, outcome.Usage.CompletionTokens)
	require.InDelta(t, fixedUsage.Cost, outcome.Usage.Cost, 1e-9)
	require.Equal(t, agent.TraceIDFromTraceparent(tp), outcome.Usage.TraceID,
		"the usage payload echoes the parent call's trace ID")
}

// A served dispatch with no usage wired completes with no usage on the
// outcome: the parent's cost is left untouched, not zeroed (#364).
func TestStreamDispatchWithoutUsage(t *testing.T) {
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{result: textResult("done")},
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
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Nil(t, outcome.Usage)
}

// The stream-side metadata decoder decodes the usage/v1 payload onto the
// outcome; a declared key whose value does not fit is dropped without
// failing the stream (#364).
func TestMetadataDecoderUsage(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
	})
	decoder := newMetadataDecoder(card)

	// A malformed value is dropped.
	var outcome agent.DispatchTransportOutcome
	decoder.apply(&outcome, &a2aspec.TaskStatusUpdateEvent{
		Status:   a2aspec.TaskStatus{State: a2aspec.TaskStateCompleted},
		Metadata: map[string]any{UsageExtensionURI: "garbage"},
	})
	require.Nil(t, outcome.Usage)

	// A well-formed value decodes, and the last one wins.
	encoded, err := Encode(UsageExt, fixedUsage)
	require.NoError(t, err)
	decoder.apply(&outcome, &a2aspec.TaskStatusUpdateEvent{
		Status:   a2aspec.TaskStatus{State: a2aspec.TaskStateCompleted},
		Metadata: map[string]any{UsageExtensionURI: encoded},
	})
	require.NotNil(t, outcome.Usage)
	require.Equal(t, fixedUsage, *outcome.Usage)
}
