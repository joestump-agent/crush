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
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
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

// queueClearer is the optional slice of a [Runner] that drops a session's
// queued calls, reporting each one unconsumed to its OnConsumed (#398).
// The executor clears a dispatch's session when its run ends, so the
// steers it never read are reported rather than left waiting. A real
// SessionAgent has it.
type queueClearer interface {
	ClearQueue(sessionID string)
}

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

// QuestionSource is the dispatched agent's own question service (#352):
// the question tool in its toolchain asks through it, the executor
// watches its requests while draining the run and parks the run in
// input-required on one, and an answer on the parked task resolves it.
// A nil source disables questions; the run lifecycle is unchanged.
type QuestionSource interface {
	Subscribe(ctx context.Context) <-chan pubsub.Event[question.Request]
	Answer(answers []question.Answer) bool
}

// Compile-time proof that a question service is a QuestionSource.
var _ QuestionSource = question.Service(nil)

// PermissionSource is the dispatched agent's scoped permission service
// (#353): its tools request approval through it, the executor watches its
// requests while draining the run and parks the run in input-required on
// one, and a decision on the parked task grants or denies it. Yolo and the
// allowlists resolve inside the service, so only requests that need a
// person are ever published. A nil source leaves requests to whoever else
// subscribes; the run lifecycle is unchanged.
type PermissionSource interface {
	Subscribe(ctx context.Context) <-chan pubsub.Event[permission.PermissionRequest]
	Grant(req permission.PermissionRequest) bool
	Deny(req permission.PermissionRequest) bool
}

// Compile-time proof that a permission service is a PermissionSource.
var _ PermissionSource = permission.Service(nil)

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
//
// A dispatched agent can ask the parent's user a question (#352): when
// its question tool asks, the executor ends the execution with an
// input-required status whose message carries the question as a typed
// DataPart, while the run stays blocked in the tool. A message whose
// taskId names that parked task is the answer — not a steer — and
// resumes the same run on the same task. The two are told apart
// explicitly: a message naming a task in input-required is an answer
// (Rejected when nothing is pending on it), and any other message on
// the context goes down the turn-or-steer path above.
//
// A permission request from one of the agent's tools parks the run the
// same way (#353): input-required carries the request as a typed
// DataPart, and the decision on the parked task grants or denies it.
// Parallel tool calls park one request at a time: the scoped service
// publishes the next request only once the parked one is decided.
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
	// questions is the dispatched agent's question service (#352); nil
	// means the agent cannot ask and runs never park.
	questions QuestionSource
	// permissions is the dispatched agent's scoped permission service
	// (#353); nil means no permission request ever parks a run.
	permissions PermissionSource
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
	// onTurn is told the task that starts the dispatch's own turn (#421);
	// nil when nothing tracks it.
	onTurn func(taskID string)
	// runEnded is set once the dispatch's run goroutine has returned
	// (#398): a steer the runner refuses before then is refused as not
	// ready, not as a finished run.
	runEnded atomic.Bool
	// turnMu guards turnStarted (#351): one executor serves one
	// dispatch's route, and every message on its context either starts
	// the dispatch's own turn — the first one — or is a steer.
	turnMu      sync.Mutex
	turnStarted bool
	// steersMu guards steers and undelivered (#398): steers holds every
	// steer accepted into the running session's queue whose fate is not
	// known yet, and undelivered the text of each one the queue dropped
	// unread. The run's terminal status reports undelivered.
	steersMu    sync.Mutex
	steers      map[*steerRecord]struct{}
	undelivered []string
	// runsMu guards runs and live. runs holds the in-flight run records
	// by task ID (#352): a record outlives the execution that drains it,
	// so the executor — not Execute's stack — owns it. live holds every
	// run goroutine that has not returned yet. A record leaves runs when
	// its drain ends or a cancel drops it, while its goroutine may still
	// be unwinding a tool call, so stopRuns joins live, not runs.
	runsMu sync.Mutex
	runs   map[string]*taskRun
	live   map[*taskRun]struct{}
	// hooks are test-only seams; every field is nil in production.
	hooks executorHooks
}

// executorHooks are seams that only tests set, to hold the SDK's
// execution and cancelation goroutines at a chosen point and force an
// interleaving deterministically. Every field is nil in production.
type executorHooks struct {
	// parked runs on the producer's goroutine after the input-required
	// status was yielded and before the execution returns (#352). While
	// it blocks, the SDK keeps the parking execution registered and its
	// event pipe open, even though the consumer has already delivered
	// input-required and canceled the producer's context.
	parked func()
	// canceled runs after Cancel yielded its Canceled status: the SDK has
	// written it to the event pipe the cancel was routed to, or failed
	// to.
	canceled func()
}

// withHooks installs test-only seams (see [executorHooks]).
func withHooks(h executorHooks) Option {
	return func(e *Executor) { e.hooks = h }
}

