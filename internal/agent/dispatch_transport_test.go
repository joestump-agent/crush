package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/stretchr/testify/require"
)

// runnerTransport is the fake A2A host (#343): StartDispatchServer
// records the DispatchServerParams like the real factory, and
// StreamDispatch drives the recorded runner with the recorded call,
// mapping the outcome the way the executor does (#342) — a canceled run
// to canceled, an error or a nil result to failed, anything else to
// completed with the response text and the diff artifact, with diff
// capture errors dropped.
type runnerTransport struct {
	mu     sync.Mutex
	served []DispatchServerParams
}

func (f *runnerTransport) StartDispatchServer(ctx context.Context, params DispatchServerParams) (string, any, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, params)
	return "http://127.0.0.1:19999", "fake-card", func() {}, nil
}

// serve records the runner and call the way StartDispatchServer does,
// for tests that drive the run without standing up the server half.
func (f *runnerTransport) serve(params DispatchServerParams) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, params)
}

func (f *runnerTransport) lastServed() DispatchServerParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served[len(f.served)-1]
}

func (f *runnerTransport) StreamDispatch(ctx context.Context, _ DispatchTransportParams) (DispatchTransportOutcome, error) {
	params := f.lastServed()
	result, err := params.Runner.Run(ctx, params.Call)
	switch {
	case errors.Is(err, context.Canceled):
		return DispatchTransportOutcome{Status: transportStatusCanceled}, nil
	case err != nil:
		return DispatchTransportOutcome{Status: transportStatusFailed, Text: err.Error()}, nil
	case result == nil:
		return DispatchTransportOutcome{Status: transportStatusFailed, Text: "agent session did not start a turn (busy or canceled)"}, nil
	}
	outcome := DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   subAgentOutput(result),
	}
	if params.Diff != nil {
		// The wire carries the diff verdict itself (#361): the diff, or
		// the capture error when it failed.
		diff, derr := params.Diff(ctx)
		switch {
		case derr != nil:
			outcome.DiffError = derr.Error()
		case diff != "":
			outcome.Diff = diff
		}
	}
	return outcome, nil
}

// gatedServingTransport is the a2a factory's test double: it starts
// "servers" like fakeServerStarter and also implements the transport
// seam (#71), gating the stream so a test can inject mid-run exactly as
// against the real loopback server.
type gatedServingTransport struct {
	fakeServerStarter

	gate    chan struct{}
	outcome DispatchTransportOutcome
	err     error

	// taskID, when set, is reported through the stream params' OnTask
	// before the gate opens — the first-event stamp the coordinator
	// records on the registry entry (#349).
	taskID string

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
	if f.taskID != "" && p.OnTask != nil {
		p.OnTask(f.taskID)
	}
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

	entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
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
		e, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && e.Status.IsTerminal() && e.Endpoint == "" && e.AgentCard == nil
	}, 10*time.Second, 50*time.Millisecond)

	e, ok := c.dispatchRegistry().Get(handle.DispatchID)
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

// The served task's ID lands on the registry entry from the stream's
// first event onward (#349): stamped mid-run, while the stream is
// still open, and kept after the terminal teardown clears the endpoint
// and card, so the run stays recoverable through tasks/resubscribe and
// tasks/get.
func TestDispatchStreamStampsRegistryTaskID(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	transport := newGatedServingTransport(DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   "done, all fixed",
	})
	transport.taskID = "task-123"
	c.SetDispatchServerStarter(transport)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "tester",
	}))

	require.Eventually(t, func() bool {
		e, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && e.TaskID == "task-123"
	}, 10*time.Second, 20*time.Millisecond, "the task ID must stamp the entry mid-run")

	close(transport.gate)
	// Wait for terminal AND the teardown's endpoint clear — the status
	// is stamped just before the run's defers tear the server down, so
	// both together are the completion signal here.
	require.Eventually(t, func() bool {
		e, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && e.Status.IsTerminal() && e.Endpoint == "" && e.AgentCard == nil
	}, 10*time.Second, 50*time.Millisecond)

	e, ok := c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusCompleted, e.Status)
	require.Equal(t, "task-123", e.TaskID, "the task ID survives the terminal teardown")
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

	require.Eventually(t, func() bool {
		entries := c.dispatchRegistry().List()
		return len(entries) > 0 && entries[0].Status == dispatch.StatusFailed
	}, 10*time.Second, 50*time.Millisecond)
	entries := c.dispatchRegistry().List()
	require.Contains(t, entries[0].Result.Error, "connection reset")
	// No fallback double-run.
	require.False(t, agent.ranOnce())
}

