package a2a

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// todoState is the sentinel states uses for todo Working events so
// ordering can be asserted in one shot.
const todoState a2aspec.TaskState = "<todo>"

// statesWithTodos summarizes an event stream distinguishing todo Working
// updates from the initial Working status.
func statesWithTodos(t *testing.T, evs []a2aspec.Event) []a2aspec.TaskState {
	t.Helper()
	out := make([]a2aspec.TaskState, 0, len(evs))
	for _, ev := range evs {
		switch e := ev.(type) {
		case *a2aspec.Task:
			out = append(out, e.Status.State)
		case *a2aspec.TaskStatusUpdateEvent:
			if e.Status.State == a2aspec.TaskStateWorking && e.Status.Message != nil {
				out = append(out, todoState)
			} else {
				out = append(out, e.Status.State)
			}
		case *a2aspec.TaskArtifactUpdateEvent:
			out = append(out, artifactState)
		default:
			t.Fatalf("unexpected event type %T", ev)
		}
	}
	return out
}

// pipeTodoSource is a fake [TodoSource] the test feeds by hand. It also
// records the subscription context, so tests can assert the executor
// unsubscribes when the run ends.
type pipeTodoSource struct {
	ch chan dispatch.TodoSnapshot

	mu       sync.Mutex
	subCtx   context.Context
	subCount int
}

func newPipeTodoSource() *pipeTodoSource {
	return &pipeTodoSource{ch: make(chan dispatch.TodoSnapshot)}
}

func (p *pipeTodoSource) SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot {
	p.mu.Lock()
	p.subCtx = ctx
	p.subCount++
	p.mu.Unlock()
	return p.ch
}

// subscriptionCanceled reports whether the subscription context has
// ended.
func (p *pipeTodoSource) subscriptionCanceled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.subCtx == nil {
		return false
	}
	return p.subCtx.Err() != nil
}

// pacedRunner is a runner whose Run blocks until released, so a test can
// push todo snapshots while the run is in flight and then finish it.
type pacedRunner struct {
	fakeRunner
	release  chan struct{}
	started  chan struct{}
	once     sync.Once
	ranCtx   context.Context
	ranCount int
}

func newPacedRunner(result string) *pacedRunner {
	return &pacedRunner{
		fakeRunner: fakeRunner{result: textResult(result)},
		release:    make(chan struct{}),
		started:    make(chan struct{}),
	}
}

func (p *pacedRunner) Run(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	p.once.Do(func() { close(p.started) })
	p.ranCtx = ctx
	p.ranCount++
	<-p.release
	return p.fakeRunner.Run(ctx, call)
}

func todoSnapshot(todos []session.Todo) dispatch.TodoSnapshot {
	snap := dispatch.TodoSnapshot{
		Entry: dispatch.Entry{SessionID: "sess-1"},
		Todos: todos,
	}
	for _, todo := range todos {
		snap.TodoTotal++
		if todo.Status == session.TodoStatusCompleted {
			snap.TodoCompleted++
		}
		if todo.Status == session.TodoStatusInProgress && snap.CurrentTodo == "" {
			snap.CurrentTodo = todo.Content
			if todo.ActiveForm != "" {
				snap.CurrentTodo = todo.ActiveForm
			}
		}
	}
	return snap
}

func runTodos(todos ...session.Todo) []session.Todo { return todos }

// The happy path (#174's DoD): a runner that mutates session todos
// mid-run yields submitted → working → working(todo)… → artifact →
// completed, with the structured todo snapshot in each todo event's
// metadata. Unchanged and empty todo lists stay silent.
func TestExecuteTodoHappyPath(t *testing.T) {
	t.Parallel()

	runner := newPacedRunner("all done")
	source := newPipeTodoSource()
	exec := newBoundExecutor(runner, WithTodos(source), WithDiff(func(context.Context) (string, error) {
		return "the diff", nil
	}))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("do the thing"))
	evsCh := make(chan []a2aspec.Event, 1)
	go func() {
		evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
	}()

	<-runner.started
	source.ch <- todoSnapshot(runTodos(
		session.Todo{Content: "read the code", Status: session.TodoStatusInProgress, ActiveForm: "reading the code"},
		session.Todo{Content: "write the fix", Status: session.TodoStatusPending},
	))
	// An unchanged list and an empty one must not produce events.
	source.ch <- todoSnapshot(runTodos(
		session.Todo{Content: "read the code", Status: session.TodoStatusInProgress, ActiveForm: "reading the code"},
		session.Todo{Content: "write the fix", Status: session.TodoStatusPending},
	))
	source.ch <- todoSnapshot(nil)
	// A real change does.
	source.ch <- todoSnapshot(runTodos(
		session.Todo{Content: "read the code", Status: session.TodoStatusCompleted},
		session.Todo{Content: "write the fix", Status: session.TodoStatusInProgress, ActiveForm: "writing the fix"},
	))
	close(source.ch)
	close(runner.release)

	select {
	case evs := <-evsCh:
		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			todoState,
			todoState,
			artifactState,
			artifactState,
			a2aspec.TaskStateCompleted,
		}
		require.Equal(t, want, statesWithTodos(t, evs))

		first := statusUpdate(t, evs[2])
		require.Equal(t, "reading the code", statusMessageText(t, evs[2]))
		progress, err := Decode[agent.TodoProgress](first.Meta(), TodoExt)
		require.NoError(t, err)
		require.Equal(t, agent.TodoProgress{
			Current:   "reading the code",
			Completed: 0,
			Total:     2,
			Todos: []agent.TodoItem{
				{Content: "read the code", Status: string(session.TodoStatusInProgress), ActiveForm: "reading the code"},
				{Content: "write the fix", Status: string(session.TodoStatusPending), ActiveForm: ""},
			},
		}, progress)

		second := statusUpdate(t, evs[3])
		require.Equal(t, "writing the fix", statusMessageText(t, evs[3]))
		secondProgress, err := Decode[*agent.TodoProgress](second.Meta(), TodoExt)
		require.NoError(t, err)
		require.Len(t, secondProgress.Todos, 2)
		require.Equal(t, string(session.TodoStatusCompleted), secondProgress.Todos[0].Status)

		// The subscription ended with the run.
		require.Eventually(t, source.subscriptionCanceled, 5*time.Second, 10*time.Millisecond)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the event sequence")
	}
}

