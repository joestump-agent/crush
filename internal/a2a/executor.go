package a2a

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"slices"
	"strings"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
)

// Runner is the slice of [agent.SessionAgent] the [Executor] drives. The full
// SessionAgent interface satisfies it; the narrow interface documents exactly
// what A2A execution depends on and keeps the executor unit-testable with a
// fake instead of the whole agent surface.
type Runner interface {
	Run(context.Context, agent.SessionAgentCall) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
}

// Compile-time proof that a real SessionAgent can be used as a Runner.
var _ Runner = agent.SessionAgent(nil)

// DiffFunc returns the git diff produced by a dispatched run, emitted as the
// task's completion artifact. It is called after a successful run. An empty
// string or an error yields no artifact (the run still completes).
type DiffFunc func(ctx context.Context) (string, error)

// TodoSource supplies per-session todo snapshots while a dispatched run is
// in flight (#174): the dispatch registry's [dispatch.TodoCollector]
// satisfies it — its reduction of the session event stream is the single
// subscription, and this executor is its second consumer after the agent
// block (#65). A nil source disables todo progress events; the run
// lifecycle is unchanged.
type TodoSource interface {
	SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot
}

// Compile-time proof that the collector is a TodoSource.
var _ TodoSource = (*dispatch.TodoCollector)(nil)

// todoMetadataKey is the TaskStatusUpdateEvent metadata key carrying the
// structured todo snapshot, so consumers (#71) can render a checklist
// without parsing the message prose.
const todoMetadataKey = "todos"

// Executor adapts a Crush [agent.SessionAgent] to the [a2asrv.AgentExecutor]
// interface: it runs one dispatched agent turn, maps the run lifecycle onto
// A2A task states (submitted -> working -> completed/failed), and emits the git
// diff as the terminal artifact.
//
// While the run is in flight it also streams progress: one non-terminal
// Working TaskStatusUpdateEvent per todo-list change (#174), with the
// current activity as the message text and the structured todo snapshot in
// the event metadata. Terminal semantics (Completed/Failed/Rejected/Canceled,
// artifact emission) are unchanged, and todo events never race the terminal
// status — both are yielded from this iterator's single goroutine.
type Executor struct {
	runner    Runner
	sessionID string
	diff      DiffFunc
	todos     TodoSource
}

// Option configures an [Executor].
type Option func(*Executor)

// WithDiff sets the function used to collect the completion artifact — the git
// diff of the dispatched worktree. Without it, runs complete with their text
// output and no artifact. See [GitDiff] for the default production collector.
func WithDiff(fn DiffFunc) Option {
	return func(e *Executor) { e.diff = fn }
}

// WithTodos sets the source of per-session todo snapshots streamed as
// non-terminal Working TaskStatusUpdateEvents while the run is in flight
// (#174). The production source is the dispatch registry's todo collector;
// without it, runs emit only the initial Working status.
func WithTodos(source TodoSource) Option {
	return func(e *Executor) { e.todos = source }
}

// NewExecutor builds an Executor that drives runner against sessionID — the
// (ephemeral) session backing the dispatched agent.
func NewExecutor(runner Runner, sessionID string, opts ...Option) *Executor {
	e := &Executor{runner: runner, sessionID: sessionID}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// Execute runs one dispatched agent turn. It announces the task submitted
// (for a new task), emits Working, invokes the SessionAgent, then emits the
// diff artifact (if any) and a terminal Completed status carrying the agent's
// text output. A run error maps to a Failed status with the error surfaced;
// per the AgentExecutor contract, failures after work has begun are reported
// as events, not as a returned error.
func (e *Executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	return func(yield func(a2aspec.Event, error) bool) {
		// A message that referenced no existing task starts a new one:
		// announce it submitted before transitioning to working.
		if execCtx.StoredTask == nil {
			if !yield(a2aspec.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}

		// A message with no text carries nothing to run — SessionAgent.Run
		// would bounce it as an empty prompt — so reject the task without
		// starting a turn. Rejected is the terminal state for "was not
		// started"; Failed is reserved for work that began and broke.
		prompt := messageText(execCtx.Message)
		if prompt == "" {
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateRejected,
				agentMessage(execCtx, "message has no text to run")), nil)
			return
		}

		if !yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateWorking, nil), nil) {
			return
		}

		result, err := e.runWithTodos(ctx, execCtx, prompt, yield)
		switch {
		case errors.Is(err, errConsumerStopped):
			// The consumer stopped consuming mid-run: nothing further can
			// be delivered, and the run's own outcome is dropped with it.
			return
		case errors.Is(err, context.Canceled):
			// The run was canceled — by this executor's Cancel, which
			// emits the terminal Canceled status itself. A Failed status
			// here would race it.
			return
		case err != nil:
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed,
				agentMessage(execCtx, err.Error())), nil)
			return
		case result == nil:
			// Run returns (nil, nil) without doing any work when the
			// session is busy (the prompt was silently queued behind the
			// active turn) or a cancel landed during dispatch. No turn ran
			// on behalf of this task, so completing it would misreport;
			// fail it and let the caller retry against an idle session.
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed,
				agentMessage(execCtx, "agent session did not start a turn (busy or canceled)")), nil)
			return
		}

		if e.diff != nil {
			if diff, derr := e.diff(ctx); derr == nil && diff != "" {
				if !yield(a2aspec.NewArtifactEvent(execCtx, a2aspec.NewTextPart(diff)), nil) {
					return
				}
			}
		}

		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCompleted,
			agentMessage(execCtx, result.Response.Content.Text())), nil)
	}
}