// A stream error before a terminal state cancels the dispatched agent
// before the run tears down (#344): the served task runs on a detached
// context, so without the explicit Cancel the agent would keep running
// unsupervised — watchdog stopped, permission bridge closed — while the
// registry records failed with the transport error.
func TestDispatchTransportStreamErrorCancelsRun(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	transport := newGatedServingTransport(DispatchTransportOutcome{})
	transport.err = errors.New("SSE stream error: context deadline exceeded")
	c.SetDispatchServerStarter(transport)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	transport.waitStreamed(t)
	close(transport.gate)

	require.Eventually(t, func() bool {
		entries := c.dispatchRegistry().List()
		return len(entries) > 0 && entries[0].Status == dispatch.StatusFailed
	}, 10*time.Second, 50*time.Millisecond)
	entries := c.dispatchRegistry().List()
	require.Contains(t, entries[0].Result.Error, "SSE stream error")
	// The orphaned run was canceled with the dispatch's session id —
	// before the failed outcome was recorded.
	require.Equal(t, []string{handle.SessionID}, agent.cancels())
	// The cancel did not double-run the prompt directly.
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

// assembleFromTransport runs the transported outcome through the same
// terminal assembly the coordinator applies, so the mapping tests below
// exercise what production executes.
func assembleFromTransport(t *testing.T, run dispatchRun, outcome DispatchTransportOutcome) dispatch.DispatchResult {
	t.Helper()
	c := newDispatchTestCoordinator(t, testEnv(t))
	return c.assembleTerminalDispatchResult(t.Context(), run, dispatchNaturalOutcomeFromTransport(outcome))
}

// The outcome mapping mirrors the direct path's semantics: completed
// keeps findings and diff, failed records the reason, canceled stays
// failed (parity — StatusKilled is wander kill's, #316), and the wire
// carries the diff verdict itself (#361): a capture error surfaces as
// "(diff unavailable: ...)", an arrived diff is used as-is, and nothing
// on the wire is "(no changes)" — there is no in-process re-diff.
func TestDispatchFromTransportOutcome(t *testing.T) {
	t.Parallel()

	repo := newTestRepoForTransport(t)
	reg := dispatch.NewAgentRegistry()
	ws, err := dispatch.NewGitWorktreeProvider(repo, filepath.Join(repo, "worktrees"), reg)
	require.NoError(t, err)
	entry := provisionProviderEntry(t, ws, reg, dispatch.ProvisionOptions{})
	run := dispatchRun{
		reg:       reg,
		provider:  ws,
		entry:     entry,
		sessionID: "session-x",
	}
	reg.SetHandle(entry.ID, "tester")

	completed := assembleFromTransport(t, run, DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   "all done",
		Diff:   "--- a/x\n+++ b/x\n@@\n+y",
	})
	require.Equal(t, dispatch.StatusCompleted, completed.Status)
	require.Equal(t, "all done", completed.KeyFindings)
	require.Contains(t, completed.DiffSummary, "+++ b/x")
	require.Equal(t, "tester", completed.Handle, "the handle reads back from the registry, not the stale snapshot")
	require.Equal(t, "(no changes)", assembleFromTransport(t, run, DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   "nothing changed",
	}).DiffSummary)

	// A wire-carried capture error (#361) maps onto the same "(diff
	// unavailable: ...)" summary the direct path produces, with the run
	// still completing.
	diffErr := assembleFromTransport(t, run, DispatchTransportOutcome{
		Status:    transportStatusCompleted,
		Text:      "done, but the diff blew up",
		DiffError: "not a git repo",
	})
	require.Equal(t, dispatch.StatusCompleted, diffErr.Status)
	require.Equal(t, "done, but the diff blew up", diffErr.KeyFindings)
	require.Contains(t, diffErr.DiffSummary, "(diff unavailable: not a git repo)")

	failed := assembleFromTransport(t, run, DispatchTransportOutcome{
		Status: transportStatusFailed,
		Text:   "boom",
	})
	require.Equal(t, dispatch.StatusFailed, failed.Status)
	require.Equal(t, "boom", failed.Error)

	canceled := assembleFromTransport(t, run, DispatchTransportOutcome{
		Status: transportStatusCanceled,
		Text:   "user asked",
	})
	require.Equal(t, dispatch.StatusFailed, canceled.Status)
	require.Contains(t, canceled.Error, "canceled")
	require.Contains(t, canceled.Error, "user asked")

	unknown := assembleFromTransport(t, run, DispatchTransportOutcome{Status: "weird"})
	require.Equal(t, dispatch.StatusFailed, unknown.Status)

	// A recorded kill with no run error means a late kill after a natural
	// completion: the completion wins, the kill reason is discarded.
	lateKill := &dispatchKill{}
	lateKill.kill(dispatch.ReasonHardTimeout)
	run.kill = lateKill
	require.Equal(t, dispatch.StatusCompleted, assembleFromTransport(t, run, DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   "all done",
	}).Status)

	// A loop stop records the tool-loop kill reason in-process, over the
	// wire it still reads completed: the assembly turns it into a kill.
	loopStop := &dispatchKill{}
	loopStop.kill(dispatch.ReasonToolLoop)
	run.kill = loopStop
	looped := assembleFromTransport(t, run, DispatchTransportOutcome{
		Status: transportStatusCompleted,
		Text:   "all done",
	})
	require.Equal(t, dispatch.StatusKilled, looped.Status)
	require.Equal(t, dispatch.ReasonToolLoop, looped.KilledReason)
}