// The message falls back to an N/M completed summary when no todo is in
// progress.
func TestExecuteTodoFallbackSummary(t *testing.T) {
	t.Parallel()

	runner := newPacedRunner("done")
	source := newPipeTodoSource()
	exec := newBoundExecutor(runner, WithTodos(source))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evsCh := make(chan []a2aspec.Event, 1)
	go func() {
		evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
	}()

	<-runner.started
	source.ch <- todoSnapshot(runTodos(
		session.Todo{Content: "read the code", Status: session.TodoStatusCompleted},
		session.Todo{Content: "write the fix", Status: session.TodoStatusCompleted},
	))
	close(source.ch)
	close(runner.release)

	select {
	case evs := <-evsCh:
		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			todoState,
			a2aspec.TaskStateCompleted,
		}
		require.Equal(t, want, statesWithTodos(t, evs))
		require.Equal(t, "2/2 completed", statusMessageText(t, evs[2]))
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the event sequence")
	}
}

// No todo events after the terminal status: snapshots pushed after the
// run finished are never delivered, even though the source channel is
// still open.
func TestExecuteNoTodoEventsAfterTerminal(t *testing.T) {
	t.Parallel()

	runner := newPacedRunner("done")
	source := newPipeTodoSource()
	exec := newBoundExecutor(runner, WithTodos(source))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evsCh := make(chan []a2aspec.Event, 1)
	go func() {
		evsCh <- collect(t, exec.Execute(context.Background(), newExecCtx(msg)))
	}()

	<-runner.started
	close(runner.release)
	select {
	case evs := <-evsCh:
		want := []a2aspec.TaskState{
			a2aspec.TaskStateSubmitted,
			a2aspec.TaskStateWorking,
			a2aspec.TaskStateCompleted,
		}
		require.Equal(t, want, statesWithTodos(t, evs))

		// The sequence is done; late snapshots go nowhere. The send
		// must not block the producer forever, so use a non-blocking
		// send with a short grace window for the subscription teardown.
		require.Eventually(t, source.subscriptionCanceled, 5*time.Second, 10*time.Millisecond)
		select {
		case source.ch <- todoSnapshot(runTodos(session.Todo{Content: "late", Status: session.TodoStatusInProgress})):
		default:
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the event sequence")
	}
}

// A consumer that stops mid-run ends the event stream: no further todo
// events are delivered, the subscription is dropped, and the sequence
// returns without a terminal status of its own.
func TestExecuteConsumerStopEndsTodoStream(t *testing.T) {
	t.Parallel()

	runner := newPacedRunner("done")
	source := newPipeTodoSource()
	exec := newBoundExecutor(runner, WithTodos(source))

	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		count := 0
		for range exec.Execute(context.Background(), newExecCtx(msg)) {
			count++
			if count == 3 { // submitted, working, first todo
				break
			}
		}
	}()

	<-runner.started
	source.ch <- todoSnapshot(runTodos(
		session.Todo{Content: "read the code", Status: session.TodoStatusInProgress, ActiveForm: "reading the code"},
	))
	<-done

	// The consumer is gone: the subscription ends and later snapshots
	// are not delivered anywhere.
	require.Eventually(t, source.subscriptionCanceled, 5*time.Second, 10*time.Millisecond)
	select {
	case source.ch <- todoSnapshot(runTodos(session.Todo{Content: "late", Status: session.TodoStatusInProgress})):
	default:
	}
	close(runner.release)
}

// The cancel race: canceling the executor context mid-run ends the
// sequence without a terminal status (the executor's Cancel emits the
// Canceled status itself), and no todo events escape after the cancel.
func TestExecuteTodoStreamCanceledMidRun(t *testing.T) {
	t.Parallel()

	runner := newPacedRunner("done")
	source := newPipeTodoSource()
	exec := newBoundExecutor(runner, WithTodos(source))

	ctx, cancel := context.WithCancel(context.Background())
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("go"))
	evsCh := make(chan []a2aspec.Event, 1)
	go func() {
		evsCh <- collect(t, exec.Execute(ctx, newExecCtx(msg)))
	}()

	<-runner.started
	cancel()

	select {
	case evs := <-evsCh:
		for _, st := range statesWithTodos(t, evs) {
			require.NotEqual(t, a2aspec.TaskStateCompleted, st, "terminal status after cancel")
			require.NotEqual(t, a2aspec.TaskStateFailed, st, "failure status after cancel")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the event sequence")
	}

	// No todo events after cancel: a queued snapshot is never consumed.
	select {
	case source.ch <- todoSnapshot(runTodos(session.Todo{Content: "late", Status: session.TodoStatusInProgress})):
	default:
	}
	require.Eventually(t, source.subscriptionCanceled, 5*time.Second, 10*time.Millisecond)
	close(runner.release)
}
