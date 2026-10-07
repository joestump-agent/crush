package a2a

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// Runner is the slice of [agent.SessionAgent] the [Executor] drives. The full
// SessionAgent interface satisfies it; the narrow interface documents exactly
// what A2A execution depends on and keeps the executor unit-testable with a
// fake instead of the whole agent surface.
type Runner interface {
	Run(context.Context, agent.SessionAgentCall) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
	// EnqueueWhenBusy accepts a steer into the running session's queue
	// and reports whether it was accepted (#351): false means the run
	// has ended and the session refuses the message instead of running
	// another turn on a task session that is never continuable.
	EnqueueWhenBusy(call agent.SessionAgentCall) bool
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

// Executor adapts a Crush [agent.SessionAgent] to the [a2asrv.AgentExecutor]
// interface: it runs one dispatched agent turn, maps the run lifecycle onto
// A2A task states (submitted -> working -> completed/failed), and emits the git
// diff as the terminal artifact.
//
// The executor is bound to a context, not a session (#350): it resolves the
// task's A2A ContextID through the host's [ContextRegistry] to the dispatch
// binding — the runner, the session, and the call template — that owns it.
// A message whose context is unknown, foreign to this route, or whose
// binding has been removed (the run ended; sub-agent sessions are not
// continuable) is rejected without the runner ever being called.
//
// While the run is in flight it also streams progress: one non-terminal
// Working TaskStatusUpdateEvent per todo-list change (#174), with the
// current activity as the message text and the structured todo snapshot in
// the event metadata. Terminal semantics (Completed/Failed/Rejected/Canceled,
// artifact emission) are unchanged, and todo events never race the terminal
// status — both are yielded from this iterator's single goroutine.
//
// While the run is in flight, a second message on the bound context is a
// steer (#351): the executor enqueues it on the running session and
// completes the steer's own task once the message was consumed — folded
// into the active turn or picked up as the follow-up turn — or fails it
// when the run dropped it without running. The reply itself streams back
// on the dispatch's own surfaces, never on the steer task. The first
// message on a context starts the dispatch's own turn; anything after
// that is steering.
type Executor struct {
	// contexts resolves the A2A context ID onto the dispatch binding the
	// turn runs against (#350). The host owns the registry; the binding
	// lives exactly as long as the dispatch run.
	contexts *ContextRegistry
	// contextID is the context this executor's route owns (#350): a task
	// naming any other context — unknown or another dispatch's — is
	// rejected here.
	contextID string
	diff      DiffFunc
	todos     TodoSource
	// inactivityTimeout is the A2A-level backstop (#360): a run that
	// yields no events for this long while in flight is canceled by
	// the executor and failed with the reason. Zero (the default)
	// disables the backstop.
	inactivityTimeout time.Duration
	// endedByExecutor holds the task IDs whose terminal status the
	// executor itself has already emitted (the inactivity backstop's
	// Failed, #360). #342's out-of-band cancel branch consults the set
	// so a run the executor ended is not also reported Canceled.
	mu              sync.Mutex
	endedByExecutor map[string]struct{}
	// cancelMu guards canceledTasks: the task IDs this executor's own
	// Cancel has touched (#342). A run that returns context.Canceled
	// while its task is in the set was ended by that Cancel — which
	// emits the terminal Canceled status itself — while the same error
	// from an out-of-band kill (the wander ladder, the watchdog) must
	// surface as this executor's own Canceled, or the task never
	// reaches a terminal state.
	cancelMu      sync.Mutex
	canceledTasks map[string]struct{}
	// cancelReason reports why the current run was killed (#316), for
	// the Canceled status an out-of-band cancel emits. Optional; a nil
	// func or an empty string falls back to "canceled".
	cancelReason func() string
	// usage reads the dispatched session's final usage once its run has
	// ended (#364). Optional; nil means terminal statuses carry no usage
	// metadata.
	usage func(ctx context.Context) (agent.Usage, error)
	// turnMu guards turnStarted (#351): one executor serves one
	// dispatch's route, and every message on its context either starts
	// the dispatch's own turn — the first one — or is a steer.
	turnMu      sync.Mutex
	turnStarted bool
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

// WithInactivityTimeout sets the A2A-level backstop (#360): while the
// run is in flight, every event the executor yields to the consumer
// resets the timer; when it fires with no progress, the executor marks
// the task as ended by the executor, cancels the runner, and ends the
// run with exactly one Failed carrying the reason. A zero or negative
// duration (the default) disables the backstop.
func WithInactivityTimeout(d time.Duration) Option {
	return func(e *Executor) { e.inactivityTimeout = d }
}

// WithCancelReason sets the reporter for why the dispatched run was killed
// (#316): the reason text carried by the Canceled status an out-of-band
// cancel emits (#342). A nil func or an empty string falls back to
// "canceled".
func WithCancelReason(fn func() string) Option {
	return func(e *Executor) { e.cancelReason = fn }
}

// WithUsage sets the reader for the dispatched session's final usage
// (#364): the executor calls it once the run has ended and attaches the
// value — stamped with the request's W3C trace ID — to every post-run
// terminal status under the usage/v1 extension. A nil func (the default)
// emits no usage metadata.
func WithUsage(fn func(ctx context.Context) (agent.Usage, error)) Option {
	return func(e *Executor) { e.usage = fn }
}

// NewExecutor builds an Executor that serves one dispatch's route (#350):
// every turn resolves its A2A context — contextID, the route's own —
// through contexts to the dispatch binding (runner, session, call
// template) registered there, and a task naming any other context is
// rejected.
func NewExecutor(contexts *ContextRegistry, contextID string, opts ...Option) *Executor {
	e := &Executor{contexts: contexts, contextID: contextID}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// Execute runs one dispatched agent turn. It resolves the task's A2A
// context to the dispatch binding that owns it (#350) — rejecting any
// message whose context is unknown, foreign to this route, or already
// unbound — then announces the task submitted (for a new task), emits
// Working, invokes the SessionAgent, and emits the chunked diff artifact
// (if any), the typed dispatch-result artifact, and a terminal Completed
// status carrying the agent's text output. A run error maps to a Failed
// status with the error surfaced; per the AgentExecutor contract,
// failures after work has begun are reported as events, not as a
// returned error.
func (e *Executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	return func(yield func(a2aspec.Event, error) bool) {
		// The backstop's terminal is the task's last word: drop the
		// "ended by the executor" mark when this sequence ends, so a
		// later run of the same executor starts clean.
		defer e.clearEndedByExecutor(execCtx.TaskID)
		// The task ID leaves the own-cancel set when this sequence ends,
		// so the silent return of an already-emitted Canceled cannot
		// outlive the task it belongs to.
		defer e.forgetCanceledTask(string(execCtx.TaskID))

		// The trace context (#364): the parent's client interceptor sent
		// the W3C traceparent and the server propagator moved it into
		// this request's context. Its trace ID is what the dispatch's
		// server-side log lines carry and what the usage payload echoes
		// back, so a parent turn and its dispatched run correlate.
		traceID := traceIDFromContext(ctx)

		// Resolve the task's context to the dispatch binding (#350):
		// only this route's own, still-bound context runs. Anything else
		// — an unknown context, another dispatch's, or one whose run has
		// ended and taken its binding with it — is rejected without the
		// runner ever being called: task sessions are not continuable.
		binding, ok := e.resolve(execCtx.ContextID)
		if !ok {
			if execCtx.StoredTask == nil {
				if !yield(a2aspec.NewSubmittedTask(execCtx, execCtx.Message), nil) {
					return
				}
			}
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateRejected,
				agentMessage(execCtx, noAgentForContextText(execCtx.ContextID))), nil)
			return
		}
		slog.Debug("A2A dispatch turn starting",
			"context_id", execCtx.ContextID,
			"session_id", binding.SessionID,
			"task_id", string(execCtx.TaskID),
			"trace_id", traceID)

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

		// The first message on the context starts the dispatch's own turn;
		// everything after it is a steer (#351) and never reaches the
		// runner's Run.
		if e.markTurnStarted() {
			e.executeSteer(ctx, execCtx, binding, prompt, yield)
			return
		}

		if !yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateWorking, nil), nil) {
			return
		}

		result, err := e.runWithTodos(ctx, execCtx, binding, prompt, yield)
		switch {
		case errors.Is(err, errConsumerStopped):
			// The consumer stopped consuming mid-run: nothing further can
			// be delivered, and the run's own outcome is dropped with it.
			return
		case errors.Is(err, context.Canceled):
			// A canceled run is either this executor's own Cancel — which
			// emits the terminal Canceled status itself, and a second one
			// here would race it — or the SDK canceling the producer's
			// context, in which case the consumer is gone with the stream.
			// The same silence holds for a task the inactivity backstop
			// already failed (#360): its reason-bearing Failed is the
			// task's last word. Any other cancel is out of band (#342):
			// the wander ladder or the watchdog killed the agent behind
			// the SDK's back, nothing else will emit a terminal state, and
			// this stream is the consumer's only way out — so yield
			// exactly one Canceled carrying the kill reason.
			if e.ownCancel(string(execCtx.TaskID)) ||
				e.endedByExecutorHas(string(execCtx.TaskID)) || ctx.Err() != nil {
				return
			}
			ev := a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCanceled,
				agentMessage(execCtx, e.canceledStatusText()))
			e.attachUsage(ctx, ev, binding.SessionID, traceID)
			yield(ev, nil)
			return
		case err != nil:
			ev := a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed,
				agentMessage(execCtx, err.Error()))
			e.attachUsage(ctx, ev, binding.SessionID, traceID)
			yield(ev, nil)
			return
		case result == nil:
			// Run returns (nil, nil) without doing any work when the
			// session is busy (the prompt was silently queued behind the
			// active turn) or a cancel landed during dispatch. No turn ran
			// on behalf of this task, so completing it would misreport;
			// fail it and let the caller retry against an idle session.
			ev := a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed,
				agentMessage(execCtx, "agent session did not start a turn (busy or canceled)"))
			e.attachUsage(ctx, ev, binding.SessionID, traceID)
			yield(ev, nil)
			return
		}

		if e.diff != nil {
			diff, derr := e.diff(ctx)
			if derr == nil && diff != "" {
				// The diff streams as chunked text/x-diff parts of one
				// named artifact (#361): no single SSE data line carries
				// more than a 256 KiB piece, so a huge diff cannot trip
				// the SDK's 10 MB line cap.
				chunks := chunkDiff(diff)
				for i := range chunks {
					if !yield(diffArtifact(execCtx, chunks, i), nil) {
						return
					}
				}
			}
			// The typed outcome rides alongside — and stands in for the
			// diff when capture failed — so the error crosses the wire
			// and the run still completes (#361).
			outcome := DispatchOutcome{DiffBytes: len(diff)}
			if derr != nil {
				outcome.DiffError = derr.Error()
			} else {
				outcome.FilesChanged = countDiffFiles(diff)
			}
			if !yield(resultArtifact(execCtx, outcome), nil) {
				return
			}
		}

		ev := a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCompleted,
			agentMessage(execCtx, result.Response.Content.Text()))
		e.attachUsage(ctx, ev, binding.SessionID, traceID)
		yield(ev, nil)
	}
}

