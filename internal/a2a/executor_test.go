package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/session"
)

// fakeRunner is a test double for the SessionAgent slice the Executor drives.
type fakeRunner struct {
	result *fantasy.AgentResult
	err    error

	// panicValue, when non-nil, makes Run panic with it instead of
	// returning (#345).
	panicValue any

	// delay, when non-zero, makes Run take that long before returning,
	// for tests that need a served run slower than a transport deadline.
	delay time.Duration

	// enqueueAccepted reports what EnqueueWhenBusy answers (#351): true
	// accepts the steer into enqueuedCalls, false refuses it the way an
	// ended run does.
	enqueueAccepted bool

	mu            sync.Mutex
	gotCall       agent.SessionAgentCall
	ran           bool
	canceledFor   string
	enqueuedCalls []agent.SessionAgentCall
}

func (f *fakeRunner) Run(_ context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	f.ran = true
	f.gotCall = call
	f.mu.Unlock()
	if f.panicValue != nil {
		panic(f.panicValue)
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.result, f.err
}

func (f *fakeRunner) Cancel(sessionID string) { f.canceledFor = sessionID }

// EnqueueWhenBusy answers with enqueueAccepted (#351), recording the
// calls it accepts so a test can release them through consume.
func (f *fakeRunner) EnqueueWhenBusy(call agent.SessionAgentCall) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enqueueAccepted {
		return false
	}
	f.enqueuedCalls = append(f.enqueuedCalls, call)
	return true
}

// enqueued returns a copy of the calls EnqueueWhenBusy accepted.
func (f *fakeRunner) enqueued() []agent.SessionAgentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.SessionAgentCall(nil), f.enqueuedCalls...)
}

// consume fires the oldest enqueued call's OnConsumed, the way the queue
// would when it folds the message into a turn (true) or drops it (false).
func (f *fakeRunner) consume(consumed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.enqueuedCalls) == 0 {
		return
	}
	first := f.enqueuedCalls[0]
	f.enqueuedCalls = f.enqueuedCalls[1:]
	if first.OnConsumed != nil {
		first.OnConsumed(consumed)
	}
}

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

// newBoundExecutor builds an Executor the way production does (#350): its
// host registry binds ctx-1 — the context newExecCtx names — to sess-1
// with runner, and the executor resolves its session from the task's A2A
// context instead of holding one.
func newBoundExecutor(runner Runner, opts ...Option) *Executor {
	reg := NewContextRegistry()
	reg.Bind("ctx-1", ContextBinding{Runner: runner, SessionID: "sess-1", Started: time.Now()})
	return NewExecutor(reg, "ctx-1", opts...)
}

func collect(t *testing.T, seq iter.Seq2[a2aspec.Event, error]) []a2aspec.Event {
	t.Helper()
	var evs []a2aspec.Event
	for ev, err := range seq {
		// The executor reports failures as events, never as the second
		// value of the sequence.
		require.NoError(t, err, "unexpected error from executor sequence")
		evs = append(evs, ev)
	}
	return evs
}

// artifactState is the sentinel states uses for artifact events so ordering
// can be asserted in one shot alongside status updates.
const artifactState a2aspec.TaskState = "<artifact>"

