package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/stretchr/testify/require"
)

// gatedServingTransport is the a2a factory's test double: it starts
// "servers" like fakeServerStarter and also implements the transport
// seam (#71), gating the stream so a test can inject mid-run exactly as
// against the real loopback server.
type gatedServingTransport struct {
	fakeServerStarter

	gate    chan struct{}
	outcome DispatchTransportOutcome
	err     error

	mu       sync.Mutex
	streamed []DispatchTransportParams
}

func newGatedServingTransport(outcome DispatchTransportOutcome) *gatedServingTransport {
	return &gatedServingTransport{gate: make(chan struct{}), outcome: outcome}
}

func (f *gatedServingTransport) StreamDispatch(ctx context.Context, p DispatchTransportParams) (DispatchTransportOutcome, error) {
	f.mu.Lock()
	f.streamed = append(f.streamed, p)
	f.mu.Unlock()
	select {
	case <-f.gate:
	case <-ctx.Done():
		return DispatchTransportOutcome{}, ctx.Err()
	}
	if f.err != nil {
		return DispatchTransportOutcome{}, f.err
	}
	return f.outcome, nil
}

func (f *gatedServingTransport) waitStreamed(t *testing.T) DispatchTransportParams {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.streamed)
		var last DispatchTransportParams
		if n > 0 {
			last = f.streamed[n-1]
		}
		f.mu.Unlock()
		if n > 0 {
			return last
		}
		select {
		case <-deadline:
			t.Fatal("transport never received the dispatch stream")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The transport swap (#71): a served dispatch runs over the transport
// seam instead of the direct in-process Run, the terminal outcome maps
// onto the DispatchResult (findings from the status message, diff from
// the artifact), the injection queue still steers the same agent
// mid-run, and the server teardown clears the endpoint with the run.
func TestDispatchRunsOverTransportSeam(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	transport := newGatedServingTransport(DispatchTransportOutcome{
		Status:        transportStatusCompleted,
		Text:          "done, all fixed",
		Diff:          "--- a/x\n+++ b/x\n@@\n+y",
		WorkingEvents: 3,
	})
	c.SetDispatchServerStarter(transport)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "tester",
	}))

	ws, _ := c.dispatchWorkspace()
	entry, ok := ws.Get(handle.DispatchID)
	require.True(t, ok)
	require.NotEmpty(t, entry.Endpoint, "the dispatch must be served for the transport to drive it")

	// The stream carries the registry's endpoint/card and the prompt.
	p := transport.waitStreamed(t)
	require.Equal(t, entry.Endpoint, p.Endpoint)
	require.Equal(t, entry.AgentCard, p.Card)
	require.Equal(t, "fix the bug", p.Prompt)

	// The direct run never fired — on the transported path the
	// coordinator does not also run the agent itself; the server side
	// owns the turn.
	require.False(t, agent.ranOnce(), "the transported dispatch must not also run directly")

	// Steer mid-run through the injection seam — the queue is agnostic
	// to which side of the protocol boundary drives the turn.
	require.NoError(t, c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: "stop writing Rust"}))
	require.Len(t, agent.injected(), 1)

	close(transport.gate)
	// Wait for terminal AND the teardown's endpoint clear — the terminal
	// status is stamped just before the run's defers tear the server
	// down, so both are the run's completion signal here.
	require.Eventually(t, func() bool {
		e, ok := ws.Get(handle.DispatchID)
		return ok && e.Status.IsTerminal() && e.Endpoint == "" && e.AgentCard == nil
	}, 10*time.Second, 50*time.Millisecond)

	e, ok := ws.Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusCompleted, e.Status)
	require.NotNil(t, e.Result)
	require.Equal(t, "done, all fixed", e.Result.KeyFindings)
	require.Contains(t, e.Result.DiffSummary, "+++ b/x")
	require.Equal(t, "tester", e.Result.Handle)
	// The server is torn down with the run.
	require.Empty(t, e.Endpoint)
	require.Nil(t, e.AgentCard)
}

// A transport failure before any terminal state fails the dispatch with
// the transport error — it must not fall back and double-run the prompt.
func TestDispatchTransportStreamErrorFails(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	transport := newGatedServingTransport(DispatchTransportOutcome{})
	transport.err = errors.New("connection reset")
	c.SetDispatchServerStarter(transport)
	tool := c.dispatchTool()

	decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	transport.waitStreamed(t)
	close(transport.gate)

	ws, _ := c.dispatchWorkspace()
	require.Eventually(t, func() bool {
		entries := ws.List()
		return len(entries) > 0 && entries[0].Status == dispatch.StatusFailed
	}, 10*time.Second, 50*time.Millisecond)
	entries := ws.List()
	require.Contains(t, entries[0].Result.Error, "connection reset")
	// No fallback double-run.
	require.False(t, agent.ranOnce())
}

// A starter that is not a transport keeps the direct in-process path:
// the server exists (endpoint stamped) but the coordinator drives the
// agent itself, as before #71.
func TestServedButNotTransportedRunsDirectly(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	c.SetDispatchServerStarter(&fakeServerStarter{})
	tool := c.dispatchTool()

	decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)
	require.True(t, agent.ranOnce(), "a served dispatch without a transport runs directly")

	close(agent.gate)
}

// The outcome mapping mirrors the direct path's semantics: completed
// keeps findings and diff, failed records the reason, canceled stays
// failed (parity — StatusKilled is wander kill's, #316).
func TestDispatchFromTransportOutcome(t *testing.T) {
	t.Parallel()

	repo := newTestRepoForTransport(t)
	ws, err := dispatch.NewWorkspace(repo)
	require.NoError(t, err)
	entry, err := ws.Provision(t.Context(), dispatch.ProvisionOptions{})
	require.NoError(t, err)
	run := dispatchRun{
		workspace: ws,
		entry:     entry,
		sessionID: "session-x",
	}
	ws.SetHandle(entry.ID, "tester")

	completed := dispatchFromTransportOutcome(run, DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   "all done",
		Diff:   "--- a/x\n+++ b/x\n@@\n+y",
	})
	require.Equal(t, dispatch.StatusCompleted, completed.Status)
	require.Equal(t, "all done", completed.KeyFindings)
	require.Contains(t, completed.DiffSummary, "+++ b/x")
	require.Equal(t, "tester", completed.Handle, "the handle reads back from the registry, not the stale snapshot")
	require.Equal(t, "(no changes)", dispatchFromTransportOutcome(run, DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   "nothing changed",
	}).DiffSummary)

	failed := dispatchFromTransportOutcome(run, DispatchTransportOutcome{
		Status: transportStatusFailed,
		Text:   "boom",
	})
	require.Equal(t, dispatch.StatusFailed, failed.Status)
	require.Equal(t, "boom", failed.Error)

	canceled := dispatchFromTransportOutcome(run, DispatchTransportOutcome{
		Status: transportStatusCanceled,
		Text:   "user asked",
	})
	require.Equal(t, dispatch.StatusFailed, canceled.Status)
	require.Contains(t, canceled.Error, "canceled")
	require.Contains(t, canceled.Error, "user asked")

	unknown := dispatchFromTransportOutcome(run, DispatchTransportOutcome{Status: "weird"})
	require.Equal(t, dispatch.StatusFailed, unknown.Status)
}

// newTestRepoForTransport builds a throwaway git repo for registry-level
// tests that do not dispatch through the tool.
func newTestRepoForTransport(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	return dir
}