// errConsumerStopped reports that the event consumer stopped consuming
// mid-run: no further events can be delivered and the run's outcome is
// dropped with the stream.
var errConsumerStopped = errors.New("a2a: event consumer stopped")

// resolve maps the request's A2A context onto the dispatch binding that
// owns it (#350). Only this route's own, still-bound context resolves:
// an unknown context, another dispatch's, or one whose binding was
// removed when its run ended all reject with the same terminal message.
func (e *Executor) resolve(contextID string) (ContextBinding, bool) {
	if contextID != e.contextID || e.contexts == nil {
		return ContextBinding{}, false
	}
	return e.contexts.Lookup(contextID)
}

// noAgentForContextText is the rejection message for a task whose
// context resolves to no running agent (#350): unknown, foreign, or
// already unbound — sub-agent sessions are not continuable.
func noAgentForContextText(contextID string) string {
	return fmt.Sprintf("no running agent for context %s; task sessions are not continuable", contextID)
}

// runWithTodos invokes the binding's runner while streaming the run's todo
// progress (#174): the run executes on its own goroutine and the todo
// subscription is drained inline on the iterator's goroutine, so Working
// progress events and the terminal status share one yield path and can
// never race. The turn runs against the resolved binding (#350) — its
// session, its call template, its runner — and the task ID is stamped as
// the call's RunID, so the run's terminal RunComplete event names the A2A
// task that started it. The subscription is bounded by the run — created
// after the initial Working status, dropped on run end, consumer stop,
// and cancel — and a snapshot is only emitted when the todo list actually
// changed, so usage-only session saves stay silent.
func (e *Executor) runWithTodos(ctx context.Context, execCtx *a2asrv.ExecutorContext, binding ContextBinding, prompt string, yield func(a2aspec.Event, error) bool) (*fantasy.AgentResult, error) {
	var todoCh <-chan dispatch.TodoSnapshot
	if e.todos != nil {
		// The subscription ends with this call: run end, consumer stop,
		// and cancel all return through the deferred cancel.
		subCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		todoCh = e.todos.SubscribeSessionTodos(subCtx, binding.SessionID)
	}

	type runOutcome struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		// A panic in a tool or provider adapter must fail this task, not
		// the whole process (#345): recover it, log it, and hand a run
		// error back so Execute yields its single Failed status. The
		// error uses %v and never wraps, so it cannot be
		// context.Canceled and the canceled branch of Execute cannot
		// swallow it (#342).
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Dispatched run panicked", "session_id", binding.SessionID, "trace_id", traceIDFromContext(ctx), "panic", r, "stack", string(debug.Stack()))
				done <- runOutcome{err: fmt.Errorf("dispatched run panicked: %v", r)}
			}
		}()
		call := binding.Call
		call.SessionID = binding.SessionID
		call.Prompt = prompt
		// The task is the run (#350): the RunComplete event echoes the
		// A2A task ID back as RunID, so the task and the run correlate.
		call.RunID = string(execCtx.TaskID)
		result, err := binding.Runner.Run(ctx, call)
		done <- runOutcome{result, err}
	}()

	// The inactivity backstop (#360): a timer that every event the
	// executor yields resets, so a run making visible progress never
	// trips it and a silent one is ended with a reason. Disabled when
	// the duration is zero or negative.
	var inactC <-chan time.Time
	var inactReset func()
	if e.inactivityTimeout > 0 {
		inactTimer := time.NewTimer(e.inactivityTimeout)
		defer inactTimer.Stop()
		inactC = inactTimer.C
		inactReset = func() {
			if inactTimer.Reset(e.inactivityTimeout) {
				// The timer fired but its value was not consumed:
				// drain it, or it would trip the backstop again on
				// the next loop pass.
				select {
				case <-inactTimer.C:
				default:
				}
			}
		}
	}

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
		case <-inactC:
			// The backstop fired: no event was yielded for the whole
			// window. End the run with exactly one Failed carrying the
			// reason — never a Canceled, which is reserved for the
			// executor's Cancel (#360, coordinated with #342).
			e.markEndedByExecutor(execCtx.TaskID)
			binding.Runner.Cancel(binding.SessionID)
			// Give the runner a brief window to observe the cancel and
			// settle; a truly wedged run ignores it, and the outcome is
			// discarded either way — the task fails with the reason.
			select {
			case <-done:
			case <-time.After(inactivitySettleWindow):
			}
			return nil, fmt.Errorf("inactivity timeout: no progress for %s", e.inactivityTimeout)
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
			if inactReset != nil {
				inactReset()
			}
		}
	}
}