// runOutcome is what a dispatched run's goroutine hands back: the
// runner's result and error.
type runOutcome struct {
	result *fantasy.AgentResult
	err    error
}

// taskRun is one dispatched run's record (#352): the run goroutine's
// outcome channel, the run's own context and the subscriptions that live
// on it. The run's context is detached from the execution that started
// it and owned here, so whichever execution drains the record reads the
// same run; every way a drain ends without the run's outcome — a cancel,
// a stopped consumer, the inactivity backstop — cancels it.
type taskRun struct {
	taskID  string
	binding ContextBinding
	// cancel ends the run's context: the runner's turn and every
	// subscription the record holds.
	cancel context.CancelFunc
	// done receives the run goroutine's single outcome.
	done chan runOutcome
	// exited is closed once the run goroutine has returned.
	exited chan struct{}
	// todoCh is the run's todo subscription (#174); nil without a todo
	// source.
	todoCh <-chan dispatch.TodoSnapshot
	// lastTodos is the last todo list emitted, so a snapshot that did
	// not change the list stays silent — across a park and resume too.
	lastTodos []session.Todo
	// questionCh is the run's question subscription (#352); nil without
	// a question source.
	questionCh <-chan pubsub.Event[question.Request]
	// pending is the question the run is parked on (#352): non-nil
	// exactly while the task is in input-required and no execution is
	// draining the record. Guarded by the executor's runsMu.
	pending *question.Request
	// permCh is the run's permission subscription (#353); nil without a
	// permission source. The scoped service publishes one request at a
	// time, so the run parks on one request at a time.
	permCh <-chan pubsub.Event[permission.PermissionRequest]
	// pendingPermission is the permission request the run is parked on
	// (#353), like pending for a question. At most one of the two is
	// set. Guarded by the executor's runsMu.
	pendingPermission *permission.PermissionRequest
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

// WithQuestions sets the dispatched agent's question service (#352): a
// request on it parks the run in input-required, and an answer on the
// parked task resumes it. Without it, the agent has no way to ask and
// runs never park.
func WithQuestions(source QuestionSource) Option {
	return func(e *Executor) { e.questions = source }
}

// WithPermissions sets the dispatched agent's scoped permission service
// (#353): a request on it parks the run in input-required, and a decision
// on the parked task grants or denies it.
func WithPermissions(source PermissionSource) Option {
	return func(e *Executor) { e.permissions = source }
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

// withOnTurn sets the hook told which task starts the dispatch's own
// turn (#421): the agent index follows that task, never a steer's.
func withOnTurn(fn func(taskID string)) Option {
	return func(e *Executor) { e.onTurn = fn }
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
// returned error. A question the agent asks ends the execution in
// input-required instead, and a message naming that parked task resumes
// the same run (#352).
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
			yield(statusEvent(execCtx, a2aspec.TaskStateRejected,
				agentMessage(execCtx, noAgentForContextText(execCtx.ContextID))), nil)
			return
		}
		slog.Debug("A2A dispatch turn starting",
			"context_id", execCtx.ContextID,
			"session_id", binding.SessionID,
			"task_id", string(execCtx.TaskID),
			"trace_id", traceID)

		// A message naming a task parked in input-required is the answer
		// to its question (#352), never a turn or a steer: it resumes the
		// parked run on the same task. Checked before the empty-prompt
		// rule — a typed answer carries a DataPart and no text.
		if awaitsAnswer(execCtx) {
			e.executeAnswer(ctx, execCtx, binding, traceID, yield)
			return
		}

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
			yield(statusEvent(execCtx, a2aspec.TaskStateRejected,
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
		// This task is the dispatch's own (#421): say so, so the agent
		// index follows it rather than whichever task came first.
		if e.onTurn != nil {
			e.onTurn(string(execCtx.TaskID))
		}

		if !yield(statusEvent(execCtx, a2aspec.TaskStateWorking, nil), nil) {
			return
		}

		result, err := e.runWithTodos(ctx, execCtx, binding, prompt, yield)
		e.finish(ctx, execCtx, binding, traceID, result, err, yield)
	}
}

// finish turns a drained run's outcome into the task's last events: the
// diff and dispatch-result artifacts and Completed for a finished turn,
// Failed or Canceled for a run that broke or was killed, and nothing at
// all when the run parked on a question (#352) — the input-required
// status already ended the execution — or the consumer is gone.
func (e *Executor) finish(ctx context.Context, execCtx *a2asrv.ExecutorContext, binding ContextBinding, traceID string, result *fantasy.AgentResult, err error, yield func(a2aspec.Event, error) bool) {
	switch {
	case errors.Is(err, errParked):
		// The run waits on its question; the answer's execution
		// drains it from here.
		return
	case errors.Is(err, errConsumerStopped):
		// The consumer stopped consuming mid-run: nothing further can
		// be delivered, and the run's own outcome is dropped with it.
		return
	}

	// The run is over (#398): steers still queued on its session will
	// never be read. Drop them, and name every unread steer on the
	// terminal status this execution yields.
	unread := e.dropUnreadSteers(binding)

	switch {
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
		ev := statusEvent(execCtx, a2aspec.TaskStateCanceled,
			agentMessage(execCtx, e.canceledStatusText()))
		e.attachUsage(ctx, ev, binding.SessionID, traceID)
		attachUndeliveredSteers(ev, unread)
		yield(ev, nil)
		return
	case err != nil:
		ev := statusEvent(execCtx, a2aspec.TaskStateFailed,
			agentMessage(execCtx, err.Error()))
		e.attachUsage(ctx, ev, binding.SessionID, traceID)
		attachUndeliveredSteers(ev, unread)
		yield(ev, nil)
		return
	case result == nil:
		// Run returns (nil, nil) without doing any work when the
		// session is busy (the prompt was silently queued behind the
		// active turn) or a cancel landed during dispatch. No turn ran
		// on behalf of this task, so completing it would misreport;
		// fail it and let the caller retry against an idle session.
		ev := statusEvent(execCtx, a2aspec.TaskStateFailed,
			agentMessage(execCtx, "agent session did not start a turn (busy or canceled)"))
		e.attachUsage(ctx, ev, binding.SessionID, traceID)
		attachUndeliveredSteers(ev, unread)
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

	ev := statusEvent(execCtx, a2aspec.TaskStateCompleted,
		agentMessage(execCtx, result.Response.Content.Text()))
	e.attachUsage(ctx, ev, binding.SessionID, traceID)
	attachUndeliveredSteers(ev, unread)
	yield(ev, nil)
}

// errConsumerStopped reports that the event consumer stopped consuming
// mid-run: no further events can be delivered and the run's outcome is
// dropped with the stream.
var errConsumerStopped = errors.New("a2a: event consumer stopped")

// errParked reports that the drain ended on a question (#352): the run
// is parked in input-required, its record kept for the answer's
// execution, and the execution that drained it is over.
var errParked = errors.New("a2a: run parked on a question")

// awaitsAnswer reports whether the message names a task parked in
// input-required (#352): such a message answers the task's question.
func awaitsAnswer(execCtx *a2asrv.ExecutorContext) bool {
	return execCtx.StoredTask != nil && execCtx.StoredTask.Status.State == a2aspec.TaskStateInputRequired
}

// executeAnswer resumes a run parked on a question (#352) or a
// permission request (#353): the message's answers resolve the agent's
// pending question — a typed answers/v1 DataPart, or the message text as
// a free-text answer — and its permission-decisions/v1 DataPart decides a
// pending request. The task goes back to Working, and the same run record
// drains on to a terminal state or the next pause. A task with nothing
// pending is Rejected: nothing is waiting for an answer, and no turn or
// steer is started.
func (e *Executor) executeAnswer(ctx context.Context, execCtx *a2asrv.ExecutorContext, binding ContextBinding, traceID string, yield func(a2aspec.Event, error) bool) {
	run, parked, ok := e.unpark(string(execCtx.TaskID))
	if !ok {
		yield(statusEvent(execCtx, a2aspec.TaskStateRejected,
			agentMessage(execCtx, "no question or permission request is pending on this task")), nil)
		return
	}
	var resolved bool
	if parked.permission != nil {
		resolved = e.decidePermission(execCtx.Message, *parked.permission)
	} else {
		resolved = e.questions.Answer(questionAnswers(execCtx.Message, *parked.question))
	}
	if !resolved {
		// The question or request was withdrawn before the answer
		// arrived — the tool call ended with its context — and the run
		// moved on; the drain below reports wherever it went.
		slog.Debug("A2A answer arrived after the request was withdrawn",
			"context_id", execCtx.ContextID,
			"task_id", string(execCtx.TaskID),
			"trace_id", traceID)
	}
	if !yield(statusEvent(execCtx, a2aspec.TaskStateWorking, nil), nil) {
		e.releaseRun(run)
		return
	}
	result, err := e.drainRun(ctx, execCtx, run, yield)
	e.finish(ctx, execCtx, binding, traceID, result, err, yield)
}

// park records the question the run is waiting on (#352).
func (e *Executor) park(run *taskRun, req question.Request) {
	e.runsMu.Lock()
	defer e.runsMu.Unlock()
	run.pending = &req
}

// parkPermission records the permission request the run is waiting on
// (#353).
func (e *Executor) parkPermission(run *taskRun, req permission.PermissionRequest) {
	e.runsMu.Lock()
	defer e.runsMu.Unlock()
	run.pendingPermission = &req
}

// parkedOn is what a parked run waits on (#352, #353): exactly one of
// question and permission is set.
type parkedOn struct {
	question   *question.Request
	permission *permission.PermissionRequest
}

// unpark takes what the task's run is parked on (#352, #353), handing the
// record to the answer's execution. ok is false when no record is parked
// under taskID: nothing was ever asked, it was answered already, or a
// cancel took it.
func (e *Executor) unpark(taskID string) (*taskRun, parkedOn, bool) {
	e.runsMu.Lock()
	defer e.runsMu.Unlock()
	run := e.runs[taskID]
	if run == nil || (run.pending == nil && run.pendingPermission == nil) {
		return nil, parkedOn{}, false
	}
	parked := parkedOn{question: run.pending, permission: run.pendingPermission}
	run.pending, run.pendingPermission = nil, nil
	return run, parked, true
}

// decidePermission resolves the permission request a run was parked on
// (#353) with the decision msg carries, and reports whether the request
// was still pending. Only an explicit allow grants it: a deny, or a
// missing or undecodable decision, denies it, so a malformed answer can
// never approve a tool call.
func (e *Executor) decidePermission(msg *a2aspec.Message, req permission.PermissionRequest) bool {
	if permissionAllowed(msg) {
		return e.permissions.Grant(req)
	}
	return e.permissions.Deny(req)
}

// permissionAllowed reports whether msg carries a permission-decisions/v1
// DataPart that allows the request (#353). The message must name the
// extension: a DataPart that merely decodes to {"allow": true} under some
// other extension never grants.
func permissionAllowed(msg *a2aspec.Message) bool {
	if msg == nil || !slices.Contains(msg.Extensions, PermissionDecisionExtensionURI) {
		return false
	}
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		data, ok := part.Content.(a2aspec.Data)
		if !ok {
			continue
		}
		decoded, err := DecodeValue(PermissionDecisionExt, data.Value)
		if err != nil {
			slog.Warn("A2A permission decision failed to decode; denying", "err", err)
			continue
		}
		if decision, ok := decoded.(*agent.PermissionDecision); ok {
			return decision.Allow
		}
	}
	return false
}

// permissionRequiredStatus parks the task on a tool's permission request
// (#353): an input-required status whose message carries a one-line
// summary for a reader and the typed permissions/v1 payload as a
// DataPart, with the extension named on the message.
func permissionRequiredStatus(execCtx *a2asrv.ExecutorContext, req permission.PermissionRequest) *a2aspec.TaskStatusUpdateEvent {
	text := "Permission required: " + req.ToolName
	if req.Description != "" {
		text += ": " + req.Description
	}
	parts := []*a2aspec.Part{a2aspec.NewTextPart(text)}
	prompt := agent.PermissionPrompt{
		ID:          req.ID,
		SessionID:   req.SessionID,
		ToolCallID:  req.ToolCallID,
		ToolName:    req.ToolName,
		Description: req.Description,
		Action:      req.Action,
		Params:      req.Params,
		Path:        req.Path,
	}
	if encoded, err := Encode(PermissionExt, prompt); err != nil {
		// The extension stays named on the message, so the client still
		// knows it is a permission request; without the payload it
		// denies.
		slog.Warn("A2A permission request failed to encode; input-required carries text only", "err", err)
	} else {
		parts = append(parts, a2aspec.NewDataPart(encoded))
	}
	msg := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, parts...)
	msg.Extensions = []string{PermissionExtensionURI}
	return statusEvent(execCtx, a2aspec.TaskStateInputRequired, msg)
}

// inputRequiredStatus parks the task on the agent's question (#352): an
// input-required status whose message carries the question text for a
// reader and the typed questions/v1 payload as a DataPart, with the
// extension named on the message.
func inputRequiredStatus(execCtx *a2asrv.ExecutorContext, req question.Request) *a2aspec.TaskStatusUpdateEvent {
	texts := make([]string, 0, len(req.Questions))
	for _, q := range req.Questions {
		texts = append(texts, q.Text)
	}
	parts := []*a2aspec.Part{a2aspec.NewTextPart(strings.Join(texts, "\n"))}
	if encoded, err := Encode(QuestionExt, agent.QuestionRequest(req)); err != nil {
		// The text still carries the question; only the typed payload is
		// lost, and the client falls back to a free-text question.
		slog.Warn("A2A question failed to encode; input-required carries text only", "err", err)
	} else {
		parts = append(parts, a2aspec.NewDataPart(encoded))
	}
	msg := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, parts...)
	msg.Extensions = []string{QuestionExtensionURI}
	return statusEvent(execCtx, a2aspec.TaskStateInputRequired, msg)
}

