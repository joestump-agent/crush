package a2a

import (
	"context"
	"errors"
	"iter"
	"testing"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/charmbracelet/crush/internal/agent"
)

// fakeRunner is a test double for the SessionAgent slice the Executor drives.
type fakeRunner struct {
	result *fantasy.AgentResult
	err    error

	gotCall     agent.SessionAgentCall
	ran         bool
	canceledFor string
}

func (f *fakeRunner) Run(_ context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	f.ran = true
	f.gotCall = call
	return f.result, f.err
}

func (f *fakeRunner) Cancel(sessionID string) { f.canceledFor = sessionID }

func textResult(s string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: s}},
		},
	}
}

func newExecCtx(msg *a2aspec.Message) *a2asrv.ExecutorContext {
	return &a2asrv.ExecutorContext{
		Message:   msg,
		TaskID:    "task-1",
		ContextID: "ctx-1",
	}
}

func collect(seq iter.Seq2[a2aspec.Event, error]) []a2aspec.Event {
	var evs []a2aspec.Event
	for ev, err := range seq {
		if err != nil {
			// The executor reports failures as events, never as the
			// second value of the sequence.
			panic("unexpected error from executor sequence: " + err.Error())
		}
		evs = append(evs, ev)
	}
	return evs
}

// states summarizes an event stream as a slice of task states, using a
// sentinel for artifact events so ordering can be asserted in one shot.
func states(t *testing.T, evs []a2aspec.Event) []a2aspec.TaskState {
	t.Helper()
	const artifactState a2aspec.TaskState = "<artifact>"
	out := make([]a2aspec.TaskState, 0, len(evs))
	for _, ev := range evs {
		switch e := ev.(type) {
		case *a2aspec.Task:
			out = append(out, e.Status.State)
		case *a2aspec.TaskStatusUpdateEvent:
			out = append(out, e.Status.State)
		case *a2aspec.TaskArtifactUpdateEvent:
			out = append(out, artifactState)
		default:
			t.Fatalf("unexpected event type %T", ev)
		}
	}
	return out
}

func statusMessageText(t *testing.T, ev a2aspec.Event) string {
	t.Helper()
	sue, ok := ev.(*a2aspec.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("event is %T, want *TaskStatusUpdateEvent", ev)
	}
	if sue.Status.Message == nil {
		return ""
	}
	return partsText(sue.Status.Message.Parts)
}

func partsText(parts a2aspec.ContentParts) string {
	var s string
	for _, p := range parts {
		s += p.Text()
	}
	return s
}

func TestExecuteHappyPathWithDiff(t *testing.T) {
	runner := &fakeRunner{result: textResult("all done")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "the diff", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	evs := collect(exec.Execute(context.Background(), newExecCtx(msg)))

	const artifactState a2aspec.TaskState = "<artifact>"
	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		artifactState,
		a2aspec.TaskStateCompleted,
	}
	if got := states(t, evs); !equalStates(got, want) {
		t.Fatalf("state sequence = %v, want %v", got, want)
	}

	if !runner.ran {
		t.Error("runner.Run was not called")
	}
	if runner.gotCall.SessionID != "sess-1" {
		t.Errorf("Run SessionID = %q, want sess-1", runner.gotCall.SessionID)
	}
	if runner.gotCall.Prompt != "do the thing" {
		t.Errorf("Run Prompt = %q, want %q", runner.gotCall.Prompt, "do the thing")
	}

	// The artifact carries the diff.
	art := evs[2].(*a2aspec.TaskArtifactUpdateEvent)
	if got := partsText(art.Artifact.Parts); got != "the diff" {
		t.Errorf("artifact text = %q, want %q", got, "the diff")
	}

	// The terminal status carries the agent's text output.
	if got := statusMessageText(t, evs[3]); got != "all done" {
		t.Errorf("completed message = %q, want %q", got, "all done")
	}
}

func TestExecuteNoDiffFunc(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}
	if got := states(t, evs); !equalStates(got, want) {
		t.Fatalf("state sequence = %v, want %v (no artifact expected)", got, want)
	}
}

func TestExecuteEmptyDiffEmitsNoArtifact(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "", nil // clean worktree
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(exec.Execute(context.Background(), newExecCtx(msg)))

	for _, ev := range evs {
		if _, ok := ev.(*a2aspec.TaskArtifactUpdateEvent); ok {
			t.Fatal("emitted an artifact event for an empty diff")
		}
	}
}

func TestExecuteDiffErrorStillCompletes(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "", errors.New("not a git repo")
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}
	if got := states(t, evs); !equalStates(got, want) {
		t.Fatalf("state sequence = %v, want %v (diff error must not fail the run)", got, want)
	}
}

func TestExecuteRunFailure(t *testing.T) {
	runner := &fakeRunner{err: errors.New("model exploded")}
	exec := NewExecutor(runner, "sess-1", WithDiff(func(context.Context) (string, error) {
		return "should not be called", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}
	if got := states(t, evs); !equalStates(got, want) {
		t.Fatalf("state sequence = %v, want %v", got, want)
	}

	// The error is surfaced in the failed status message.
	if got := statusMessageText(t, evs[2]); got != "model exploded" {
		t.Errorf("failed message = %q, want %q", got, "model exploded")
	}
}

func TestExecuteExistingTaskSkipsSubmitted(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	exec := NewExecutor(runner, "sess-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	execCtx := newExecCtx(msg)
	execCtx.StoredTask = &a2aspec.Task{ID: "task-1", ContextID: "ctx-1"}

	evs := collect(exec.Execute(context.Background(), execCtx))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}
	if got := states(t, evs); !equalStates(got, want) {
		t.Fatalf("state sequence = %v, want %v (no submitted for an existing task)", got, want)
	}
}

func TestCancel(t *testing.T) {
	runner := &fakeRunner{}
	exec := NewExecutor(runner, "sess-1")

	evs := collect(exec.Cancel(context.Background(), newExecCtx(nil)))

	if runner.canceledFor != "sess-1" {
		t.Errorf("Cancel session = %q, want sess-1", runner.canceledFor)
	}
	want := []a2aspec.TaskState{a2aspec.TaskStateCanceled}
	if got := states(t, evs); !equalStates(got, want) {
		t.Fatalf("state sequence = %v, want %v", got, want)
	}
}

func TestMessageText(t *testing.T) {
	tests := []struct {
		name string
		msg  *a2aspec.Message
		want string
	}{
		{"nil message", nil, ""},
		{
			"single part",
			a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("hello")),
			"hello",
		},
		{
			"multiple parts joined by newline",
			a2aspec.NewMessage(a2aspec.MessageRoleUser,
				a2aspec.NewTextPart("line one"),
				a2aspec.NewTextPart("line two"),
			),
			"line one\nline two",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageText(tc.msg); got != tc.want {
				t.Errorf("messageText = %q, want %q", got, tc.want)
			}
		})
	}
}

func equalStates(a, b []a2aspec.TaskState) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