// states summarizes an event stream as a slice of task states.
func states(t *testing.T, evs []a2aspec.Event) []a2aspec.TaskState {
	t.Helper()
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

func statusUpdate(t *testing.T, ev a2aspec.Event) *a2aspec.TaskStatusUpdateEvent {
	t.Helper()
	sue, ok := ev.(*a2aspec.TaskStatusUpdateEvent)
	require.True(t, ok, "event is %T, want *TaskStatusUpdateEvent", ev)
	return sue
}

// Status timestamps are UTC on the wire (TCK DM-SERIAL-003): the SDK
// constructor stamps a local-zone time.Now, and the executor's
// statusEvent normalizes every status it yields so the serialized
// timestamp carries a Z suffix wherever the host runs.
func TestStatusEventTimestampIsUTC(t *testing.T) {
	ev := statusEvent(newExecCtx(a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("hi"))), a2aspec.TaskStateWorking, nil)
	require.NotNil(t, ev.Status.Timestamp)
	require.Equal(t, time.UTC, ev.Status.Timestamp.Location())
	data, err := json.Marshal(ev)
	require.NoError(t, err)
	var wire struct {
		Status struct {
			Timestamp string `json:"timestamp"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	require.True(t, strings.HasSuffix(wire.Status.Timestamp, "Z"),
		"timestamp %q must end with the Z suffix", wire.Status.Timestamp)
}

func statusMessageText(t *testing.T, ev a2aspec.Event) string {
	t.Helper()
	sue := statusUpdate(t, ev)
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
	t.Parallel()

	runner := &fakeRunner{result: textResult("all done")}
	exec := newBoundExecutor(runner, WithDiff(func(context.Context) (string, error) {
		return "the diff", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		artifactState,
		artifactState,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs))

	require.True(t, runner.ran, "runner.Run was not called")
	require.Equal(t, "sess-1", runner.gotCall.SessionID)
	require.Equal(t, "do the thing", runner.gotCall.Prompt)
	// The task is the run (#350): the RunID echoes the A2A task ID, so
	// the run's terminal RunComplete event names the task that started
	// it.
	require.Equal(t, "task-1", runner.gotCall.RunID)

	// The diff artifact carries the chunk (one, for a short diff), the
	// task's identifiers, and the diff identity.
	art, ok := evs[2].(*a2aspec.TaskArtifactUpdateEvent)
	require.True(t, ok, "event is %T, want *TaskArtifactUpdateEvent", evs[2])
	require.Equal(t, DiffArtifactID, art.Artifact.ID)
	require.Equal(t, DiffArtifactName, art.Artifact.Name)
	require.False(t, art.Append)
	require.True(t, art.LastChunk, "a single-chunk diff is also the last chunk")
	require.Equal(t, DiffMediaType, art.Artifact.Parts[0].MediaType)
	require.Equal(t, DiffFilename, art.Artifact.Parts[0].Filename)
	require.Equal(t, "the diff", partsText(art.Artifact.Parts))
	require.Equal(t, a2aspec.TaskID("task-1"), art.TaskID)
	require.Equal(t, "ctx-1", art.ContextID)

	// The dispatch-result artifact carries the typed outcome.
	res, ok := evs[3].(*a2aspec.TaskArtifactUpdateEvent)
	require.True(t, ok, "event is %T, want *TaskArtifactUpdateEvent", evs[3])
	require.Equal(t, ResultArtifactID, res.Artifact.ID)
	require.Equal(t, ResultArtifactName, res.Artifact.Name)
	require.False(t, res.Append)
	require.True(t, res.LastChunk)
	decoded, ok := decodeDispatchOutcome(res.Artifact.Parts[0])
	require.True(t, ok)
	require.Equal(t, DispatchOutcome{DiffBytes: len("the diff")}, decoded)

	// The terminal status carries the agent's text output, stamped with
	// the task's identifiers.
	terminal := statusUpdate(t, evs[4])
	require.Equal(t, a2aspec.TaskID("task-1"), terminal.TaskID)
	require.Equal(t, "ctx-1", terminal.ContextID)
	require.Equal(t, "all done", statusMessageText(t, evs[4]))
	require.NotNil(t, terminal.Status.Message)
	require.Equal(t, a2aspec.TaskID("task-1"), terminal.Status.Message.TaskID)
}

func TestExecuteNoDiffFunc(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs), "no artifact expected")
}

func TestExecuteEmptyDiffEmitsNoArtifact(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := newBoundExecutor(runner, WithDiff(func(context.Context) (string, error) {
		return "", nil // clean worktree
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	// No diff artifact for an empty diff; the dispatch-result artifact
	// still reports the zero work (#361).
	for _, ev := range evs {
		if art, isArtifact := ev.(*a2aspec.TaskArtifactUpdateEvent); isArtifact {
			require.Equal(t, ResultArtifactID, art.Artifact.ID, "an empty diff emits only the dispatch-result artifact")
			decoded, ok := decodeDispatchOutcome(art.Artifact.Parts[0])
			require.True(t, ok)
			require.Zero(t, decoded.DiffBytes)
			require.Zero(t, decoded.FilesChanged)
			require.Empty(t, decoded.DiffError)
		}
	}
}

func TestExecuteDiffErrorStillCompletes(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := newBoundExecutor(runner, WithDiff(func(context.Context) (string, error) {
		return "", errors.New("not a git repo")
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	// The run still completes: no diff artifact, but the typed outcome
	// carries the capture error on the wire (#361).
	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		artifactState,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs), "diff error must not fail the run")

	art, ok := evs[2].(*a2aspec.TaskArtifactUpdateEvent)
	require.True(t, ok)
	require.Equal(t, ResultArtifactID, art.Artifact.ID)
	decoded, ok := decodeDispatchOutcome(art.Artifact.Parts[0])
	require.True(t, ok)
	require.Equal(t, "not a git repo", decoded.DiffError)
}

func TestExecuteRunFailure(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{err: errors.New("model exploded")}
	exec := newBoundExecutor(runner, WithDiff(func(context.Context) (string, error) {
		return "should not be called", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}
	require.Equal(t, want, states(t, evs))

	// The error is surfaced in the failed status message.
	require.Equal(t, "model exploded", statusMessageText(t, evs[2]))
}

// A panic inside the runner (a tool or provider adapter bug) must fail
// the task and keep the process alive (#345): Execute yields Submitted,
// Working, then exactly one Failed with "panicked" in the message.
func TestExecuteRunnerPanicFails(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{panicValue: errors.New("tool blew up")}
	exec := newBoundExecutor(runner, WithDiff(func(context.Context) (string, error) {
		return "should not be called", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}
	require.Equal(t, want, states(t, evs))
	require.Contains(t, statusMessageText(t, evs[2]), "panicked")

	// The recovered error must not wrap context.Canceled, so the
	// canceled branch of Execute cannot swallow it (#342).
	binding := ContextBinding{Runner: runner, SessionID: "sess-1", Started: time.Now()}
	_, err := exec.runWithTodos(context.Background(), newExecCtx(msg), binding, "go",
		func(a2aspec.Event, error) bool { return true })
	require.Error(t, err)
	require.Contains(t, err.Error(), "panicked")
	require.False(t, errors.Is(err, context.Canceled))
}

func TestExecuteNilResultFails(t *testing.T) {
	t.Parallel()

	// SessionAgent.Run returns (nil, nil) without running anything when
	// the session is busy (prompt silently queued) or a cancel landed
	// during dispatch. The task must not report Completed.
	runner := &fakeRunner{result: nil, err: nil}
	exec := newBoundExecutor(runner, WithDiff(func(context.Context) (string, error) {
		return "should not become an artifact", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}
	require.Equal(t, want, states(t, evs), "a turn that never ran must not complete")
	require.Contains(t, statusMessageText(t, evs[2]), "did not start a turn")
}

// steerOnContext returns a fresh steer message for the bound test context,
// optionally naming the running task in ReferenceTasks the way the
// coordinator's front door does (#351).
func steerOnContext(text string, reference *a2aspec.TaskID) *a2aspec.Message {
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart(text))
	if reference != nil {
		msg.ReferenceTasks = []a2aspec.TaskID{*reference}
	}
	return msg
}

// TestExecuteSecondMessageSteers is the #351 contract: the first message
// on the context is the dispatch's own turn; every later message is a
// steer — enqueued on the running session, never run as a turn — and the
// steer's own task completes once the queue consumed the message. The
// steer call inherits the binding's shaping with RunID empty and Steer
// set, and ReferenceTasks are advisory: naming the running task neither
// helps nor blocks delivery.
func TestExecuteSecondMessageSteers(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("work done"), enqueueAccepted: true}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}, states(t, evs), "the first message starts the dispatch's own turn")
	require.Equal(t, "do the thing", runner.gotCall.Prompt)

	// The steer: a new task on the same context.
	running := a2aspec.TaskID("task-1")
	var evs2 []a2aspec.Event
	for ev, err := range exec.Execute(context.Background(), newExecCtx(steerOnContext("stop writing Rust", &running))) {
		require.NoError(t, err)
		evs2 = append(evs2, ev)
		if len(evs2) != 2 {
			continue
		}
		// Submitted and Working are out; the call is queued. Assert its
		// shaping and hand the queue its verdict.
		enqueued := runner.enqueued()
		require.Len(t, enqueued, 1, "the steer must be enqueued exactly once")
		call := enqueued[0]
		require.Equal(t, "sess-1", call.SessionID)
		require.Equal(t, "stop writing Rust", call.Prompt)
		require.Empty(t, call.RunID, "a steer folds like an injection")
		require.True(t, call.Steer, "the persisted message is marked as a steer (#410)")
		require.Nil(t, call.Accepted)
		require.Nil(t, call.OnComplete)
		require.NotNil(t, call.OnConsumed)
		require.Equal(t, "do the thing", runner.gotCall.Prompt, "the steer must not reach Run")
		runner.consume(true)
	}

	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}, states(t, evs2), "the steer completes once its message was consumed")
	require.Equal(t, "delivered", statusMessageText(t, evs2[2]))
}

// A steer whose message the queue dropped without running — the run was
// canceled while the steer sat queued — fails the steer's own task: the
// caller learns the message never reached the agent (#351).
func TestExecuteSteerDroppedByQueueFails(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("work done"), enqueueAccepted: true}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	var evs []a2aspec.Event
	for ev, err := range exec.Execute(context.Background(), newExecCtx(steerOnContext("too late", nil))) {
		require.NoError(t, err)
		evs = append(evs, ev)
		if len(evs) < 2 {
			continue
		}
		runner.consume(false)
	}

	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateFailed,
	}, states(t, evs))
	require.Contains(t, statusMessageText(t, evs[2]), "agent finished before the message was consumed")
}

// A steer the runner refuses — the run ended between the registry
// lookup and the enqueue — rejects the steer's own task without ever
// waiting on the queue (#351).
func TestExecuteSteerRefusedWhenRunEnded(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("work done")}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	steer := steerOnContext("anyone there?", nil)
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(steer)))
	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateRejected,
	}, states(t, evs), "a refused steer must not reach Working")
	require.Contains(t, statusMessageText(t, evs[1]), "no longer running")
	require.Empty(t, runner.enqueued())
}

// A steer's attachments ride the message's non-text parts and come back
// on the steer call, the same pipeline a typed prompt's attachments take
// (#351).
func TestExecuteSteerCarriesAttachments(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("work done"), enqueueAccepted: true}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	steer := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("see this file"))
	steer.Parts = append(steer.Parts, &a2aspec.Part{
		Content:   a2aspec.Raw([]byte("package main")),
		Filename:  "main.go",
		MediaType: "text/x-go",
	})
	var evs []a2aspec.Event
	for ev, err := range exec.Execute(context.Background(), newExecCtx(steer)) {
		require.NoError(t, err)
		evs = append(evs, ev)
		if len(evs) < 2 {
			continue
		}
		enqueued := runner.enqueued()
		require.Len(t, enqueued, 1)
		require.Len(t, enqueued[0].Attachments, 1, "the file part must come back as an attachment")
		require.Equal(t, "main.go", enqueued[0].Attachments[0].FileName)
		require.Equal(t, "text/x-go", enqueued[0].Attachments[0].MimeType)
		require.Equal(t, []byte("package main"), enqueued[0].Attachments[0].Content)
		enqueued[0].OnConsumed(true)
	}

	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}, states(t, evs))
}

// blockingCancelRunner blocks in Run until its kill channel fires, then
// returns context.Canceled — a kill delivered behind the executor's back
// unless the test calls Cancel itself.
type blockingCancelRunner struct {
	started chan struct{}
	kill    chan struct{}
}

func (f *blockingCancelRunner) Run(_ context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	if f.started != nil {
		close(f.started)
	}
	<-f.kill
	return nil, context.Canceled
}

func (f *blockingCancelRunner) Cancel(sessionID string) {
	close(f.kill)
}

func (f *blockingCancelRunner) EnqueueWhenBusy(agent.SessionAgentCall) bool { return false }

func TestExecuteOutOfBandCancelEmitsCanceled(t *testing.T) {
	t.Parallel()

	// A runner killed behind the SDK's back — the wander ladder or the
	// watchdog canceling the agent directly — returns context.Canceled
	// on a live context with no executor Cancel in play. The executor
	// must surface exactly one terminal Canceled carrying the kill
	// reason, or the consumer waits forever (#342).
	tests := []struct {
		name   string
		opts   []Option
		reason string
	}{
		{
			name:   "default reason",
			reason: "canceled",
		},
		{
			name:   "kill reason",
			opts:   []Option{WithCancelReason(func() string { return "wander kill: no todo progress" })},
			reason: "wander kill: no todo progress",
		},
		{
			name:   "empty reason falls back",
			opts:   []Option{WithCancelReason(func() string { return "" })},
			reason: "canceled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{err: context.Canceled}
			exec := newBoundExecutor(runner, tt.opts...)

			msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
			evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

			want := []a2aspec.TaskState{
				a2aspec.TaskStateSubmitted,
				a2aspec.TaskStateWorking,
				a2aspec.TaskStateCanceled,
			}
			require.Equal(t, want, states(t, evs), "an out-of-band cancel must end the task canceled")
			require.Equal(t, tt.reason, statusMessageText(t, evs[2]))
		})
	}
}

func TestExecuteOwnCancelEmitsOneTerminal(t *testing.T) {
	t.Parallel()

	// The executor's own Cancel emits the terminal Canceled status; the
	// run ending on that same cancel must not emit a second one.
	runner := &blockingCancelRunner{started: make(chan struct{}), kill: make(chan struct{})}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evsCh := make(chan []a2aspec.Event, 1)
	go func() {
		evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
	}()

	<-runner.started
	cancelEvs := collect(t, exec.Cancel(context.Background(), newExecCtx(nil)))

	var executeEvs []a2aspec.Event
	select {
	case executeEvs = <-evsCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the canceled run to end")
	}

	canceled := 0
	for _, st := range append(states(t, cancelEvs), states(t, executeEvs)...) {
		if st == a2aspec.TaskStateCanceled {
			canceled++
		}
	}
	require.Equal(t, 1, canceled, "exactly one Canceled across the Execute and Cancel sequences")
}

func TestExecuteEmptyPromptRejects(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("never")}
	exec := newBoundExecutor(runner)

	// A message with no text parts carries nothing to run.
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewDataPart(map[string]any{"k": "v"}))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateRejected,
	}
	require.Equal(t, want, states(t, evs))
	require.False(t, runner.ran, "runner.Run must not be called for an empty prompt")
}

// TestExecuteRoutesByContext pins the #350 mapping at the executor level:
// two dispatches share one host registry, each bound to its own context
// and session, and each task runs against the binding its context names
// — its own session, with the task ID stamped as the call's RunID.
func TestExecuteRoutesByContext(t *testing.T) {
	t.Parallel()

	one := &fakeRunner{result: textResult("one done")}
	two := &fakeRunner{result: textResult("two done")}
	reg := NewContextRegistry()
	reg.Bind("ctx-1", ContextBinding{Runner: one, SessionID: "sess-1", Started: time.Now()})
	reg.Bind("ctx-2", ContextBinding{Runner: two, SessionID: "sess-2", Started: time.Now()})
	execOne := NewExecutor(reg, "ctx-1")
	execTwo := NewExecutor(reg, "ctx-2")

	ctxFor := func(contextID, taskID string) *a2asrv.ExecutorContext {
		execCtx := newExecCtx(a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing")))
		execCtx.ContextID = contextID
		execCtx.TaskID = a2aspec.TaskID(taskID)
		return execCtx
	}

	evsOne := collect(t, execOne.Execute(context.Background(), ctxFor("ctx-1", "task-1")))
	evsTwo := collect(t, execTwo.Execute(context.Background(), ctxFor("ctx-2", "task-2")))

	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}, states(t, evsOne))
	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}, states(t, evsTwo))

	// Each task ran on its own session, and each RunComplete carries its
	// task ID as RunID.
	require.Equal(t, "sess-1", one.gotCall.SessionID)
	require.Equal(t, "task-1", one.gotCall.RunID)
	require.Equal(t, "sess-2", two.gotCall.SessionID)
	require.Equal(t, "task-2", two.gotCall.RunID)
}

// TestExecuteRejectsUnknownContext pins the rejection half of #350: a
// task naming a context the registry does not know is rejected without
// the runner ever being called.
func TestExecuteRejectsUnknownContext(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("never")}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	execCtx := newExecCtx(msg)
	execCtx.ContextID = "ctx-unknown"
	evs := collect(t, exec.Execute(context.Background(), execCtx))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateRejected,
	}
	require.Equal(t, want, states(t, evs))
	require.Contains(t, statusMessageText(t, evs[1]),
		"no running agent for context ctx-unknown; task sessions are not continuable")
	require.False(t, runner.ran, "runner.Run must not be called for an unknown context")
}

// TestExecuteRejectsForeignContext pins the route isolation (#350): a
// message that arrives on one dispatch's route naming another dispatch's
// context is rejected on that route, and the foreign dispatch's runner
// is never called.
func TestExecuteRejectsForeignContext(t *testing.T) {
	t.Parallel()

	one := &fakeRunner{result: textResult("never")}
	two := &fakeRunner{result: textResult("never")}
	reg := NewContextRegistry()
	reg.Bind("ctx-1", ContextBinding{Runner: one, SessionID: "sess-1", Started: time.Now()})
	reg.Bind("ctx-2", ContextBinding{Runner: two, SessionID: "sess-2", Started: time.Now()})
	execOne := NewExecutor(reg, "ctx-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	execCtx := newExecCtx(msg)
	execCtx.ContextID = "ctx-2"
	evs := collect(t, execOne.Execute(context.Background(), execCtx))

	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateRejected,
	}, states(t, evs))
	require.False(t, one.ran, "the route's runner must not be called for a foreign context")
	require.False(t, two.ran, "the foreign dispatch's runner must not be called")
}

// TestExecuteRejectsEndedRun pins the binding lifetime (#350): once the
// run ends its binding is removed, and the next message on the context
// is rejected — sub-agent sessions are not continuable.
func TestExecuteRejectsEndedRun(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("never")}
	reg := NewContextRegistry()
	reg.Bind("ctx-1", ContextBinding{Runner: runner, SessionID: "sess-1", Started: time.Now()})
	exec := NewExecutor(reg, "ctx-1")
	reg.Unbind("ctx-1")

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateRejected,
	}, states(t, evs))
	require.Contains(t, statusMessageText(t, evs[1]),
		"no running agent for context ctx-1; task sessions are not continuable")
	require.False(t, runner.ran, "runner.Run must not be called after the run ended")
}

// TestCancelRejectsUnboundContext: a cancel whose context resolves to no
// running agent reports the run already ended and touches no runner.
func TestCancelRejectsUnboundContext(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	reg := NewContextRegistry()
	reg.Bind("ctx-1", ContextBinding{Runner: runner, SessionID: "sess-1", Started: time.Now()})
	exec := NewExecutor(reg, "ctx-1")
	reg.Unbind("ctx-1")

	evs := collect(t, exec.Cancel(context.Background(), newExecCtx(nil)))

	require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateCanceled}, states(t, evs))
	require.Contains(t, statusMessageText(t, evs[0]),
		"no running agent for context ctx-1; task sessions are not continuable")
	require.Empty(t, runner.canceledFor, "an unbound cancel must not cancel a runner")
}

func TestExecuteConsumerStopsBeforeRun(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("never")}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	// Stop consuming after the first (Submitted) event, as a disconnecting
	// consumer would.
	for range exec.Execute(context.Background(), newExecCtx(msg)) {
		break
	}

	require.False(t, runner.ran, "runner.Run must not be called after the consumer stops")
}

func TestExecuteExistingTaskSkipsSubmitted(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	exec := newBoundExecutor(runner)

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	execCtx := newExecCtx(msg)
	execCtx.StoredTask = &a2aspec.Task{ID: "task-1", ContextID: "ctx-1"}

	evs := collect(t, exec.Execute(context.Background(), execCtx))

	want := []a2aspec.TaskState{
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}
	require.Equal(t, want, states(t, evs), "no submitted for an existing task")
}

func TestCancel(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	exec := newBoundExecutor(runner)

	evs := collect(t, exec.Cancel(context.Background(), newExecCtx(nil)))

	require.Equal(t, "sess-1", runner.canceledFor)
	require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateCanceled}, states(t, evs))
}

// TestCancelCarriesReason pins #348: the reason a tasks/cancel request
// carried in its declared metadata lands on the terminal Canceled
// status message — the wire's JSON round-trip of the typed payload, a
// direct typed payload, and a bare string all decode. Without metadata
// the in-process kill reason (#342) is used, falling back to the
// generic "canceled".
func TestCancelCarriesReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]any
		opts     []Option
		want     string
	}{
		{
			name:     "typed reason from the wire round-trip",
			metadata: map[string]any{CancelReasonMetadataKey: map[string]any{"reason": "hard timeout"}},
			want:     "hard timeout",
		},
		{
			name:     "direct typed payload",
			metadata: map[string]any{CancelReasonMetadataKey: CancelReason{Reason: "ignored nudges"}},
			want:     "ignored nudges",
		},
		{
			name:     "bare string",
			metadata: map[string]any{CancelReasonMetadataKey: "stalled todos"},
			want:     "stalled todos",
		},
		{
			name: "no metadata falls back to the in-process reason",
			opts: []Option{WithCancelReason(func() string { return "crush exited" })},
			want: "crush exited",
		},
		{
			name: "no metadata and no in-process reason",
			want: "canceled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &fakeRunner{}
			exec := newBoundExecutor(runner, tt.opts...)
			execCtx := newExecCtx(nil)
			execCtx.Metadata = tt.metadata

			evs := collect(t, exec.Cancel(context.Background(), execCtx))

			require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateCanceled}, states(t, evs))
			require.Equal(t, tt.want, statusMessageText(t, evs[0]))
		})
	}
}

func TestMessageText(t *testing.T) {
	t.Parallel()

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
		{
			// A nil parts entry is constructible from a remote peer's
			// JSON ([null, ...]) and must be skipped, not dereferenced.
			"nil part entry skipped",
			&a2aspec.Message{Parts: a2aspec.ContentParts{nil, a2aspec.NewTextPart("x")}},
			"x",
		},
		{
			// Non-text parts are ignored without leaving stray
			// separators between the surviving text parts.
			"non-text parts ignored",
			a2aspec.NewMessage(a2aspec.MessageRoleUser,
				a2aspec.NewTextPart("a"),
				a2aspec.NewDataPart(map[string]any{"k": "v"}),
				a2aspec.NewRawPart([]byte("zz")),
				a2aspec.NewTextPart("b"),
			),
			"a\nb",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, messageText(tc.msg))
		})
	}
}

// initTestRepo creates a git repo in a temp dir with one committed file and
// returns the dir. Skips the test when git is unavailable.
func initTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{
			"-C", dir,
			"-c", "user.name=test",
			"-c", "user.email=test@example.com",
			"-c", "commit.gpgsign=false",
		}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("original\n"), 0o644))
	run("init", "-q")
	run("add", "tracked.txt")
	run("commit", "-q", "-m", "initial")
	return dir
}

func TestGitDiff(t *testing.T) {
	t.Parallel()

	dir := initTestRepo(t)

	// Unstaged change to a tracked file, plus a brand-new untracked file:
	// both are the dispatched agent's work product and must appear.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("modified\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "created.txt"), []byte("new file\n"), 0o644))

	diff, err := GitDiff(dir)(t.Context())
	require.NoError(t, err)
	require.Contains(t, diff, "tracked.txt")
	require.Contains(t, diff, "modified")
	require.Contains(t, diff, "created.txt")
	require.Contains(t, diff, "new file")

	// The real index must be untouched: the new file stays untracked and
	// nothing is staged.
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "status", "--porcelain").Output()
	require.NoError(t, err)
	require.Contains(t, string(out), "?? created.txt")
	staged, err := exec.CommandContext(t.Context(), "git", "-C", dir, "diff", "--cached", "--name-only").Output()
	require.NoError(t, err)
	require.Empty(t, string(staged), "GitDiff must not stage anything in the real index")
}

func TestGitDiffCleanWorktree(t *testing.T) {
	t.Parallel()

	dir := initTestRepo(t)
	diff, err := GitDiff(dir)(t.Context())
	require.NoError(t, err)
	require.Empty(t, diff)
}

func TestGitDiffNotARepo(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	diff, err := GitDiff(t.TempDir())(t.Context())
	require.Error(t, err)
	require.Empty(t, diff)
}

// TestGitDiffIgnoresUserDiffConfig verifies GitDiff returns a plain
// unified diff even when the user's global git config forces an external
// diff driver and always-on color: the diff keeps its a/ b/ headers and
// carries no external-tool output or escape sequences.
func TestGitDiffIgnoresUserDiffConfig(t *testing.T) {
	// A shell script only runs on Unix; on Windows git would fail to
	// invoke it, so the external-diff half of the test cannot apply.
	if runtime.GOOS == "windows" {
		t.Skip("diff.external script requires a Unix shell")
	}

	dir := initTestRepo(t)

	// A global config that forces an external diff driver (a script
	// that prints EXTERNAL) and always-on color.
	cfg := t.TempDir()
	script := filepath.Join(cfg, "ext-diff.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\necho EXTERNAL\n"), 0o755))
	global := filepath.Join(cfg, "gitconfig")
	require.NoError(t, os.WriteFile(global,
		[]byte("[diff]\n\texternal = "+script+"\n[color]\n\tui = always\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	// An unstaged change and a new file: both are the agent's work
	// product and must appear as a unified diff.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("modified\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "created.txt"), []byte("new file\n"), 0o644))

	diff, err := GitDiff(dir)(t.Context())
	require.NoError(t, err)
	require.Contains(t, diff, "+++ b/")
	require.Contains(t, diff, "tracked.txt")
	require.Contains(t, diff, "created.txt")
	require.NotContains(t, diff, "EXTERNAL")
	require.NotContains(t, diff, "\x1b")
}

// inactivityRunner is a runner whose Run blocks until released, so a
// test can hold a run silent (or feed it progress) and count how many
// times the executor canceled it.
type inactivityRunner struct {
	result  *fantasy.AgentResult
	release chan struct{}

	mu          sync.Mutex
	cancelCount int
}

func newInactivityRunner(result string) *inactivityRunner {
	return &inactivityRunner{
		result:  textResult(result),
		release: make(chan struct{}),
	}
}

func (r *inactivityRunner) Run(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	<-r.release
	return r.result, nil
}

func (r *inactivityRunner) Cancel(string) {
	r.mu.Lock()
	r.cancelCount++
	r.mu.Unlock()
}

func (r *inactivityRunner) EnqueueWhenBusy(agent.SessionAgentCall) bool { return false }

func (r *inactivityRunner) cancels() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelCount
}

// The backstop's core case (#360): a silent runner that outlives the
// timeout gets runner.Cancel called exactly once and the run ends with
// exactly one Failed carrying the reason — no Canceled, which is
// reserved for the executor's Cancel.
func TestExecuteInactivitySilentRunFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newInactivityRunner("never")
		exec := newBoundExecutor(runner, WithInactivityTimeout(5*time.Minute))

		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
		evsCh := make(chan []a2aspec.Event, 1)
		go func() {
			evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
		}()

		// The run never yields an event: with every goroutine durably
		// blocked, virtual time runs the backstop (5m) and the settle
		// window (2s) past, well inside this sleep.
		time.Sleep(6 * time.Minute)

		var evs []a2aspec.Event
		select {
		case evs = <-evsCh:
		default:
			t.Fatal("the backstop did not end the run")
		}

		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			a2aspec.TaskStateFailed,
		}
		require.Equal(t, want, states(t, evs))
		require.Equal(t, "inactivity timeout: no progress for 5m0s", statusMessageText(t, evs[2]))
		require.Equal(t, 1, runner.cancels(), "runner.Cancel must be called exactly once")

		close(runner.release)
	})
}

// A runner that emits a changed todo snapshot every half the timeout
// keeps resetting the backstop and runs to completion: a run that is
// visibly making progress must not trip it.
func TestExecuteInactivityProgressResetsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newInactivityRunner("done")
		source := newPipeTodoSource()
		exec := newBoundExecutor(runner, WithTodos(source), WithInactivityTimeout(10*time.Second))

		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
		evsCh := make(chan []a2aspec.Event, 1)
		go func() {
			evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
		}()

		// A changed snapshot every 4 virtual seconds — half the
		// timeout — resets the backstop each time, so it never fires
		// while the run makes progress.
		for i := 0; i < 3; i++ {
			source.ch <- todoSnapshot(runTodos(
				session.Todo{Content: fmt.Sprintf("step %d", i), Status: session.TodoStatusInProgress, ActiveForm: fmt.Sprintf("stepping %d", i)},
			))
			<-time.After(4 * time.Second)
		}
		close(runner.release)
		synctest.Wait()

		var evs []a2aspec.Event
		select {
		case evs = <-evsCh:
		default:
			t.Fatal("the run did not end")
		}
		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			todoState,
			todoState,
			todoState,
			a2aspec.TaskStateCompleted,
		}
		require.Equal(t, want, statesWithTodos(t, evs))
		require.Equal(t, 0, runner.cancels(), "a run that keeps making progress must not be canceled")
	})
}

// With inactivity_timeout=0 no timer runs and behavior is unchanged: a
// silent run is left alone no matter how long it stays quiet, and
// completes normally when the runner finishes.
func TestExecuteInactivityDisabledNoTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newInactivityRunner("done")
		exec := newBoundExecutor(runner, WithInactivityTimeout(0))

		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
		evsCh := make(chan []a2aspec.Event, 1)
		go func() {
			evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
		}()

		// An hour of virtual silence: with the backstop disabled nothing
		// may fire, so the run must still be in flight.
		time.Sleep(time.Hour)
		select {
		case <-evsCh:
			t.Fatal("the run must not end while the runner is silent and the backstop is disabled")
		default:
		}

		close(runner.release)
		synctest.Wait()
		var evs []a2aspec.Event
		select {
		case evs = <-evsCh:
		default:
			t.Fatal("the run did not end")
		}

		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			a2aspec.TaskStateCompleted,
		}
		require.Equal(t, want, states(t, evs))
		require.Equal(t, 0, runner.cancels())
	})
}

// The diff is chunked, not shipped as one SSE line (#361): every chunk
// stays within diffChunkSize, chunks break at line boundaries where the
// lines allow, and concatenating them reproduces the diff byte for byte.
func TestChunkDiff(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, chunkDiff(""))
	})

	t.Run("small diff is one chunk", func(t *testing.T) {
		t.Parallel()
		diff := "diff --git a/x b/x\n@@\n+y\n"
		require.Equal(t, []string{diff}, chunkDiff(diff))
	})

	t.Run("multi-line diff breaks at line boundaries", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("x", 1024) + "\n"
		var b strings.Builder
		for range 300 {
			b.WriteString(line)
		}
		diff := b.String()
		require.Greater(t, len(diff), diffChunkSize, "the fixture must exceed one chunk")

		chunks := chunkDiff(diff)
		require.Greater(t, len(chunks), 1)
		var reassembled strings.Builder
		for _, chunk := range chunks {
			require.LessOrEqual(t, len(chunk), diffChunkSize)
			require.True(t, strings.HasSuffix(chunk, "\n"), "chunks break at line boundaries")
			reassembled.WriteString(chunk)
		}
		require.Equal(t, diff, reassembled.String())
	})

	t.Run("a line longer than the cap is hard-split", func(t *testing.T) {
		t.Parallel()
		diff := strings.Repeat("x", 1024*1024) // 1 MiB, no newline
		chunks := chunkDiff(diff)
		require.Len(t, chunks, 4)
		require.Equal(t, diff, strings.Join(chunks, ""))
	})

	t.Run("an 11 MiB diff streams in chunks within the cap", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("+", 4096) + "\n"
		var b strings.Builder
		for len(b.String()) < 11*1024*1024 {
			b.WriteString(line)
		}
		diff := b.String()

		chunks := chunkDiff(diff)
		require.Greater(t, len(chunks), 40, "an 11 MiB diff needs many chunks")
		var reassembled strings.Builder
		for _, chunk := range chunks {
			require.LessOrEqual(t, len(chunk), diffChunkSize)
			reassembled.WriteString(chunk)
		}
		require.Equal(t, diff, reassembled.String())
	})
}

// The executor emits the diff as a sequence of chunk updates on one named
// artifact: the first replaces, the rest append, the last closes, and the
// typed dispatch-result artifact follows (#361).
func TestExecuteDiffChunkedArtifact(t *testing.T) {
	t.Parallel()

	line := strings.Repeat("+", 4096) + "\n"
	var b strings.Builder
	for len(b.String()) < 11*1024*1024 {
		b.WriteString(line)
	}
	diff := "diff --git a/big.txt b/big.txt\n" + b.String()

	runner := &fakeRunner{result: textResult("done")}
	exec := newBoundExecutor(runner, WithDiff(func(context.Context) (string, error) {
		return diff, nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evs := collect(t, exec.Execute(context.Background(), newExecCtx(msg)))

	var chunks []*a2aspec.TaskArtifactUpdateEvent
	var res *a2aspec.TaskArtifactUpdateEvent
	for _, ev := range evs {
		art, ok := ev.(*a2aspec.TaskArtifactUpdateEvent)
		if !ok {
			continue
		}
		switch art.Artifact.ID {
		case DiffArtifactID:
			chunks = append(chunks, art)
		case ResultArtifactID:
			res = art
		}
	}
	require.NotEmpty(t, chunks)
	require.NotNil(t, res)

	var reassembled strings.Builder
	for i, chunk := range chunks {
		require.LessOrEqual(t, len(chunk.Artifact.Parts[0].Text()), diffChunkSize, "no chunk exceeds the SSE-safe cap")
		require.Equal(t, i > 0, chunk.Append, "only chunks after the first append")
		require.Equal(t, i == len(chunks)-1, chunk.LastChunk, "exactly the last chunk closes the artifact")
		reassembled.WriteString(chunk.Artifact.Parts[0].Text())
	}
	require.Equal(t, diff, reassembled.String(), "the chunks reassemble byte for byte")

	decoded, ok := decodeDispatchOutcome(res.Artifact.Parts[0])
	require.True(t, ok)
	require.Equal(t, len(diff), decoded.DiffBytes)
	require.Empty(t, decoded.DiffError)
	require.Positive(t, decoded.FilesChanged)
}

// askingRunner is a dispatched agent whose turn asks a question through
// its scoped question service with the real question tool (#352), then
// ends the turn with the tool's response as its text — or with the run's
// context error once the ask was canceled, the way the agent's turn
// ends when its context dies.
type askingRunner struct {
	fakeRunner

	svc question.Service
	// toolResp receives the question tool's response once the ask ends.
	toolResp chan fantasy.ToolResponse
	// runs counts Run calls: a resumed run is the same call, never a
	// second one.
	runs atomic.Int32
}

func newAskingRunner(svc question.Service) *askingRunner {
	return &askingRunner{
		fakeRunner: fakeRunner{enqueueAccepted: true},
		svc:        svc,
		toolResp:   make(chan fantasy.ToolResponse, 1),
	}
}

// askingRunnerInput is the question tool call the asking runner makes.
const askingRunnerInput = `{"questions":[{"type":"free_text","question":"Which database?","description":"The schema differs per engine."}]}`

func (r *askingRunner) Run(ctx context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	r.runs.Add(1)
	resp, err := tools.NewQuestionTool(r.svc).Run(ctx, fantasy.ToolCall{
		ID:    "call-1",
		Name:  tools.QuestionToolName,
		Input: askingRunnerInput,
	})
	r.toolResp <- resp
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return textResult(resp.Content), nil
}

// newAskingExecutor binds an asking runner and its scoped question
// service to the test context, the way the dispatch host wires them.
func newAskingExecutor() (*askingRunner, *Executor) {
	svc := question.NewService()
	runner := newAskingRunner(svc)
	return runner, newBoundExecutor(runner, WithQuestions(svc))
}

// parkedTaskCtx is the executor context of a message naming the test
// task while it is parked in input-required (#352).
func parkedTaskCtx(msg *a2aspec.Message) *a2asrv.ExecutorContext {
	execCtx := newExecCtx(msg)
	execCtx.StoredTask = &a2aspec.Task{
		ID:        "task-1",
		ContextID: "ctx-1",
		Status:    a2aspec.TaskStatus{State: a2aspec.TaskStateInputRequired},
	}
	return execCtx
}

// parkOnQuestion starts the dispatch's turn and returns the typed
// question it parked on: the execution ends in input-required, and the
// status message names the questions/v1 extension and carries the
// question both as text and as a typed DataPart.
func parkOnQuestion(t *testing.T, exec *Executor) agent.QuestionRequest {
	t.Helper()
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("set up the schema"))
	evs := collect(t, exec.Execute(t.Context(), newExecCtx(msg)))
	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateInputRequired,
	}, states(t, evs), "a question ends the execution in input-required")

	parked := statusUpdate(t, evs[2])
	require.NotNil(t, parked.Status.Message)
	require.Contains(t, parked.Status.Message.Extensions, QuestionExtensionURI)
	require.Equal(t, "Which database?", partsText(parked.Status.Message.Parts))
	var payload *agent.QuestionRequest
	for _, part := range parked.Status.Message.Parts {
		data, ok := part.Content.(a2aspec.Data)
		if !ok {
			continue
		}
		decoded, err := DecodeValue(QuestionExt, data.Value)
		require.NoError(t, err)
		payload = decoded.(*agent.QuestionRequest)
	}
	require.NotNil(t, payload, "the question rides a typed questions/v1 DataPart")
	require.Len(t, payload.Questions, 1)
	require.Equal(t, question.TypeFreeText, payload.Questions[0].Type)
	require.Equal(t, "Which database?", payload.Questions[0].Text)
	require.NotEmpty(t, payload.Questions[0].ID)
	return *payload
}

// typedAnswer is an answer message carrying the answers/v1 DataPart.
func typedAnswer(t *testing.T, questionID, text string) *a2aspec.Message {
	t.Helper()
	encoded, err := Encode(AnswerExt, agent.QuestionAnswer{Answers: []question.Answer{
		{QuestionID: questionID, FillInText: text},
	}})
	require.NoError(t, err)
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewDataPart(encoded))
	msg.Extensions = []string{AnswerExtensionURI}
	return msg
}

// TestExecuteQuestionParksAndResumes is the #352 contract at the
// executor: the agent's question parks the run in input-required with a
// typed payload while the tool call keeps waiting, and a message naming
// the parked task resumes the same run — no second turn — which then
// ends on the answer's execution. A typed answer and plain text (a
// free-text answer) both resume it.
func TestExecuteQuestionParksAndResumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		answer func(t *testing.T, q agent.QuestionRequest) *a2aspec.Message
	}{
		{
			name: "typed answer",
			answer: func(t *testing.T, q agent.QuestionRequest) *a2aspec.Message {
				return typedAnswer(t, q.Questions[0].ID, "postgres")
			},
		},
		{
			name: "plain text counts as free text",
			answer: func(*testing.T, agent.QuestionRequest) *a2aspec.Message {
				return a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("postgres"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner, exec := newAskingExecutor()
			q := parkOnQuestion(t, exec)
			select {
			case resp := <-runner.toolResp:
				t.Fatalf("the parked question returned before any answer: %+v", resp)
			default:
			}

			evs := collect(t, exec.Execute(t.Context(), parkedTaskCtx(tt.answer(t, q))))
			require.Equal(t, []a2aspec.TaskState{
				a2aspec.TaskStateWorking,
				a2aspec.TaskStateCompleted,
			}, states(t, evs), "the answer resumes the run to its terminal state")
			require.Contains(t, statusMessageText(t, evs[1]), "User provided: postgres")

			resp := <-runner.toolResp
			require.False(t, resp.IsError)
			require.EqualValues(t, 1, runner.runs.Load(), "the answer resumes the same run, never a new turn")
		})
	}
}

// Canceling a parked task ends it Canceled, and the run's parked question
// tool call returns an error: the ask ends with the run's context (#352).
// The canceled run's question is gone, so a late answer is Rejected.
func TestExecuteCancelWhileParked(t *testing.T) {
	t.Parallel()

	runner, exec := newAskingExecutor()
	parkOnQuestion(t, exec)

	evs := collect(t, exec.Cancel(t.Context(), parkedTaskCtx(nil)))
	require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateCanceled}, states(t, evs))
	require.Equal(t, "sess-1", runner.canceledFor)

	resp := <-runner.toolResp
	require.True(t, resp.IsError, "the parked tool call must return an error")
	require.Contains(t, resp.Content, context.Canceled.Error())

	late := collect(t, exec.Execute(t.Context(), parkedTaskCtx(
		a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("postgres")))))
	require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateRejected}, states(t, late))
}

// A parked task's cancel that lost the race to the parking execution's
// exit is re-issued (#352). The first Cancel already dropped the parked
// run, so the second finds no run record: it still ends the task
// Canceled with the request's reason, but the own-cancel mark it sets is
// one no Execute will ever forget. The SDK's Cleanup for the resolved
// cancelation drops it; an execution's Cleanup leaves marks to Execute.
func TestExecuteReissuedCancelOfParkedTask(t *testing.T) {
	t.Parallel()

	runner, exec := newAskingExecutor()
	parkOnQuestion(t, exec)
	cancelCtx := func() *a2asrv.ExecutorContext {
		execCtx := parkedTaskCtx(nil)
		execCtx.Metadata = map[string]any{CancelReasonMetadataKey: CancelReason{Reason: "hard timeout"}}
		return execCtx
	}

	first := collect(t, exec.Cancel(t.Context(), cancelCtx()))
	require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateCanceled}, states(t, first))
	require.True(t, (<-runner.toolResp).IsError, "the parked tool call must return an error")
	require.False(t, exec.ownCancel("task-1"), "the parked branch drops its own mark")

	second := collect(t, exec.Cancel(t.Context(), cancelCtx()))
	require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateCanceled}, states(t, second))
	require.Equal(t, "hard timeout", statusMessageText(t, second[0]), "the re-issued cancel carries the reason")
	require.True(t, exec.ownCancel("task-1"), "a cancel with no run record leaves its mark behind")

	exec.Cleanup(t.Context(), parkedTaskCtx(a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("postgres"))), nil, nil)
	require.True(t, exec.ownCancel("task-1"), "an execution's Cleanup leaves the mark to Execute")

	exec.Cleanup(t.Context(), cancelCtx(), nil, nil)
	require.False(t, exec.ownCancel("task-1"), "the resolved cancelation drops its mark")
	require.EqualValues(t, 1, runner.runs.Load())
}

// An answer on a task with no pending question is Rejected (#352) and
// starts nothing: neither a turn nor a steer. That holds for a task
// whose run never asked and for one whose question was already
// answered.
func TestExecuteAnswerWithoutPendingQuestionRejects(t *testing.T) {
	t.Parallel()

	answer := func() *a2aspec.Message {
		return a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("postgres"))
	}

	t.Run("never asked", func(t *testing.T) {
		t.Parallel()

		runner := &fakeRunner{result: textResult("never"), enqueueAccepted: true}
		exec := newBoundExecutor(runner, WithQuestions(question.NewService()))

		evs := collect(t, exec.Execute(t.Context(), parkedTaskCtx(answer())))
		require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateRejected}, states(t, evs))
		require.Contains(t, statusMessageText(t, evs[0]), "no question is pending")
		require.False(t, runner.ran, "a rejected answer must not start a turn")
		require.Empty(t, runner.enqueued(), "a rejected answer must not steer")
	})

	t.Run("already answered", func(t *testing.T) {
		t.Parallel()

		runner, exec := newAskingExecutor()
		parkOnQuestion(t, exec)
		collect(t, exec.Execute(t.Context(), parkedTaskCtx(answer())))
		<-runner.toolResp

		evs := collect(t, exec.Execute(t.Context(), parkedTaskCtx(answer())))
		require.Equal(t, []a2aspec.TaskState{a2aspec.TaskStateRejected}, states(t, evs))
		require.Contains(t, statusMessageText(t, evs[0]), "no question is pending")
		require.EqualValues(t, 1, runner.runs.Load())
		require.Empty(t, runner.enqueued())
	})
}

// Answers and steers stay apart while a question is parked (#351, #352):
// a new message on the context that names no parked task is a steer —
// enqueued, never an answer — and the question stays parked until a
// message naming the task answers it.
func TestExecuteSteerWhileParkedStaysASteer(t *testing.T) {
	t.Parallel()

	runner, exec := newAskingExecutor()
	q := parkOnQuestion(t, exec)

	steerCtx := newExecCtx(steerOnContext("also add an index", nil))
	steerCtx.TaskID = "task-2"
	var evs []a2aspec.Event
	for ev, err := range exec.Execute(t.Context(), steerCtx) {
		require.NoError(t, err)
		evs = append(evs, ev)
		if len(evs) != 2 {
			continue
		}
		enqueued := runner.enqueued()
		require.Len(t, enqueued, 1, "the message is enqueued as a steer")
		require.True(t, enqueued[0].Steer)
		require.Equal(t, "also add an index", enqueued[0].Prompt)
		runner.consume(true)
	}
	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateSubmitted,
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}, states(t, evs))
	select {
	case resp := <-runner.toolResp:
		t.Fatalf("a steer answered the parked question: %+v", resp)
	default:
	}

	answered := collect(t, exec.Execute(t.Context(), parkedTaskCtx(typedAnswer(t, q.Questions[0].ID, "postgres"))))
	require.Equal(t, []a2aspec.TaskState{
		a2aspec.TaskStateWorking,
		a2aspec.TaskStateCompleted,
	}, states(t, answered))
	require.Contains(t, (<-runner.toolResp).Content, "User provided: postgres")
}

// unwindingRunner models a run still unwinding a tool call after its
// context ended: Run reports the context's end on ctxDone, then returns
// only once unwind is closed.
type unwindingRunner struct {
	started chan struct{}
	ctxDone chan struct{}
	unwind  chan struct{}
}

func newUnwindingRunner() *unwindingRunner {
	return &unwindingRunner{
		started: make(chan struct{}),
		ctxDone: make(chan struct{}),
		unwind:  make(chan struct{}),
	}
}

func (r *unwindingRunner) Run(ctx context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	close(r.started)
	<-ctx.Done()
	close(r.ctxDone)
	<-r.unwind
	return nil, ctx.Err()
}

func (r *unwindingRunner) Cancel(string)                               {}
func (r *unwindingRunner) EnqueueWhenBusy(agent.SessionAgentCall) bool { return false }

// startUnwindingRun starts one run of runner on exec and waits until the
// runner is in its turn.
func startUnwindingRun(exec *Executor, runner *unwindingRunner) {
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	go func() {
		for range exec.Execute(context.Background(), newExecCtx(msg)) {
		}
	}()
	<-runner.started
}

// A canceled run can still be unwinding a tool call after Cancel has
// reported the task canceled. stopRuns waits for its goroutine to
// return, so the route's teardown never closes the toolchain or releases
// the workspace under it.
func TestStopRunsJoinsUnwindingRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newUnwindingRunner()
		exec := newBoundExecutor(runner)
		startUnwindingRun(exec, runner)

		collect(t, exec.Cancel(context.Background(), newExecCtx(nil)))
		<-runner.ctxDone

		stopped := make(chan error, 1)
		go func() { stopped <- exec.stopRuns(context.Background()) }()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stopRuns returned while the canceled run was still unwinding")
		default:
		}

		close(runner.unwind)
		require.NoError(t, <-stopped)
	})
}

// stopRuns cancels a run that nothing else canceled and joins it.
func TestStopRunsCancelsLiveRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newUnwindingRunner()
		close(runner.unwind)
		exec := newBoundExecutor(runner)
		startUnwindingRun(exec, runner)

		require.NoError(t, exec.stopRuns(context.Background()))
		select {
		case <-runner.ctxDone:
		default:
			t.Fatal("stopRuns returned without canceling the run")
		}
	})
}

// A run that never returns cannot hold stopRuns past its context.
func TestStopRunsHonorsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newUnwindingRunner()
		exec := newBoundExecutor(runner)
		startUnwindingRun(exec, runner)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, exec.stopRuns(ctx), context.DeadlineExceeded)

		close(runner.unwind)
	})
}

// steeringRunner holds its turn until released, then ends it with the
// fake's result and error; steers it accepts wait in the fake's queue
// until consumed, or until ClearQueue drops them unread the way the
// session agent does (#398).
type steeringRunner struct {
	fakeRunner
	started chan struct{}
	release chan struct{}
}

func newSteeringRunner(result *fantasy.AgentResult, err error, accept bool) *steeringRunner {
	return &steeringRunner{
		fakeRunner: fakeRunner{result: result, err: err, enqueueAccepted: accept},
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (r *steeringRunner) Run(context.Context, agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	close(r.started)
	<-r.release
	return r.result, r.err
}

func (r *steeringRunner) ClearQueue(string) {
	for len(r.enqueued()) > 0 {
		r.consume(false)
	}
}

// drainEvents collects a sequence's events off the test goroutine.
func drainEvents(seq iter.Seq2[a2aspec.Event, error]) []a2aspec.Event {
	var evs []a2aspec.Event
	for ev, err := range seq {
		if err != nil {
			break
		}
		evs = append(evs, ev)
	}
	return evs
}

// undeliveredSteers decodes the undelivered-steers/v1 value on a status.
func undeliveredSteers(t *testing.T, ev a2aspec.Event) []string {
	t.Helper()
	raw, ok := statusUpdate(t, ev).Meta()[UndeliveredSteersExt.URI]
	if !ok {
		return nil
	}
	decoded, err := DecodeValue(UndeliveredSteersExt, raw)
	require.NoError(t, err)
	return decoded.(*agent.UndeliveredSteers).Steers
}

// A steer accepted while the run is working, which the run then never
// reads because it fails, is named on the run's Failed status, and the
// steer's own task fails (#398). A steer the run did read is not.
func TestExecuteUndeliveredSteersOnFailedRun(t *testing.T) {
	t.Parallel()

	runner := newSteeringRunner(nil, errors.New("provider exploded"), true)
	exec := newBoundExecutor(runner)

	mainEvs := make(chan []a2aspec.Event, 1)
	go func() {
		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
		mainEvs <- drainEvents(exec.Execute(context.Background(), newExecCtx(msg)))
	}()
	<-runner.started

	steer := func(text string) chan []a2aspec.Event {
		out := make(chan []a2aspec.Event, 1)
		before := len(runner.enqueued())
		go func() { out <- drainEvents(exec.Execute(context.Background(), newExecCtx(steerOnContext(text, nil)))) }()
		require.Eventually(t, func() bool { return len(runner.enqueued()) == before+1 },
			10*time.Second, 5*time.Millisecond, "the steer was never enqueued")
		return out
	}
	read := steer("read me")
	runner.consume(true)
	unread := steer("also update the docs")

	close(runner.release)
	evs := <-mainEvs
	last := evs[len(evs)-1]
	require.Equal(t, a2aspec.TaskStateFailed, statusUpdate(t, last).Status.State)
	require.Equal(t, []string{"also update the docs"}, undeliveredSteers(t, last),
		"only the steer the run never read is named")

	readEvs := <-read
	require.Equal(t, a2aspec.TaskStateCompleted, statusUpdate(t, readEvs[len(readEvs)-1]).Status.State)
	unreadEvs := <-unread
	require.Equal(t, a2aspec.TaskStateFailed, statusUpdate(t, unreadEvs[len(unreadEvs)-1]).Status.State)
	require.Contains(t, statusMessageText(t, unreadEvs[len(unreadEvs)-1]), "agent finished before the message was consumed")
}

// A run whose steers were all read reports none (#398).
func TestExecuteNoUndeliveredSteersWhenAllRead(t *testing.T) {
	t.Parallel()

	runner := newSteeringRunner(textResult("work done"), nil, true)
	exec := newBoundExecutor(runner)
	mainEvs := make(chan []a2aspec.Event, 1)
	go func() {
		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
		mainEvs <- drainEvents(exec.Execute(context.Background(), newExecCtx(msg)))
	}()
	<-runner.started
	steerEvs := make(chan []a2aspec.Event, 1)
	go func() {
		steerEvs <- drainEvents(exec.Execute(context.Background(), newExecCtx(steerOnContext("folded in", nil))))
	}()
	require.Eventually(t, func() bool { return len(runner.enqueued()) == 1 }, 10*time.Second, 5*time.Millisecond)
	runner.consume(true)
	<-steerEvs

	close(runner.release)
	evs := <-mainEvs
	last := evs[len(evs)-1]
	require.Equal(t, a2aspec.TaskStateCompleted, statusUpdate(t, last).Status.State)
	require.Nil(t, undeliveredSteers(t, last))
}

// A steer the runner refuses while the run is still live — its session
// not busy yet — is refused as not ready, with the typed reason, never as
// a finished run (#398). Once the run has returned the refusal is final.
func TestExecuteSteerRefusedNotReadyWhileRunLive(t *testing.T) {
	t.Parallel()

	runner := newSteeringRunner(textResult("work done"), nil, false)
	exec := newBoundExecutor(runner)
	mainEvs := make(chan []a2aspec.Event, 1)
	go func() {
		msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
		mainEvs <- drainEvents(exec.Execute(context.Background(), newExecCtx(msg)))
	}()
	<-runner.started

	evs := collect(t, exec.Execute(context.Background(), newExecCtx(steerOnContext("too early", nil))))
	refused := statusUpdate(t, evs[len(evs)-1])
	require.Equal(t, a2aspec.TaskStateRejected, refused.Status.State)
	require.Contains(t, statusMessageText(t, evs[len(evs)-1]), "not ready for messages yet")
	raw, ok := refused.Meta()[SteerRefusalExt.URI]
	require.True(t, ok, "the refusal carries its typed reason")
	decoded, err := DecodeValue(SteerRefusalExt, raw)
	require.NoError(t, err)
	require.Equal(t, agent.SteerRefusalNotReady, decoded.(*agent.SteerRefusal).Reason)

	close(runner.release)
	<-mainEvs
	evs = collect(t, exec.Execute(context.Background(), newExecCtx(steerOnContext("too late", nil))))
	ended := statusUpdate(t, evs[len(evs)-1])
	require.Contains(t, statusMessageText(t, evs[len(evs)-1]), "no longer running")
	_, ok = ended.Meta()[SteerRefusalExt.URI]
	require.False(t, ok, "a finished run's refusal carries no retry reason")
}
