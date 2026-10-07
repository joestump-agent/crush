package main

import (
	"context"
	"sync/atomic"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
)

// ScriptedRunner is the deterministic a2a.Runner the TCK harness serves:
// every prompt gets the same completed answer, so the TCK's core
// operation tests see a stable SUT (#363).
type ScriptedRunner struct {
	// Text is the completion text every run answers with.
	Text string
	// Delay, when non-zero, is how long Run takes before returning —
	// wide enough for the TCK's in-flight task-state polls.
	Delay time.Duration

	// running tracks whether a scripted run is in flight, so steers can
	// be accepted exactly while the session is busy (#351).
	running atomic.Int32
}

var _ interface {
	Run(context.Context, agent.SessionAgentCall) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
	EnqueueWhenBusy(call agent.SessionAgentCall) bool
} = (*ScriptedRunner)(nil)

// Run implements a2a.Runner.
func (r *ScriptedRunner) Run(ctx context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	r.running.Add(1)
	defer r.running.Add(-1)
	if r.Delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.Delay):
		}
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: r.Text}},
		},
	}, nil
}

// Cancel implements a2a.Runner: a scripted run has nothing to cancel.
func (r *ScriptedRunner) Cancel(sessionID string) {}

// EnqueueWhenBusy implements a2a.Runner: while the scripted run is in
// flight a steer is accepted and folded into the answer already being
// scripted — the harness has exactly one — and once Run has returned
// the session is over, so the message is refused and the executor
// rejects the steer's task (#351).
func (r *ScriptedRunner) EnqueueWhenBusy(call agent.SessionAgentCall) bool {
	if r.running.Load() == 0 {
		return false
	}
	if call.OnConsumed != nil {
		call.OnConsumed(true)
	}
	return true
}

// ScriptedTodos is the a2a.TodoSource the harness serves: one snapshot
// — a two-item checklist, one item in progress — on the first
// subscribe, so the TCK sees a TaskStatusUpdateEvent with todo
// metadata (#174).
type ScriptedTodos struct{}

var _ interface {
	SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot
} = (*ScriptedTodos)(nil)

// SubscribeSessionTodos implements a2a.TodoSource.
func (t *ScriptedTodos) SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot {
	ch := make(chan dispatch.TodoSnapshot, 1)
	snap := dispatch.TodoSnapshot{
		Entry:       dispatch.Entry{ID: sessionID, SessionID: sessionID},
		CurrentTodo: "Serving the TCK scenario",
		Todos: []session.Todo{
			{Content: "Receive the TCK prompt", Status: session.TodoStatusCompleted},
			{Content: "Answer with the scripted result", Status: session.TodoStatusInProgress, ActiveForm: "Serving the TCK scenario"},
		},
	}
	select {
	case ch <- snap:
	case <-ctx.Done():
	}
	return ch
}

// ScriptedDiff is the a2a.DiffFunc the harness serves: a small, valid
// unified diff, so the TCK's completion artifact is inspectable.
func ScriptedDiff(context.Context) (string, error) {
	return `diff --git a/README.md b/README.md
index e69de29..980f48b 100644
--- a/README.md
+++ b/README.md
@@ -0,0 +1 @@
+# TCK harness scripted change
`, nil
}