// errConsumerStopped reports that the event consumer stopped consuming
// mid-run: no further events can be delivered and the run's outcome is
// dropped with the stream.
var errConsumerStopped = errors.New("a2a: event consumer stopped")

// runWithTodos invokes the runner while streaming the run's todo progress
// (#174): the run executes on its own goroutine and the todo subscription
// is drained inline on the iterator's goroutine, so Working progress events
// and the terminal status share one yield path and can never race. The
// subscription is bounded by the run — created after the initial Working
// status, dropped on run end, consumer stop, and cancel — and a snapshot is
// only emitted when the todo list actually changed, so usage-only session
// saves stay silent.
func (e *Executor) runWithTodos(ctx context.Context, execCtx *a2asrv.ExecutorContext, prompt string, yield func(a2aspec.Event, error) bool) (*fantasy.AgentResult, error) {
	var todoCh <-chan dispatch.TodoSnapshot
	if e.todos != nil {
		// The subscription ends with this call: run end, consumer stop,
		// and cancel all return through the deferred cancel.
		subCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		todoCh = e.todos.SubscribeSessionTodos(subCtx, e.sessionID)
	}

	type runOutcome struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, err := e.runner.Run(ctx, agent.SessionAgentCall{
			SessionID: e.sessionID,
			Prompt:    prompt,
		})
		done <- runOutcome{result, err}
	}()

	var lastTodos []session.Todo
	for {
		// Check cancellation before selecting: a canceled context and a
		// queued snapshot are both ready, and select would pick either,
		// so the pre-check keeps the guarantee that no todo event is
		// emitted after cancel.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			// Canceled — the executor's Cancel emits the terminal Canceled
			// status itself, and the runner is aborting on this same
			// context; reporting anything here would race it.
			return nil, context.Canceled
		case out := <-done:
			return out.result, out.err
		case snap, ok := <-todoCh:
			if !ok {
				todoCh = nil
				continue
			}
			if len(snap.Todos) == 0 || slices.Equal(snap.Todos, lastTodos) {
				continue
			}
			lastTodos = slices.Clone(snap.Todos)
			if !yield(todoStatusUpdate(execCtx, snap), nil) {
				return nil, errConsumerStopped
			}
		}
	}
}

// todoStatusUpdate maps one todo snapshot onto a non-terminal Working
// TaskStatusUpdateEvent (#174): the current activity — the in-progress
// todo's active form or content — as the message text, falling back to an
// N/M completed summary, and the structured todo list under
// [todoMetadataKey] in the event metadata so consumers can render a
// checklist without parsing the prose.
func todoStatusUpdate(execCtx *a2asrv.ExecutorContext, snap dispatch.TodoSnapshot) *a2aspec.TaskStatusUpdateEvent {
	text := snap.CurrentTodo
	if text == "" {
		text = fmt.Sprintf("%d/%d completed", snap.TodoCompleted, snap.TodoTotal)
	}
	ev := a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateWorking, agentMessage(execCtx, text))
	ev.SetMeta(todoMetadataKey, snap.Todos)
	return ev
}

// Cancel stops the in-flight dispatched run for this executor's session and
// reports the task canceled.
func (e *Executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	return func(yield func(a2aspec.Event, error) bool) {
		e.runner.Cancel(e.sessionID)
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCanceled, nil), nil)
	}
}

// GitDiff returns a [DiffFunc] that captures the full uncommitted state of
// the git worktree rooted at dir — staged and unstaged changes to tracked
// files plus untracked files — as one unified diff against HEAD, the
// dispatched agent's work product. Everything is staged into a throwaway
// temporary index so the worktree's real index is never touched. A git
// failure, missing repo, or unborn HEAD surfaces as an error, which the
// executor treats as "no artifact" rather than a run failure.
func GitDiff(dir string) DiffFunc {
	return func(ctx context.Context) (string, error) {
		tmp, err := os.CreateTemp("", "crush-a2a-index-")
		if err != nil {
			return "", err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())

		env := append(os.Environ(), "GIT_INDEX_FILE="+tmp.Name())
		git := func(args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
			cmd.Env = env
			return cmd
		}

		// Seed the temp index from HEAD, stage the entire worktree into it
		// (respecting .gitignore, so untracked files are included), and
		// diff that against HEAD.
		if err := git("read-tree", "HEAD").Run(); err != nil {
			return "", err
		}
		if err := git("add", "-A").Run(); err != nil {
			return "", err
		}
		out, err := git("diff", "--cached", "HEAD").Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

// messageText concatenates the text parts of an incoming A2A message into a
// single prompt. Non-text parts are ignored in Phase 1.
func messageText(msg *a2aspec.Message) string {
	if msg == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		if t := part.Text(); t != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
	}
	return b.String()
}

// agentMessage wraps text as an agent-role A2A message stamped with the
// task's IDs, for terminal status-update payloads.
func agentMessage(info a2aspec.TaskInfoProvider, text string) *a2aspec.Message {
	return a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, info, a2aspec.NewTextPart(text))
}