// inactivitySettleWindow bounds the backstop's wait for the run goroutine
// after canceling the runner (#360): long enough for a responsive runner
// to observe the cancel, short enough that a wedged one delays the
// terminal Failed by seconds, not minutes.
const inactivitySettleWindow = 2 * time.Second

// markEndedByExecutor records that the executor itself has emitted the
// task's terminal status — the inactivity backstop's Failed (#360) — so
// #342's out-of-band cancel branch, which lands separately, does not
// also emit a Canceled status for the same task.
func (e *Executor) markEndedByExecutor(taskID a2aspec.TaskID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.endedByExecutor == nil {
		e.endedByExecutor = make(map[string]struct{})
	}
	e.endedByExecutor[string(taskID)] = struct{}{}
}

// clearEndedByExecutor drops the mark: each run ends with at most one
// terminal of the executor's own, so a finished task's ID is safe to
// forget before the next run.
func (e *Executor) clearEndedByExecutor(taskID a2aspec.TaskID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.endedByExecutor, string(taskID))
}

// todoStatusUpdate maps one todo snapshot onto a non-terminal Working
// TaskStatusUpdateEvent (#174): the current activity — the in-progress
// todo's active form or content — as the message text, falling back to an
// N/M completed summary, and the typed todo progress under the declared
// todos/v1 extension's URI ([TodoExtensionURI]) in the event metadata, so
// consumers that negotiated the extension can render a checklist without
// parsing the prose.
func todoStatusUpdate(execCtx *a2asrv.ExecutorContext, snap dispatch.TodoSnapshot) *a2aspec.TaskStatusUpdateEvent {
	text := snap.CurrentTodo
	if text == "" {
		text = fmt.Sprintf("%d/%d completed", snap.TodoCompleted, snap.TodoTotal)
	}
	ev := a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateWorking, agentMessage(execCtx, text))
	if encoded, err := Encode(TodoExt, todoProgress(snap)); err != nil {
		// The text still carries the progress; only the structured
		// checklist is lost.
		slog.Warn("A2A todo progress failed to encode; event carries no extension metadata", "err", err)
	} else {
		ev.SetMeta(TodoExt.URI, encoded)
	}
	return ev
}