// newTestRepoForTransport builds a throwaway git repo for registry-level
// tests that do not dispatch through the tool.
func newTestRepoForTransport(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	return dir
}

// TestDispatchParityAcrossPaths pins #343: every terminal outcome a
// dispatched run can reach reads the same on the direct in-process path
// and the served transport path — same status, kill reason, diff
// summary and handle — because both paths assemble through one set of
// rules.
func TestDispatchParityAcrossPaths(t *testing.T) {
	t.Parallel()

	type want struct {
		status    dispatch.Status
		reason    string
		summaryOf func(f *wanderKillFixture) string
	}
	scenarios := []struct {
		name  string
		build func(t *testing.T, f *wanderKillFixture)
		check func(t *testing.T, f *wanderKillFixture) want
	}{
		{
			name: "hard timeout",
			build: func(t *testing.T, f *wanderKillFixture) {
				blocked := f.runModel.(*blockingScriptedModel)
				done := make(chan struct{})
				go func() {
					defer close(done)
					f.runDispatchSync(t)
				}()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					close(blocked.hold)
					t.Fatal("hard-timeout kill did not end the dispatch")
				}
			},
			check: func(t *testing.T, f *wanderKillFixture) want {
				return want{
					status: dispatch.StatusKilled,
					reason: dispatch.ReasonHardTimeout,
					summaryOf: func(f *wanderKillFixture) string {
						return "(no changes)"
					},
				}
			},
		},
		{
			name: "ignored nudges",
			build: func(t *testing.T, f *wanderKillFixture) {
				f.runDispatchSync(t)
			},
			check: func(t *testing.T, f *wanderKillFixture) want {
				return want{
					status: dispatch.StatusKilled,
					reason: dispatch.ReasonIgnoredNudges,
					summaryOf: func(f *wanderKillFixture) string {
						return "(no changes)"
					},
				}
			},
		},
		{
			name: "tool loop",
			build: func(t *testing.T, f *wanderKillFixture) {
				f.runDispatchSync(t)
			},
			check: func(t *testing.T, f *wanderKillFixture) want {
				return want{
					status: dispatch.StatusKilled,
					reason: dispatch.ReasonToolLoop,
					summaryOf: func(f *wanderKillFixture) string {
						return "(no changes)"
					},
				}
			},
		},
		{
			name: "diff error",
			build: func(t *testing.T, f *wanderKillFixture) {
				require.NoError(t, os.RemoveAll(f.entry.Path), "a vanished workspace makes diff capture fail")
				f.runDispatchSync(t)
			},
			check: func(t *testing.T, f *wanderKillFixture) want {
				return want{
					status: dispatch.StatusCompleted,
					summaryOf: func(f *wanderKillFixture) string {
						return "(diff unavailable:"
					},
				}
			},
		},
		{
			name: "natural completion",
			build: func(t *testing.T, f *wanderKillFixture) {
				f.runDispatchSync(t)
			},
			check: func(t *testing.T, f *wanderKillFixture) want {
				return want{
					status: dispatch.StatusCompleted,
					summaryOf: func(f *wanderKillFixture) string {
						return "(no changes)"
					},
				}
			},
		},
		{
			name: "late kill",
			build: func(t *testing.T, f *wanderKillFixture) {
				f.kill.kill(dispatch.ReasonHardTimeout)
				f.runDispatchSync(t)
			},
			check: func(t *testing.T, f *wanderKillFixture) want {
				return want{
					status: dispatch.StatusCompleted,
					summaryOf: func(f *wanderKillFixture) string {
						return "(no changes)"
					},
				}
			},
		},
	}

	for _, path := range []string{"direct", "transport"} {
		for _, sc := range scenarios {
			t.Run(path+"/"+sc.name, func(t *testing.T) {
				t.Parallel()
				var f *wanderKillFixture
				switch sc.name {
				case "hard timeout":
					model := &scriptedModel{steps: []scriptedStep{
						{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
						{text: "done"},
					}}
					blocked := &blockingScriptedModel{scriptedModel: model, hold: make(chan struct{})}
					settings := config.TodoEnforcementSettings{Enabled: false, HardTimeout: 200 * time.Millisecond}
					f = newWanderKillFixture(t, model, settings)
					f.runModel = blocked
					f.buildDispatched(t, blocked, settings, nil)
				case "ignored nudges":
					var steps []scriptedStep
					for range 3 {
						steps = append(steps, scriptedStep{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}})
					}
					steps = append(steps, scriptedStep{text: "done"})
					settings := config.TodoEnforcementSettings{Enabled: true, NudgeThreshold: 1, KillAfterNudges: 1}
					f = newWanderKillFixture(t, &scriptedModel{steps: steps}, settings)
				case "tool loop":
					var steps []scriptedStep
					for range 12 {
						steps = append(steps, scriptedStep{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}})
					}
					steps = append(steps, scriptedStep{text: "done"})
					settings := config.TodoEnforcementSettings{Enabled: true, NudgeThreshold: 50, KillAfterNudges: 0}
					f = newWanderKillFixture(t, &scriptedModel{steps: steps}, settings)
				default:
					settings := config.TodoEnforcementSettings{}
					f = newWanderKillFixture(t, &scriptedModel{steps: []scriptedStep{{text: "all done"}}}, settings)
				}
				if path == "transport" {
					f.wireTransport(t, &runnerTransport{})
				}
				sc.build(t, f)

				w := sc.check(t, f)
				entry, ok := f.reg.Get(f.entry.ID)
				require.True(t, ok)
				require.NotNil(t, entry.Result)
				require.Equal(t, w.status, entry.Result.Status, "both paths must report the same status")
				require.Equal(t, w.reason, entry.Result.KilledReason, "both paths must report the same kill reason")
				require.Equal(t, "tester", entry.Result.Handle, "both paths must carry the handle")
				require.Contains(t, entry.Result.DiffSummary, w.summaryOf(f), "both paths must report the same diff outcome")
			})
		}
	}
}