// questionAnswers maps an answer message onto the pending question's
// answers (#352): a typed answers/v1 DataPart carrying answers wins;
// otherwise the message text answers every question as free text, and a
// message with neither skips them.
func questionAnswers(msg *a2aspec.Message, req question.Request) []question.Answer {
	if msg != nil {
		for _, part := range msg.Parts {
			if part == nil {
				continue
			}
			data, ok := part.Content.(a2aspec.Data)
			if !ok {
				continue
			}
			decoded, err := DecodeValue(AnswerExt, data.Value)
			if err != nil {
				slog.Warn("A2A answer DataPart failed to decode; falling back to the message text", "err", err)
				continue
			}
			if answer, ok := decoded.(*agent.QuestionAnswer); ok && len(answer.Answers) > 0 {
				return answer.Answers
			}
		}
	}
	text := messageText(msg)
	answers := make([]question.Answer, len(req.Questions))
	for i, q := range req.Questions {
		answers[i] = question.Answer{QuestionID: q.ID, FillInText: text}
	}
	return answers
}

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

// statusEvent builds a status-update event with a UTC timestamp: the SDK
// stamps [a2aspec.NewStatusUpdateEvent] with time.Now() in the server's
// local zone, and the wire format requires ISO 8601 timestamps with a Z
// suffix (the TCK's DM-SERIAL-003). A submitted task carries no timestamp
// and needs no normalization.
func statusEvent(execCtx *a2asrv.ExecutorContext, state a2aspec.TaskState, msg *a2aspec.Message) *a2aspec.TaskStatusUpdateEvent {
	ev := a2aspec.NewStatusUpdateEvent(execCtx, state, msg)
	if ev.Status.Timestamp != nil {
		utc := ev.Status.Timestamp.UTC()
		ev.Status.Timestamp = &utc
	}
	return ev
}