// todoProgress reduces a todo snapshot into the todos/v1 extension's typed
// payload.
func todoProgress(snap dispatch.TodoSnapshot) agent.TodoProgress {
	items := make([]agent.TodoItem, 0, len(snap.Todos))
	for _, todo := range snap.Todos {
		items = append(items, agent.TodoItem{
			Content:    todo.Content,
			Status:     string(todo.Status),
			ActiveForm: todo.ActiveForm,
		})
	}
	return agent.TodoProgress{
		Current:   snap.CurrentTodo,
		Completed: snap.TodoCompleted,
		Total:     snap.TodoTotal,
		Todos:     items,
	}
}

// attachUsage stamps a terminal status update with the usage/v1 extension's
// payload (#364): the usage closure's reading of the dispatched session's
// final totals, plus the request's W3C trace ID. Called only on post-run
// terminals — a status emitted mid-run (the executor's own Cancel) carries
// no usage, because the run's totals are not final. Every failure — the
// closure erroring, or the value failing to encode — is logged, and the
// status ships without metadata: usage is accounting, never a reason to
// fail a finished run.
func (e *Executor) attachUsage(ctx context.Context, ev *a2aspec.TaskStatusUpdateEvent, sessionID, traceID string) {
	if e.usage == nil {
		return
	}
	// The closure reads the session row; an ended run's context can be
	// on its way out, and the totals are durable regardless.
	usage, err := e.usage(context.WithoutCancel(ctx))
	if err != nil {
		slog.Warn("A2A usage collection failed; terminal status carries no usage metadata", "session_id", sessionID, "trace_id", traceID, "err", err)
		return
	}
	usage.TraceID = traceID
	encoded, err := Encode(UsageExt, usage)
	if err != nil {
		slog.Warn("A2A usage failed to encode; terminal status carries no usage metadata", "session_id", sessionID, "trace_id", traceID, "err", err)
		return
	}
	ev.SetMeta(UsageExt.URI, encoded)
}

// traceIDFromContext returns the trace-id segment of the W3C traceparent
// the server propagator lifted into the request context (#364), or the
// empty string when the call carried none.
func traceIDFromContext(ctx context.Context) string {
	for key, values := range a2aext.GetRequestHeaders(ctx) {
		if strings.EqualFold(key, traceparentHeader) && len(values) > 0 {
			return agent.TraceIDFromTraceparent(values[0])
		}
	}
	return ""
}

// Cancel stops the in-flight dispatched run for this executor's context
// and reports the task canceled. The task is marked as this executor's own
// cancel before the runner aborts (#342), so the run's returning
// context.Canceled takes the silent path and this Canceled status stays
// the only terminal one. The reason travels the protocol (#348): the
// cancel request's declared metadata carries it, and it lands on the
// terminal Canceled status message so the caller — and any tasks/get
// reader — sees why the run stopped. Without one, the in-process kill
// reason (an out-of-band cancel that bypassed tasks/cancel, #342) is
// used, falling back to the generic "canceled". A cancel whose context
// resolves to no running agent (#350) reports the run already ended and
// touches no runner.
func (e *Executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	return func(yield func(a2aspec.Event, error) bool) {
		binding, ok := e.resolve(execCtx.ContextID)
		if !ok {
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCanceled,
				agentMessage(execCtx, noAgentForContextText(execCtx.ContextID))), nil)
			return
		}
		e.markOwnCancel(string(execCtx.TaskID))
		binding.Runner.Cancel(binding.SessionID)
		text := cancelReasonFromMetadata(execCtx.Metadata)
		if text == "" {
			text = e.canceledStatusText()
		}
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCanceled, agentMessage(execCtx, text)), nil)
	}
}