// runWithTodos invokes the binding's runner while streaming the run's todo
// progress (#174): it starts the turn's run record and drains it. The
// turn runs against the resolved binding (#350) — its session, its call
// template, its runner — and the task ID is stamped as the call's RunID,
// so the run's terminal RunComplete event names the A2A task that
// started it.
func (e *Executor) runWithTodos(ctx context.Context, execCtx *a2asrv.ExecutorContext, binding ContextBinding, prompt string, yield func(a2aspec.Event, error) bool) (*fantasy.AgentResult, error) {
	run := e.startRun(ctx, execCtx, binding, prompt)
	return e.drainRun(ctx, execCtx, run, yield)
}

// startRun starts the turn's run on its own goroutine and records it
// under the task ID (#352). The run's context is detached from ctx — the
// execution's — and owned by the record: the SDK cancels an execution's
// context once its consumer has a final event, which must not reach a
// run another execution may still drain. The todo subscription is
// created on the run's context before the run starts, so it is bounded
// by the record: dropped on run end, consumer stop, and cancel.
func (e *Executor) startRun(ctx context.Context, execCtx *a2asrv.ExecutorContext, binding ContextBinding, prompt string) *taskRun {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run := &taskRun{
		taskID:  string(execCtx.TaskID),
		binding: binding,
		cancel:  cancel,
		done:    make(chan runOutcome, 1),
		exited:  make(chan struct{}),
	}
	if e.todos != nil {
		run.todoCh = e.todos.SubscribeSessionTodos(runCtx, binding.SessionID)
	}
	if e.questions != nil {
		// Subscribed before the run starts, so a question the agent asks
		// right away is never published to nobody.
		run.questionCh = e.questions.Subscribe(runCtx)
	}
	if e.permissions != nil {
		// Subscribed before the run starts, for the same reason: a
		// request published to no subscriber would strand its tool call.
		run.permCh = e.permissions.Subscribe(runCtx)
	}
	e.runsMu.Lock()
	if e.runs == nil {
		e.runs = make(map[string]*taskRun)
	}
	e.runs[run.taskID] = run
	if e.live == nil {
		e.live = make(map[*taskRun]struct{})
	}
	e.live[run] = struct{}{}
	e.runsMu.Unlock()

	go func() {
		// Deferred first so it runs last: the run has handed back its
		// outcome, panic or not, before stopRuns can see it exit.
		defer e.runExited(run)
		// A panic in a tool or provider adapter must fail this task, not
		// the whole process (#345): recover it, log it, and hand a run
		// error back so Execute yields its single Failed status. The
		// error uses %v and never wraps, so it cannot be
		// context.Canceled and the canceled branch of Execute cannot
		// swallow it (#342).
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Dispatched run panicked", "session_id", binding.SessionID, "trace_id", traceIDFromContext(runCtx), "panic", r, "stack", string(debug.Stack()))
				e.runEnded.Store(true)
				run.done <- runOutcome{err: fmt.Errorf("dispatched run panicked: %v", r)}
			}
		}()
		call := binding.Call
		call.SessionID = binding.SessionID
		call.Prompt = prompt
		// The task is the run (#350): the RunComplete event echoes the
		// A2A task ID back as RunID, so the task and the run correlate.
		call.RunID = run.taskID
		result, err := binding.Runner.Run(runCtx, call)
		e.runEnded.Store(true)
		run.done <- runOutcome{result, err}
	}()
	return run
}