// markOwnCancel records taskID as canceled by this executor's own Cancel.
func (e *Executor) markOwnCancel(taskID string) {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	if e.canceledTasks == nil {
		e.canceledTasks = make(map[string]struct{})
	}
	e.canceledTasks[taskID] = struct{}{}
}

// ownCancel reports whether taskID was canceled by this executor's own
// Cancel.
func (e *Executor) ownCancel(taskID string) bool {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	_, ok := e.canceledTasks[taskID]
	return ok
}

// forgetCanceledTask drops taskID from the own-cancel set: the task's
// execution sequence has ended, one way or the other.
func (e *Executor) forgetCanceledTask(taskID string) {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	delete(e.canceledTasks, taskID)
}

// endedByExecutorHas reports whether the executor itself already emitted
// the task's terminal status (the inactivity backstop's Failed, #360):
// that Failed is the task's last word, and a cancel that observes it
// stays silent.
func (e *Executor) endedByExecutorHas(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.endedByExecutor[taskID]
	return ok
}

// canceledStatusText is the message text for an out-of-band Canceled
// status: the kill reason when one is wired and non-empty, else the
// generic "canceled".
func (e *Executor) canceledStatusText() string {
	if e.cancelReason != nil {
		if reason := e.cancelReason(); reason != "" {
			return reason
		}
	}
	return "canceled"
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

		// Run git hermetically: scrub the git variables that leak in
		// from a surrounding shell or hook so the command can only act
		// on the worktree in dir, pin LC_ALL to C for stable output,
		// and carry the temporary index on top.
		env := scrubGitEnv([]string{"GIT_INDEX_FILE=" + tmp.Name()})
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
		// The diff's output is parsed, so pin the format on the command
		// line: no external diff driver or textconv, no color, and the
		// a/ b/ prefixes the consumer expects.
		out, err := git(
			"-c", "color.ui=never",
			"-c", "diff.noprefix=false",
			"-c", "diff.mnemonicPrefix=false",
			"diff", "--no-ext-diff", "--no-textconv", "--no-color",
			"--src-prefix=a/", "--dst-prefix=b/",
			"--cached", "HEAD",
		).Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

// scrubGitEnv removes the git variables that leak in from a surrounding
// shell or hook so a git command can only act on the repository it is
// told to, and pins LC_ALL to C for stable, locale-independent output.
// extraEnv is appended on top so a temporary GIT_INDEX_FILE survives.
func scrubGitEnv(extraEnv []string) []string {
	env := make([]string, 0, len(os.Environ())+1+len(extraEnv))
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "LC_ALL=C")
	env = append(env, extraEnv...)
	return env
}

// markTurnStarted flips the executor's first-turn flag (#351): it
// reports whether a turn had already started on this route, in which case
// the arriving message is a steer, not the dispatch's own turn. One
// executor serves one dispatch route, so the first message is the served
// dispatch's prompt and everything after it steers. A steer that races
// the first message cannot observe a busy session — the session only
// becomes busy once the first message starts its run — so the ordering
// is safe.
func (e *Executor) markTurnStarted() bool {
	e.turnMu.Lock()
	defer e.turnMu.Unlock()
	started := e.turnStarted
	e.turnStarted = true
	return started
}

// executeSteer delivers one mid-run message to the running agent (#351):
// it enqueues the message through the runner's EnqueueWhenBusy — the same
// delivery primitive the in-process front doors use — and turns the
// steer's own task terminal on the queue's verdict: Completed once the
// message was consumed (folded into the active turn or picked up as the
// follow-up turn), Failed when the queue dropped it without running,
// Rejected when the runner refused it because the run has ended. The
// steer's reply streams back on the dispatch's own surfaces, never on
// this task.
func (e *Executor) executeSteer(ctx context.Context, execCtx *a2asrv.ExecutorContext, binding ContextBinding, prompt string, yield func(a2aspec.Event, error) bool) {
	// ReferenceTasks are advisory (#351): a protocol client may name the
	// running task it is steering — the coordinator's front door does —
	// but the executor accepts a steer that names nothing, or names
	// something else, rather than failing a deliverable message over
	// bookkeeping.
	if len(execCtx.Message.ReferenceTasks) > 0 {
		ids := make([]string, 0, len(execCtx.Message.ReferenceTasks))
		for _, id := range execCtx.Message.ReferenceTasks {
			ids = append(ids, string(id))
		}
		slog.Debug("A2A steer references tasks",
			"context_id", execCtx.ContextID,
			"task_id", string(execCtx.TaskID),
			"reference_tasks", ids)
	}

	// The steer call inherits the run's shaping from the binding's call
	// template with the message as its prompt. RunID and accept state
	// stay empty: a RunID-bearing queued call runs as its own turn with
	// its own lifecycle, while an untracked one folds into the running
	// agent's next input — the injection contract. Steer marks the
	// persisted message as an injection (#410). OnConsumed is the
	// queue's verdict channel (#351); the buffer absorbs a verdict that
	// lands before the wait below starts.
	consumed := make(chan bool, 1)
	call := binding.Call
	call.SessionID = binding.SessionID
	call.Prompt = prompt
	call.Attachments = steerAttachments(execCtx.Message)
	call.RunID = ""
	call.Steer = true
	call.Accepted = nil
	call.OnComplete = nil
	call.OnConsumed = func(ok bool) { consumed <- ok }

	if !binding.Runner.EnqueueWhenBusy(call) {
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateRejected,
			agentMessage(execCtx, "agent is no longer running; task sessions are not continuable")), nil)
		return
	}

	if !yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateWorking, nil), nil) {
		return
	}

	select {
	case <-ctx.Done():
		// The consumer is gone; nothing further can be delivered, and the
		// message stays queued for the agent to consume on its own.
	case ok := <-consumed:
		if ok {
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCompleted,
				agentMessage(execCtx, "delivered")), nil)
		} else {
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed,
				agentMessage(execCtx, "agent finished before the message was consumed")), nil)
		}
	}
}

// steerAttachments decodes a steer message's non-text parts back into
// call attachments (#351): the client encoded the editor's pasted files
// and long pastes as parts, and the steer call runs them through the
// same message pipeline a typed prompt's attachments take. Text parts
// are the prompt itself, never attachments.
func steerAttachments(msg *a2aspec.Message) []message.Attachment {
	if msg == nil {
		return nil
	}
	var out []message.Attachment
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		raw, ok := part.Content.(a2aspec.Raw)
		if !ok || len(raw) == 0 {
			continue
		}
		out = append(out, message.Attachment{
			FileName: part.Filename,
			MimeType: part.MediaType,
			Content:  []byte(raw),
		})
	}
	return out
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