// runExited drops a returned run goroutine from live and closes its
// exited channel.
func (e *Executor) runExited(run *taskRun) {
	e.runsMu.Lock()
	delete(e.live, run)
	e.runsMu.Unlock()
	close(run.exited)
}

// stopRuns ends the executor's runs for the route's teardown: it cancels
// every run it still records, parked ones included, then waits until
// every run goroutine has returned or ctx ends. A canceled run can still
// be unwinding a tool call in the dispatch's workspace, and the teardown
// after Stop closes the toolchain and releases that workspace. A run that
// starts during the wait is canceled and joined too.
func (e *Executor) stopRuns(ctx context.Context) error {
	e.runsMu.Lock()
	records := e.runs
	e.runs = nil
	e.runsMu.Unlock()
	for _, run := range records {
		run.cancel()
	}
	for {
		e.runsMu.Lock()
		var next *taskRun
		for run := range e.live {
			next = run
			break
		}
		e.runsMu.Unlock()
		if next == nil {
			return nil
		}
		next.cancel()
		select {
		case <-next.exited:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// drainRun drains the run record until the run ends or the drain cannot
// go on, then releases the record: a drain that ends without the run's
// outcome cancels the run with it. A run parked on a question (#352) is
// the exception — its record stays for the answer's execution.
func (e *Executor) drainRun(ctx context.Context, execCtx *a2asrv.ExecutorContext, run *taskRun, yield func(a2aspec.Event, error) bool) (*fantasy.AgentResult, error) {
	result, err := e.drain(ctx, execCtx, run, yield)
	if !errors.Is(err, errParked) {
		e.releaseRun(run)
	}
	return result, err
}

// releaseRun drops the record and cancels the run's context, ending its
// subscriptions — and the run itself, when it is still going.
func (e *Executor) releaseRun(run *taskRun) {
	e.runsMu.Lock()
	if e.runs[run.taskID] == run {
		delete(e.runs, run.taskID)
	}
	e.runsMu.Unlock()
	run.cancel()
}

// cancelRun cancels the run recorded under taskID, if any: the
// executor's Cancel ends the run's own context, not only the runner's
// session, so a run whose runner ignores the session cancel still stops.
// A run parked on a question (#352) or a permission request (#353) has
// no execution draining it, so its record is dropped here; parked reports
// that case. The question's ask ends with the run's context, and the tool
// call returns an error; the permission request is denied.
func (e *Executor) cancelRun(taskID string) (parked bool) {
	e.runsMu.Lock()
	run := e.runs[taskID]
	var denied *permission.PermissionRequest
	if run != nil && (run.pending != nil || run.pendingPermission != nil) {
		parked = true
		denied = run.pendingPermission
		run.pending, run.pendingPermission = nil, nil
		delete(e.runs, taskID)
	}
	e.runsMu.Unlock()
	if denied != nil {
		// A canceled task's parked permission request is denied (#353),
		// so its tool call returns a denial rather than waiting on the
		// context below.
		e.permissions.Deny(*denied)
	}
	if run != nil {
		run.cancel()
	}
	return parked
}

// drain streams the run's progress until it ends (#174): the run
// executes on its own goroutine and the todo subscription is drained
// inline on the iterator's goroutine, so Working progress events and the
// terminal status share one yield path and can never race. A snapshot is
// only emitted when the todo list actually changed, so usage-only
// session saves stay silent. A question the agent asks (#352) ends the
// drain with errParked after the input-required status.
func (e *Executor) drain(ctx context.Context, execCtx *a2asrv.ExecutorContext, run *taskRun, yield func(a2aspec.Event, error) bool) (*fantasy.AgentResult, error) {
	binding := run.binding

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
			// status itself, and the run is aborted with the record's
			// release; reporting anything here would race it.
			return nil, context.Canceled
		case out := <-run.done:
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
			case <-run.done:
			case <-time.After(inactivitySettleWindow):
			}
			return nil, fmt.Errorf("inactivity timeout: no progress for %s", e.inactivityTimeout)
		case snap, ok := <-run.todoCh:
			if !ok {
				run.todoCh = nil
				continue
			}
			if len(snap.Todos) == 0 || slices.Equal(snap.Todos, run.lastTodos) {
				continue
			}
			run.lastTodos = slices.Clone(snap.Todos)
			// Usage rides each progress event too (#421), so a watcher's
			// token count moves while the agent works, not only at the
			// end.
			progress := todoStatusUpdate(execCtx, snap)
			e.attachUsage(ctx, progress, binding.SessionID, traceIDFromContext(ctx))
			if !yield(progress, nil) {
				return nil, errConsumerStopped
			}
			if inactReset != nil {
				inactReset()
			}
		case ev, ok := <-run.questionCh:
			if !ok {
				run.questionCh = nil
				continue
			}
			// A run that already ended does not park on a question its
			// ask has since withdrawn: its outcome is the task's.
			select {
			case out := <-run.done:
				return out.result, out.err
			default:
			}
			// The agent asked (#352): park the run on the question and
			// end this execution with input-required. The run stays
			// blocked in its question tool, on the record's context, and
			// the answer's execution drains it from here. The SDK's
			// inactivity guard is per execution and this backstop is per
			// drain, so a parked run waits on a human without either.
			e.park(run, ev.Payload)
			if !yield(inputRequiredStatus(execCtx, ev.Payload), nil) {
				return nil, errConsumerStopped
			}
			if e.hooks.parked != nil {
				e.hooks.parked()
			}
			return nil, errParked
		case ev, ok := <-run.permCh:
			if !ok {
				run.permCh = nil
				continue
			}
			select {
			case out := <-run.done:
				return out.result, out.err
			default:
			}
			// A tool asked for permission (#353): park the run on the
			// request exactly like a question. The tool call stays
			// blocked in the scoped service until the decision, and the
			// service holds any parallel request back until then.
			e.parkPermission(run, ev.Payload)
			if !yield(permissionRequiredStatus(execCtx, ev.Payload), nil) {
				return nil, errConsumerStopped
			}
			if e.hooks.parked != nil {
				e.hooks.parked()
			}
			return nil, errParked
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
	ev := statusEvent(execCtx, a2aspec.TaskStateWorking, agentMessage(execCtx, text))
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

// attachUsage stamps a status update with the usage/v1 extension's
// payload (#364): the usage closure's reading of the dispatched session's
// totals, plus the request's W3C trace ID. Post-run terminals carry the
// final totals, which the parent is charged; todo progress events carry
// the totals so far (#421), which only a watcher's display uses — the
// client folds usage from terminal statuses alone. The executor's own
// Cancel carries none. Every failure — the closure erroring, or the value
// failing to encode — is logged, and the status ships without metadata:
// usage is accounting, never a reason to fail a run.
func (e *Executor) attachUsage(ctx context.Context, ev *a2aspec.TaskStatusUpdateEvent, sessionID, traceID string) {
	if e.usage == nil {
		return
	}
	// The closure reads the session row; an ended run's context can be
	// on its way out, and the totals are durable regardless.
	usage, err := e.usage(context.WithoutCancel(ctx))
	if err != nil {
		slog.Warn("A2A usage collection failed; status carries no usage metadata", "session_id", sessionID, "trace_id", traceID, "err", err)
		return
	}
	usage.TraceID = traceID
	encoded, err := Encode(UsageExt, usage)
	if err != nil {
		slog.Warn("A2A usage failed to encode; status carries no usage metadata", "session_id", sessionID, "trace_id", traceID, "err", err)
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
// the only terminal one. The runner's session and the task's run record
// (#352) are both canceled. The reason travels the protocol (#348): the
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
			yield(statusEvent(execCtx, a2aspec.TaskStateCanceled,
				agentMessage(execCtx, noAgentForContextText(execCtx.ContextID))), nil)
			return
		}
		e.markOwnCancel(string(execCtx.TaskID))
		binding.Runner.Cancel(binding.SessionID)
		if e.cancelRun(string(execCtx.TaskID)) {
			// A parked run has no drain to consult the own-cancel mark,
			// and the canceled task takes no further message.
			e.forgetCanceledTask(string(execCtx.TaskID))
		}
		text := cancelReasonFromMetadata(execCtx.Metadata)
		if text == "" {
			text = e.canceledStatusText()
		}
		yield(statusEvent(execCtx, a2aspec.TaskStateCanceled, agentMessage(execCtx, text)), nil)
		if e.hooks.canceled != nil {
			e.hooks.canceled()
		}
	}
}

var _ a2asrv.AgentExecutionCleaner = (*Executor)(nil)

// Cleanup implements [a2asrv.AgentExecutionCleaner]: the SDK calls it
// once an execution or a cancelation has resolved. A resolved
// cancelation drops the own-cancel mark its Cancel set (#342). Execute's
// own deferred forget covers a cancel that ended a drained run, and the
// parked branch covers a parked one, but a cancel that found no run
// record at all — the re-issued cancel of a parked task whose first
// cancel already dropped the run (#352) — has neither, and no Execute
// for that task will ever run again. Dropping the mark here is safe: the
// SDK refuses to start an execution while a cancelation is registered,
// and calls Cleanup before unregistering it — after any concurrent
// execution's Execute has returned, since the cancel either waited for
// that execution's result or found its event pipe already closed
// (a2a-go v2.5.0 internal/taskexec/local_manager.go handleCancel and
// handleCancelWithConcurrentRun). An execution (execCtx.Message is
// non-nil) leaves its mark to Execute's own defer.
func (e *Executor) Cleanup(_ context.Context, execCtx *a2asrv.ExecutorContext, _ a2aspec.SendMessageResult, _ error) {
	if execCtx == nil || execCtx.Message != nil {
		return
	}
	e.forgetCanceledTask(string(execCtx.TaskID))
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
	// The steer is tracked until its verdict (#398): one the queue drops
	// unread is reported on the run's terminal status.
	steer := e.trackSteer(prompt)
	call.OnConsumed = func(ok bool) {
		e.settleSteer(steer, ok)
		consumed <- ok
	}

	if !binding.Runner.EnqueueWhenBusy(call) {
		e.untrackSteer(steer)
		yield(e.steerRefusedStatus(execCtx), nil)
		return
	}

	if !yield(statusEvent(execCtx, a2aspec.TaskStateWorking, nil), nil) {
		return
	}

	select {
	case <-ctx.Done():
		// The consumer is gone; nothing further can be delivered, and the
		// message stays queued for the agent to consume on its own.
	case ok := <-consumed:
		if ok {
			yield(statusEvent(execCtx, a2aspec.TaskStateCompleted,
				agentMessage(execCtx, "delivered")), nil)
		} else {
			yield(statusEvent(execCtx, a2aspec.TaskStateFailed,
				agentMessage(execCtx, "agent finished before the message was consumed")), nil)
		}
	}
}

// steerRecord is one accepted steer awaiting its verdict (#398).
type steerRecord struct {
	text string
}

// trackSteer records a steer about to be enqueued (#398).
func (e *Executor) trackSteer(text string) *steerRecord {
	rec := &steerRecord{text: text}
	e.steersMu.Lock()
	defer e.steersMu.Unlock()
	if e.steers == nil {
		e.steers = make(map[*steerRecord]struct{})
	}
	e.steers[rec] = struct{}{}
	return rec
}

// untrackSteer forgets a steer the runner refused: it was never queued.
func (e *Executor) untrackSteer(rec *steerRecord) {
	e.steersMu.Lock()
	defer e.steersMu.Unlock()
	delete(e.steers, rec)
}

// settleSteer records a queued steer's verdict (#398): consumed steers are
// forgotten, and the text of one dropped unread is kept for the run's
// terminal status.
func (e *Executor) settleSteer(rec *steerRecord, consumed bool) {
	e.steersMu.Lock()
	defer e.steersMu.Unlock()
	if _, ok := e.steers[rec]; !ok {
		return
	}
	delete(e.steers, rec)
	if !consumed {
		e.undelivered = append(e.undelivered, rec.text)
	}
}

// dropUnreadSteers ends a finished run's steer bookkeeping (#398). The
// dispatched session never runs again, so whatever is still queued on it
// is dropped — each dropped steer's verdict reports it unread — and the
// text of every steer the agent accepted but never read is returned.
func (e *Executor) dropUnreadSteers(binding ContextBinding) []string {
	if clearer, ok := binding.Runner.(queueClearer); ok {
		clearer.ClearQueue(binding.SessionID)
	}
	e.steersMu.Lock()
	defer e.steersMu.Unlock()
	unread := e.undelivered
	e.undelivered = nil
	return unread
}

// attachUndeliveredSteers puts the steers the agent never read on a
// terminal status under the undelivered-steers/v1 extension (#398).
func attachUndeliveredSteers(ev *a2aspec.TaskStatusUpdateEvent, unread []string) {
	if len(unread) == 0 {
		return
	}
	encoded, err := Encode(UndeliveredSteersExt, agent.UndeliveredSteers{Steers: unread})
	if err != nil {
		slog.Warn("A2A undelivered steers failed to encode; terminal status omits them", "err", err)
		return
	}
	ev.SetMeta(UndeliveredSteersExt.URI, encoded)
}

// steerRefusedStatus is the Rejected status for a steer the runner would
// not enqueue (#398). While the run is still live its session can be
// briefly idle: before the run marks it busy, and around a mid-turn
// context compaction. That refusal says to send the message again and
// carries the not-ready reason under the steer-refusals/v1 extension.
// Once the run has returned, the refusal is final.
func (e *Executor) steerRefusedStatus(execCtx *a2asrv.ExecutorContext) *a2aspec.TaskStatusUpdateEvent {
	if e.runEnded.Load() {
		return statusEvent(execCtx, a2aspec.TaskStateRejected,
			agentMessage(execCtx, "agent is no longer running; task sessions are not continuable"))
	}
	ev := statusEvent(execCtx, a2aspec.TaskStateRejected,
		agentMessage(execCtx, "agent is not ready for messages yet; send the message again in a moment"))
	if encoded, err := Encode(SteerRefusalExt, agent.SteerRefusal{Reason: agent.SteerRefusalNotReady}); err != nil {
		slog.Warn("A2A steer refusal failed to encode", "err", err)
	} else {
		ev.SetMeta(SteerRefusalExt.URI, encoded)
	}
	return ev
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
