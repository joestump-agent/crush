package a2a

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aext"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/question"
)

// Dispatch transport status tokens (#71) — the protocol-side vocabulary
// the a2a package hands the coordinator, mapped there onto
// dispatch.Status. Plain strings because the agent package cannot import
// this one.
const (
	// DispatchStatusCompleted maps TaskStateCompleted.
	DispatchStatusCompleted = "completed"
	// DispatchStatusFailed maps TaskStateFailed and TaskStateRejected.
	DispatchStatusFailed = "failed"
	// DispatchStatusCanceled maps TaskStateCanceled.
	DispatchStatusCanceled = "canceled"
	// DispatchStatusWorking maps every non-terminal task state: the task
	// is still in flight (#349).
	DispatchStatusWorking = "working"
)

// Steer outcome status tokens (#351) — the protocol-side vocabulary the
// a2a package hands the coordinator for one steer delivery; plain
// strings, same reason as the transport tokens above.
const (
	// SteerStatusWorking: the served agent accepted the message into its
	// queue; the reply streams on the dispatch's own surfaces.
	SteerStatusWorking = "working"
	// SteerStatusCompleted: the steer task ran to its own Completed —
	// the queue consumed the message.
	SteerStatusCompleted = "completed"
	// SteerStatusRejected: the served agent refused the message; the run
	// has ended.
	SteerStatusRejected = "rejected"
	// SteerStatusFailed: the message was accepted but never consumed, or
	// the steer task failed.
	SteerStatusFailed = "failed"
)

// dispatchResumeBackoff is the pause before each resubscribe attempt
// (#349): the stream just cut, so a first beat gives a flapping
// connection a moment to settle, and the growth keeps a wedged server
// from being polled into a hole.
var dispatchResumeBackoff = []time.Duration{250 * time.Millisecond, time.Second, 4 * time.Second}

// newDispatchClient builds the A2A client for one dispatch: in-memory
// discovery from the dispatch's registered card, with the JSON-RPC
// transport wired to httpClient — the factory's injected client in
// tests, the factory's unix-socket client in production (#346).
// Every client carries the traceparent interceptor (#364): calls whose
// context holds a W3C traceparent send it as a request header, and calls
// whose context does not (cancels from kill paths, task queries) go out
// without one. The auth interceptor is always attached (#357): it reads
// the host token from the factory's credential store under the
// dispatch's session id and stamps it on every call, as the card's
// security requirements demand. Extra options (the extension-activation
// interceptor) ride along.
func newDispatchClient(ctx context.Context, card *a2aspec.AgentCard, httpClient *http.Client, f *ServerFactory, opts ...a2aclient.FactoryOption) (*a2aclient.Client, error) {
	opts = append([]a2aclient.FactoryOption{
		a2aclient.WithJSONRPCTransport(httpClient),
		a2aclient.WithCallInterceptors(&traceparentInterceptor{}, &a2aclient.AuthInterceptor{Service: f.creds}),
	}, opts...)
	client, err := a2aclient.NewFromCard(ctx, card, opts...)
	if err != nil {
		return nil, fmt.Errorf("a2a: client from card: %w", err)
	}
	return client, nil
}

// StreamDispatch drives one dispatch's initial task over the A2A
// protocol (#71): it builds a client from the dispatch's registered
// AgentCard — in-memory discovery, no directory hop — sends the prompt as
// a streaming message carrying the dispatch's ContextID (#350), and
// consumes the SSE event stream to its terminal state. The returned
// outcome carries the agent's final text (the terminal status message),
// the artifact (the diff), and how many Working progress events were
// observed on the wire.
//
// A stream that ends in input-required (#352) is a question, not an
// end: the typed question goes to the params' OnInputRequired, the
// answer goes back as a message on the same task, and the stream that
// opens consumes on toward the terminal state — or the next question.
// When no answer is coming, the parked task is canceled with the reason.
//
// In-process the agent block keeps rendering from the dispatch
// registry's todo collector — the same snapshots the server re-emits as
// Working TaskStatusUpdateEvents (#174) — so the client counts but does
// not re-publish progress: no parallel progress path. The count proves
// the stream in tests and becomes the progress feed when #72/#73 move
// the agent out of the process.
func (f *ServerFactory) StreamDispatch(ctx context.Context, p agent.DispatchTransportParams) (agent.DispatchTransportOutcome, error) {
	card, ok := p.Card.(*a2aspec.AgentCard)
	if !ok || card == nil {
		return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch %s has no resolvable agent card", p.Endpoint)
	}
	if p.Prompt == "" {
		return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch prompt is empty")
	}
	if p.ContextID == "" {
		return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch context id is empty")
	}
	httpClient := f.httpClient
	if httpClient == nil {
		httpClient = f.dispatchHTTPClient()
	}
	// Activate the extensions the remote agent's card declares and this
	// process registered (#359), and decode the metadata they carry off
	// the stream's status updates. The traceparent interceptor rides on
	// every client this package builds (#364).
	decoder := newMetadataDecoder(card)
	// The dispatch's calls authenticate (#357): the session-scoped
	// bearer token rides every request this client makes.
	ctx = f.dispatchAuthContext(ctx, p.Endpoint)
	client, err := newDispatchClient(ctx, card, httpClient, f, a2aclient.WithCallInterceptors(a2aext.NewActivator(decoder.activatedURIs()...)))
	if err != nil {
		return agent.DispatchTransportOutcome{}, err
	}

	// The A2A context is the task session (#350): the served executor
	// resolves it to the dispatch's runner and session, and the SDK
	// stamps it on the new task, so every event the stream carries is
	// addressable under one context.
	req := &a2aspec.SendMessageRequest{
		Message: a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart(p.Prompt)),
	}
	req.Message.ContextID = p.ContextID

	// One task, one or more streams (#352): each stream runs to a
	// terminal state or to an input-required pause; a pause is answered
	// on the same task, which opens the next stream on the same run.
	s := &dispatchStream{params: p, client: client, decoder: decoder}
	for {
		if err := s.consume(ctx, client.SendStreamingMessage(ctx, req)); err != nil {
			return agent.DispatchTransportOutcome{}, err
		}
		if s.outcome.Status != "" {
			return s.outcome, nil
		}
		parked := s.question
		if parked == nil {
			return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch stream ended without a terminal state")
		}
		s.question = nil
		next, err := s.answer(ctx, *parked)
		if err != nil {
			return agent.DispatchTransportOutcome{}, err
		}
		if next == nil {
			// The parked task was canceled instead of answered; its
			// terminal state is folded.
			return s.outcome, nil
		}
		req = next
	}
}

// dispatchStream is one dispatch's client-side state across its streams
// (#349, #352): the outcome folded so far, the served task's ID once the
// first event named it, and the question the last stream parked on.
type dispatchStream struct {
	params  agent.DispatchTransportParams
	client  *a2aclient.Client
	decoder *metadataDecoder
	outcome agent.DispatchTransportOutcome
	taskID  a2aspec.TaskID
	// question is the question the task is parked on (#352): set by an
	// input-required status or task snapshot, cleared by any later
	// non-terminal state; nil when the stream ended any other way.
	question *agent.QuestionRequest
}

// consume folds one stream's events into the dispatch's state. A stream
// error after a terminal state or a pause is the transport winding down.
// Before one, the task is still live server side — and once the stream
// has named the task (#349), the run is recoverable through resubscribe
// plus tasks/get; without the ID there is nothing to resume and the
// error stands.
func (s *dispatchStream) consume(ctx context.Context, events iter.Seq2[a2aspec.Event, error]) error {
	for ev, err := range events {
		if err != nil {
			if s.outcome.Status != "" || s.question != nil {
				return nil
			}
			if s.taskID == "" {
				return fmt.Errorf("a2a: dispatch stream: %w", err)
			}
			if rerr := s.resume(ctx); rerr != nil {
				return fmt.Errorf("a2a: dispatch stream: %w (resume: %w)", err, rerr)
			}
			return nil
		}
		// The first event names the served task (#349): hand the ID to
		// the caller before anything else, so the registry entry
		// carries it from the first event onward.
		if s.taskID == "" {
			if id := ev.TaskInfo().TaskID; id != "" {
				s.taskID = id
				if s.params.OnTask != nil {
					s.params.OnTask(string(id))
				}
			}
		}
		switch e := ev.(type) {
		case *a2aspec.TaskStatusUpdateEvent:
			applyStatusUpdate(&s.outcome, e)
			s.decoder.apply(&s.outcome, e)
			s.noteQuestion(e.Status)
		case *a2aspec.TaskArtifactUpdateEvent:
			applyArtifactUpdate(&s.outcome, e)
		case *a2aspec.Task:
			foldTaskSnapshot(&s.outcome, e)
			s.noteQuestion(e.Status)
		}
	}
	return nil
}

// noteQuestion tracks whether the task is parked on a question (#352):
// an input-required status parks it with the question it carries, and
// any later non-terminal status means the run moved on.
func (s *dispatchStream) noteQuestion(status a2aspec.TaskStatus) {
	switch {
	case s.outcome.Status != "":
		s.question = nil
	case status.State == a2aspec.TaskStateInputRequired:
		q := questionFromStatus(status.Message)
		s.question = &q
	default:
		s.question = nil
	}
}

// answer gets the parked question answered (#352) and returns the
// message that sends the answer on the same task. Without a handler the
// question gets agent.UnattendedQuestionAnswer. When no answer is
// coming — the handler failed, a kill ended its wait — the parked task
// is canceled with the error's text as the reason, its terminal state
// folded, and the returned request is nil.
func (s *dispatchStream) answer(ctx context.Context, q agent.QuestionRequest) (*a2aspec.SendMessageRequest, error) {
	answer := agent.UnattendedQuestionAnswer(q)
	if s.params.OnInputRequired != nil {
		var err error
		answer, err = s.params.OnInputRequired(ctx, q)
		if err != nil {
			return nil, s.cancelParked(ctx, err.Error())
		}
	}
	encoded, err := Encode(AnswerExt, answer)
	if err != nil {
		return nil, s.cancelParked(ctx, err.Error())
	}
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewDataPart(encoded))
	msg.TaskID = s.taskID
	msg.ContextID = s.params.ContextID
	msg.Extensions = []string{AnswerExtensionURI}
	return &a2aspec.SendMessageRequest{Message: msg}, nil
}

// cancelParked ends a parked task no answer is coming for (#352): a
// tasks/cancel carrying the reason, on a context detached from the
// caller's — the cancel must land even when the caller's context is what
// ended the wait. The cancel usually arrives while the execution that
// parked the task is still on its way out, so it goes through cancelTask,
// which re-issues it once when it lost that race. A task that is already
// terminal (a kill's own tasks/cancel got there first) is read back with
// tasks/get instead. Either way its terminal state folds into the
// outcome.
func (s *dispatchStream) cancelParked(ctx context.Context, reason string) error {
	ctx = context.WithoutCancel(ctx)
	task, err := cancelTask(ctx, s.client, &a2aspec.CancelTaskRequest{
		ID: s.taskID,
		Metadata: map[string]any{
			CancelReasonMetadataKey: CancelReason{Reason: reason},
		},
	})
	if err != nil {
		var gerr error
		task, gerr = s.client.GetTask(ctx, &a2aspec.GetTaskRequest{ID: s.taskID})
		if gerr != nil {
			return fmt.Errorf("a2a: cancel parked task %s: %w (get: %w)", s.taskID, err, gerr)
		}
	}
	foldTaskSnapshot(&s.outcome, task)
	if s.outcome.Status == "" {
		return fmt.Errorf("a2a: parked task %s did not end after its cancel", s.taskID)
	}
	return nil
}

// cancelTask sends one tasks/cancel and re-issues it exactly once when
// the served agent answers TaskNotCancelable (#352). For a task that is
// not terminal, that answer means the cancel lost a race against the end
// of the task's own execution — the race a parked task's cancel walks
// into. The SDK unregisters an execution only after its consumer has
// delivered the final event, input-required, to the client (a2a-go
// v2.5.0 internal/taskexec/local_manager.go handleExecution and
// cleanupExecution), so a client that cancels at once can find the
// execution still registered. Such a cancel is routed into that
// execution's event pipe, where nothing reads the executor's Canceled any
// more, and resolves to the execution's own input-required result
// (handleCancelWithConcurrentRun, promise.go convertToCancelationResult,
// and the SDK's TODO at local_manager.go:391-396). By then the served
// executor has dropped the parked run, yet the task is still
// input-required.
//
// The re-issue is not a timed retry. The racing cancel waits on the
// execution's result, which cleanupExecution signals only after it has
// deleted the execution under the manager's lock, so when the client
// reads TaskNotCancelable no execution is registered: the second cancel
// takes the no-execution path (handleCancel), the executor's Canceled
// carrying the same reason is processed, and the task ends Canceled. The
// served host reports the race's closed-pipe variant as
// TaskNotCancelable too (cancelRaceHandler). A task that really is not
// cancelable — already Completed or Failed — refuses the second cancel
// as well, and that error stands.
func cancelTask(ctx context.Context, client *a2aclient.Client, req *a2aspec.CancelTaskRequest) (*a2aspec.Task, error) {
	task, err := client.CancelTask(ctx, req)
	if errors.Is(err, a2aspec.ErrTaskNotCancelable) {
		task, err = client.CancelTask(ctx, req)
	}
	return task, err
}

// questionFromStatus reads the question off an input-required status
// message (#352): the typed questions/v1 DataPart, or — for a served
// agent that only wrote prose — the message text as one free-text
// question.
func questionFromStatus(msg *a2aspec.Message) agent.QuestionRequest {
	if msg != nil {
		for _, part := range msg.Parts {
			if part == nil {
				continue
			}
			data, ok := part.Content.(a2aspec.Data)
			if !ok {
				continue
			}
			decoded, err := DecodeValue(QuestionExt, data.Value)
			if err != nil {
				slog.Warn("A2A question DataPart failed to decode; asking the status text instead", "err", err)
				continue
			}
			if req, ok := decoded.(*agent.QuestionRequest); ok && len(req.Questions) > 0 {
				return *req
			}
		}
	}
	return agent.QuestionRequest{Questions: []question.Question{{
		ID:          "input",
		Type:        question.TypeFreeText,
		Text:        statusUpdateMessageText(&a2aspec.TaskStatusUpdateEvent{Status: a2aspec.TaskStatus{Message: msg}}),
		Description: "The dispatched agent asked this without a typed question.",
	}}}
}

// resume recovers a dispatch whose SSE stream dropped before a terminal
// state (#349): the task is still running — or already finished, or
// parked on a question (#352) — server side, and resubscribe plus
// tasks/get bring the run home. Up to three attempts, the backoff
// before each: SubscribeToTask replays the stored task snapshot and then
// the live events; when the execution has already ended the server
// answers ErrTaskNotFound and GetTask returns the stored task instead.
// The caller's stream error stands only when every attempt is exhausted
// — after that, the coordinator's cancel-before-teardown (#344) reaps
// the run.
func (s *dispatchStream) resume(ctx context.Context) error {
	var lastErr error
	for _, delay := range dispatchResumeBackoff {
		// A terminal state or a pause from a prior attempt — the stream
		// may have cut right after naming one — already ended the
		// stream.
		if s.ended() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		err := s.consumeResubscription(ctx)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, a2aspec.ErrTaskNotFound):
			// The resubscribe itself failed — the connection is
			// still flapping, or the replay cut again. Try once
			// more after the next backoff.
			lastErr = err
			continue
		}
		// The execution has ended server side: tasks/get returns the
		// stored task with its status and artifacts. A terminal state
		// folds and lands, and so does a question the task is parked on;
		// any other state means the task is live without an execution —
		// retry the subscription instead.
		task, gerr := s.client.GetTask(ctx, &a2aspec.GetTaskRequest{ID: s.taskID})
		if gerr != nil {
			lastErr = gerr
			continue
		}
		foldTaskSnapshot(&s.outcome, task)
		s.noteQuestion(task.Status)
		if s.ended() {
			return nil
		}
	}
	if lastErr != nil {
		return fmt.Errorf("resume attempts exhausted for task %s: %w", s.taskID, lastErr)
	}
	return fmt.Errorf("resume attempts exhausted for task %s without a terminal state", s.taskID)
}

// ended reports whether the stream reached its end: a terminal state, or
// a pause on a question (#352).
func (s *dispatchStream) ended() bool {
	return s.outcome.Status != "" || s.question != nil
}

// consumeResubscription subscribes to the task's event stream and folds
// what arrives into the outcome (#349). The replay opens with the
// stored task snapshot — authoritative for the artifacts received
// before the cut — and then only newer events. Nil only when a
// terminal state or a pause landed; the error wraps
// a2a.ErrTaskNotFound when the server has no active execution for the
// task.
func (s *dispatchStream) consumeResubscription(ctx context.Context) error {
	req := &a2aspec.SubscribeToTaskRequest{ID: s.taskID}
	for ev, err := range s.client.SubscribeToTask(ctx, req) {
		if err != nil {
			return fmt.Errorf("a2a: resubscribe task %s: %w", s.taskID, err)
		}
		s.foldEvent(ev)
	}
	if !s.ended() {
		return fmt.Errorf("a2a: resubscribed stream for task %s ended without a terminal state", s.taskID)
	}
	return nil
}

// foldEvent folds one replay event into the dispatch's state. Both the
// initial stream and a resubscription's replay carry the same event
// vocabulary.
func (s *dispatchStream) foldEvent(ev a2aspec.Event) {
	switch e := ev.(type) {
	case *a2aspec.TaskStatusUpdateEvent:
		applyStatusUpdate(&s.outcome, e)
		s.noteQuestion(e.Status)
	case *a2aspec.TaskArtifactUpdateEvent:
		applyArtifactUpdate(&s.outcome, e)
	case *a2aspec.Task:
		foldTaskSnapshot(&s.outcome, e)
		s.noteQuestion(e.Status)
	}
}

// metadataDecoder decodes declared A2A extension metadata off one stream's
// TaskStatusUpdateEvents (#359): only keys a registered extension owns and
// the remote agent's card declared are decoded — anything else is logged and
// dropped, so unknown or undeclared metadata never fails a dispatch stream.
type metadataDecoder struct {
	// declared is the set of extension URIs on the card's capabilities.
	declared map[string]struct{}
}

// newMetadataDecoder builds the decoder for one dispatch from its card.
func newMetadataDecoder(card *a2aspec.AgentCard) *metadataDecoder {
	declared := make(map[string]struct{}, len(card.Capabilities.Extensions))
	for _, ext := range card.Capabilities.Extensions {
		declared[ext.URI] = struct{}{}
	}
	return &metadataDecoder{declared: declared}
}

// activatedURIs returns the card-declared URIs this process has registered,
// for the extension-activation request the client interceptor sends.
func (d *metadataDecoder) activatedURIs() []string {
	uris := make([]string, 0, len(d.declared))
	for _, ext := range Registered() {
		if _, ok := d.declared[ext.URI]; ok {
			uris = append(uris, ext.URI)
		}
	}
	return uris
}

// apply decodes one status update's extension metadata into the outcome.
// The last decoded value wins per key, mirroring the server's latest-snapshot
// emission order. TodoProgress is the only typed payload consumed today.
func (d *metadataDecoder) apply(outcome *agent.DispatchTransportOutcome, ev *a2aspec.TaskStatusUpdateEvent) {
	meta := ev.Meta()
	if len(meta) == 0 {
		return
	}
	for uri, raw := range meta {
		ext, registered := Lookup(uri)
		if !registered {
			slog.Debug("A2A metadata key is not a registered extension; dropped", "uri", uri)
			continue
		}
		if _, ok := d.declared[uri]; !ok {
			slog.Debug("A2A extension metadata was not declared on the agent card; dropped", "uri", uri)
			continue
		}
		decoded, err := DecodeValue(ext, raw)
		if err != nil {
			slog.Warn("A2A extension metadata failed to decode; dropped", "uri", uri, "err", err)
			continue
		}
		if progress, ok := decoded.(*agent.TodoProgress); ok {
			outcome.TodoProgress = progress
		}
		// Only a terminal status's usage is the run's final total, the
		// one the parent is charged (#364). Progress events carry the
		// totals so far for watchers (#421); a run that ends without a
		// terminal usage — the executor's own Cancel — leaves the
		// parent's cost unchanged rather than charging a partial reading.
		if usage, ok := decoded.(*agent.Usage); ok && ev.Status.State.Terminal() {
			outcome.Usage = usage
		}
	}
}

// traceparentInterceptor sends the parent turn's W3C traceparent as the
// traceparent request header on every A2A call (#364). The value rides
// the request's context — the dispatch turn generates one when the caller
// supplied none — and the server propagator lifts the header back out on
// the other side, so parent call, server logs, and the usage payload's
// trace_id share one trace. A call whose context carries no traceparent
// (resumes, cancels from kill paths) goes out without one.
type traceparentInterceptor struct {
	a2aclient.PassthroughInterceptor
}

// Before implements [a2aclient.CallInterceptor].
func (i *traceparentInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if tp := agent.TraceparentFromContext(ctx); tp != "" {
		req.ServiceParams.Append(traceparentHeader, tp)
	}
	return ctx, nil, nil
}

// applyStatusUpdate folds one status update into the outcome: Working
// counts as progress; terminal states set the outcome's status and text.
func applyStatusUpdate(outcome *agent.DispatchTransportOutcome, ev *a2aspec.TaskStatusUpdateEvent) {
	switch ev.Status.State {
	case a2aspec.TaskStateWorking:
		outcome.WorkingEvents++
	case a2aspec.TaskStateCompleted:
		outcome.Status = DispatchStatusCompleted
		outcome.Text = statusUpdateMessageText(ev)
	case a2aspec.TaskStateFailed, a2aspec.TaskStateRejected:
		outcome.Status = DispatchStatusFailed
		outcome.Text = statusUpdateMessageText(ev)
	case a2aspec.TaskStateCanceled:
		outcome.Status = DispatchStatusCanceled
		outcome.Text = statusUpdateMessageText(ev)
	}
}

// applyTaskSnapshot folds a terminal task snapshot's state and status
// message into the outcome. Call it through foldTaskSnapshot, which
// guards the terminal-wins rule and folds the snapshot's artifacts.
func applyTaskSnapshot(outcome *agent.DispatchTransportOutcome, task *a2aspec.Task) {
	switch task.Status.State {
	case a2aspec.TaskStateCompleted:
		outcome.Status = DispatchStatusCompleted
	case a2aspec.TaskStateFailed, a2aspec.TaskStateRejected:
		outcome.Status = DispatchStatusFailed
	case a2aspec.TaskStateCanceled:
		outcome.Status = DispatchStatusCanceled
	}
	if text := statusUpdateMessageText(&a2aspec.TaskStatusUpdateEvent{Status: task.Status}); text != "" {
		outcome.Text = text
	}
}

// foldTaskSnapshot folds a task snapshot — a resubscription's replay
// opener or a tasks/get answer (#349) — into the outcome. The snapshot
// is the server's authoritative accumulated state, so the
// wire-accumulated artifacts reset before its artifacts fold: an
// already-received partial diff must never double. The replayed
// snapshot itself never counts as progress, and a terminal state folds
// only when the outcome has none — once a terminal state is applied,
// it wins.
func foldTaskSnapshot(outcome *agent.DispatchTransportOutcome, task *a2aspec.Task) {
	if task == nil {
		return
	}
	outcome.Diff = ""
	outcome.DiffError = ""
	outcome.DiffTruncated = false
	for _, art := range task.Artifacts {
		applyArtifact(outcome, art)
	}
	if isTerminalTaskState(task.Status.State) && outcome.Status == "" {
		applyTaskSnapshot(outcome, task)
	}
}

// applyArtifactUpdate folds one artifact update event into the outcome
// (#361): the named diff artifact reassembles by artifact ID — capped
// at maxReassembledDiffBytes, anything past the cap marks the outcome
// truncated — and the dispatch-result artifact decodes into the
// outcome's typed fields. Unrecognized artifacts are ignored.
func applyArtifactUpdate(outcome *agent.DispatchTransportOutcome, ev *a2aspec.TaskArtifactUpdateEvent) {
	if ev == nil || ev.Artifact == nil {
		return
	}
	applyArtifact(outcome, ev.Artifact)
}

// applyArtifact folds one artifact — an update event's payload or one
// of a task snapshot's stored artifacts — into the outcome.
func applyArtifact(outcome *agent.DispatchTransportOutcome, art *a2aspec.Artifact) {
	if art == nil {
		return
	}
	switch art.ID {
	case DiffArtifactID:
		for _, part := range art.Parts {
			if part == nil {
				continue
			}
			text := part.Text()
			if len(outcome.Diff)+len(text) > maxReassembledDiffBytes {
				outcome.DiffTruncated = true
				break
			}
			outcome.Diff += text
		}
	case ResultArtifactID:
		for _, part := range art.Parts {
			if decoded, ok := decodeDispatchOutcome(part); ok {
				outcome.DiffError = decoded.DiffError
			}
		}
	}
}

// isTerminalTaskState reports whether the state ends the task.
func isTerminalTaskState(state a2aspec.TaskState) bool {
	switch state {
	case a2aspec.TaskStateCompleted, a2aspec.TaskStateFailed, a2aspec.TaskStateCanceled, a2aspec.TaskStateRejected:
		return true
	default:
		return false
	}
}

// statusMessageText extracts the text of a status update's message.
func statusUpdateMessageText(ev *a2aspec.TaskStatusUpdateEvent) string {
	if ev == nil || ev.Status.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range ev.Status.Message.Parts {
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

// dispatchAuthContext stamps the session id the dispatch client's auth
// interceptor looks up and stores this host's token under it (#357):
// the SDK's AuthInterceptor requires an [a2aclient.SessionID] on the
// context and fetches the credential per (session, scheme) pair. The
// endpoint doubles as the session id — unique per dispatch, stable
// across resume and cancel.
func (f *ServerFactory) dispatchAuthContext(ctx context.Context, endpoint string) context.Context {
	sid := a2aclient.SessionID(endpoint)
	f.creds.Set(sid, bearerSchemeName, a2aclient.AuthCredential(f.authToken()))
	return a2aclient.AttachSessionID(ctx, sid)
}

// dispatchHTTPClient builds the HTTP client dispatch streams dial
// through: its transport maps every dial onto the factory's unix socket,
// ignoring the resolved address, so the card's http://crush-a2a endpoint
// is a routing label rather than a dial target (#346). There is
// deliberately no proxy (ProxyFromEnvironment would route an http:// URL
// at an HTTP proxy, breaking the unix dial) and no overall timeout: the
// SDK's own 3-minute total timeout is dropped, matching the per-phase
// bounds the dispatch client carries.
func (f *ServerFactory) dispatchHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: 10 * time.Second}
				return dialer.DialContext(ctx, "unix", f.socketPath())
			},
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		},
	}
}

// dispatchTaskStateToken maps an A2A task state onto the transport's
// status vocabulary: the terminal tokens are the outcome's, everything
// non-terminal reports as working (#349).
func dispatchTaskStateToken(state a2aspec.TaskState) string {
	switch state {
	case a2aspec.TaskStateCompleted:
		return DispatchStatusCompleted
	case a2aspec.TaskStateFailed, a2aspec.TaskStateRejected:
		return DispatchStatusFailed
	case a2aspec.TaskStateCanceled:
		return DispatchStatusCanceled
	default:
		return DispatchStatusWorking
	}
}

// GetDispatchTask answers one task query over the A2A protocol (#349):
// tasks/get returns the stored task, its status message while the task
// is still in flight and its terminal status, text, and artifacts once
// it has finished. It serves surfaces that meet a dispatched run
// without a live stream (#348, #421). Card is the registry entry's
// opaque AgentCard; TaskID is the ID StreamDispatch reported through
// the params' OnTask.
func (f *ServerFactory) GetDispatchTask(ctx context.Context, p agent.GetDispatchTaskParams) (agent.DispatchTaskStatus, error) {
	card, ok := p.Card.(*a2aspec.AgentCard)
	if !ok || card == nil {
		return agent.DispatchTaskStatus{}, fmt.Errorf("a2a: dispatch %s has no resolvable agent card", p.Endpoint)
	}
	if p.TaskID == "" {
		return agent.DispatchTaskStatus{}, fmt.Errorf("a2a: dispatch %s has no task id", p.Endpoint)
	}
	httpClient := f.httpClient
	if httpClient == nil {
		httpClient = f.dispatchHTTPClient()
	}
	ctx = f.dispatchAuthContext(ctx, p.Endpoint)
	client, err := newDispatchClient(ctx, card, httpClient, f)
	if err != nil {
		return agent.DispatchTaskStatus{}, err
	}
	task, err := client.GetTask(ctx, &a2aspec.GetTaskRequest{ID: a2aspec.TaskID(p.TaskID)})
	if err != nil {
		return agent.DispatchTaskStatus{}, fmt.Errorf("a2a: get task %s: %w", p.TaskID, err)
	}
	return agent.DispatchTaskStatus{
		Status: dispatchTaskStateToken(task.Status.State),
		Text:   statusUpdateMessageText(&a2aspec.TaskStatusUpdateEvent{Status: task.Status}),
	}, nil
}

// CancelReasonMetadataKey is the metadata key a tasks/cancel request
// carries its reason under (#348). It rides CancelTaskRequest.Metadata,
// which the server copies onto the cancel's ExecutorContext, and the
// executor puts the decoded reason on the terminal Canceled status
// message. Declared as a single, crush-owned extension key; #359's
// declared, statically typed metadata will adopt it.
const CancelReasonMetadataKey = "crush.dispatch.cancel_reason"

// CancelReason is the typed metadata payload a tasks/cancel request
// carries its kill reason under (#348), keyed by
// CancelReasonMetadataKey.
type CancelReason struct {
	Reason string `json:"reason"`
}

// cancelReasonFromMetadata decodes the kill reason a cancel request
// carried (#348). The wire round-trips the typed payload through JSON,
// so a served executor sees it as a nested map; accept the round-tripped
// form, a direct payload from an in-process caller, and a bare string.
func cancelReasonFromMetadata(md map[string]any) string {
	if md == nil {
		return ""
	}
	switch v := md[CancelReasonMetadataKey].(type) {
	case string:
		return v
	case CancelReason:
		return v.Reason
	case *CancelReason:
		if v != nil {
			return v.Reason
		}
	case map[string]any:
		if s, ok := v["reason"].(string); ok {
			return s
		}
	}
	return ""
}

// CancelDispatch sends one tasks/cancel for a served dispatch (#348):
// the protocol-native kill the direct SessionAgent cancel can never be
// for an out-of-process agent (#72/#73). The reason rides the request as
// declared metadata and lands on the terminal Canceled status message
// the dispatch's stream, or any tasks/get reader, reports. A kill that
// lands as the task parks on a question meets the same race as a parked
// task's own cancel (#352), so it goes through cancelTask too. An error
// here means the cancel did not land — the caller falls back to the
// direct cancel.
func (f *ServerFactory) CancelDispatch(ctx context.Context, p agent.DispatchCancelParams) error {
	card, ok := p.Card.(*a2aspec.AgentCard)
	if !ok || card == nil {
		return fmt.Errorf("a2a: dispatch %s has no resolvable agent card", p.Endpoint)
	}
	httpClient := f.httpClient
	if httpClient == nil {
		httpClient = f.dispatchHTTPClient()
	}
	ctx = f.dispatchAuthContext(ctx, p.Endpoint)
	client, err := newDispatchClient(ctx, card, httpClient, f)
	if err != nil {
		return err
	}
	return cancelDispatch(ctx, client, p)
}

// cancelDispatch sends the cancel over client: the host's own socket
// client, or the server proxy's (#421).
func cancelDispatch(ctx context.Context, client *a2aclient.Client, p agent.DispatchCancelParams) error {
	if p.TaskID == "" {
		return fmt.Errorf("a2a: dispatch %s has no task id", p.Endpoint)
	}
	req := &a2aspec.CancelTaskRequest{
		ID: a2aspec.TaskID(p.TaskID),
		Metadata: map[string]any{
			CancelReasonMetadataKey: CancelReason{Reason: p.Reason},
		},
	}
	if _, err := cancelTask(ctx, client, req); err != nil {
		return fmt.Errorf("a2a: cancel task %s: %w", p.TaskID, err)
	}
	return nil
}

// Compile-time proof the factory satisfies the dispatch host and the
// cancel seam.
var (
	_ agent.DispatchHost     = (*ServerFactory)(nil)
	_ agent.DispatchCanceler = (*ServerFactory)(nil)
	_ agent.DispatchSteerer  = (*ServerFactory)(nil)
)

// SteerDispatch delivers one mid-run steer to a served dispatch (#351):
// the protocol-native form of the in-process injection — a message on
// the dispatch's running context — so an out-of-process agent is steered
// exactly like an in-process one. It returns once the served agent
// accepted the message into its queue (the stream's Working event) or
// refused it (Rejected), and a goroutine drains the steer task's own
// tail to its terminal state, logging a Failed outcome: the steer's
// reply streams back on the dispatch's own surfaces, not on this call.
func (f *ServerFactory) SteerDispatch(ctx context.Context, p agent.DispatchSteerParams) (agent.DispatchSteerOutcome, error) {
	card, ok := p.Card.(*a2aspec.AgentCard)
	if !ok || card == nil {
		return agent.DispatchSteerOutcome{}, fmt.Errorf("a2a: dispatch %s has no resolvable agent card", p.Endpoint)
	}
	httpClient := f.httpClient
	if httpClient == nil {
		httpClient = f.dispatchHTTPClient()
	}
	ctx = f.dispatchAuthContext(ctx, p.Endpoint)
	client, err := newDispatchClient(ctx, card, httpClient, f)
	if err != nil {
		return agent.DispatchSteerOutcome{}, err
	}
	return steerDispatch(ctx, client, p)
}

// steerDispatch delivers the steer over client: the host's own socket
// client, or the server proxy's (#421).
func steerDispatch(ctx context.Context, client *a2aclient.Client, p agent.DispatchSteerParams) (agent.DispatchSteerOutcome, error) {
	if p.ContextID == "" {
		return agent.DispatchSteerOutcome{}, fmt.Errorf("a2a: dispatch %s has no context id", p.Endpoint)
	}
	if p.Text == "" {
		return agent.DispatchSteerOutcome{}, fmt.Errorf("a2a: steer text is empty")
	}
	req := &a2aspec.SendMessageRequest{
		Message: a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart(p.Text)),
	}
	// The A2A context is the task session (#350): the served executor
	// resolves it to the running agent and enqueues the message.
	req.Message.ContextID = p.ContextID
	for _, id := range p.ReferenceTaskIDs {
		req.Message.ReferenceTasks = append(req.Message.ReferenceTasks, a2aspec.TaskID(id))
	}
	// Attachments ride along as raw parts (#351): the executor decodes
	// them back into the steer call's attachments, so the editor's
	// pasted files and long pastes steer an out-of-process agent the
	// same way they steer an in-process one.
	for _, att := range p.Attachments {
		if len(att.Content) == 0 {
			continue
		}
		req.Message.Parts = append(req.Message.Parts, &a2aspec.Part{
			Content:   a2aspec.Raw(att.Content),
			Filename:  att.FileName,
			MediaType: att.MimeType,
		})
	}

	// terminalSteerStatus maps a steer task's terminal state onto the
	// steer outcome's vocabulary; ok is false for non-terminal states.
	terminalSteerStatus := func(state a2aspec.TaskState) (string, bool) {
		switch state {
		case a2aspec.TaskStateCompleted:
			return SteerStatusCompleted, true
		case a2aspec.TaskStateFailed:
			return SteerStatusFailed, true
		case a2aspec.TaskStateRejected:
			return SteerStatusRejected, true
		case a2aspec.TaskStateCanceled:
			// The run was killed while the steer was in flight: the
			// message will never be consumed.
			return SteerStatusFailed, true
		default:
			return "", false
		}
	}

	var outcome agent.DispatchSteerOutcome
	accepted := false
	var steerTaskID a2aspec.TaskID
	for ev, err := range client.SendStreamingMessage(ctx, req) {
		if err != nil {
			// A stream error after the steer was accepted is the tail
			// drain's problem — the delivery itself succeeded. Before
			// that, the steer never reached the agent and the error
			// stands.
			if accepted {
				slog.Warn("A2A steer stream error after delivery", "endpoint", p.Endpoint, "context_id", p.ContextID, "error", err)
				return outcome, nil
			}
			return agent.DispatchSteerOutcome{}, fmt.Errorf("a2a: steer stream: %w", err)
		}
		if steerTaskID == "" {
			if id := ev.TaskInfo().TaskID; id != "" {
				steerTaskID = id
			}
		}
		sue, isStatus := ev.(*a2aspec.TaskStatusUpdateEvent)
		if !isStatus {
			continue
		}
		if sue.Status.State == a2aspec.TaskStateWorking {
			if accepted {
				continue
			}
			// Accepted into the queue: report success and hand the tail
			// to a drainer. The steer task's own terminal state is
			// bookkeeping — the reply arrives on the dispatch's own
			// surfaces — so a Failed tail is logged, not surfaced. The
			// drainer resumes the steer task's stream by its ID
			// (SubscribeToTask): this call's own stream winds down with
			// the return, and the drainer outlives it on a detached
			// context.
			accepted = true
			outcome.Status = SteerStatusWorking
			if steerTaskID != "" {
				drainCtx := context.WithoutCancel(ctx)
				taskID := string(steerTaskID)
				go func() {
					for ev, err := range client.SubscribeToTask(drainCtx, &a2aspec.SubscribeToTaskRequest{ID: a2aspec.TaskID(taskID)}) {
						if err != nil {
							slog.Warn("A2A steer tail drain error", "endpoint", p.Endpoint, "context_id", p.ContextID, "task_id", taskID, "error", err)
							return
						}
						sue, ok := ev.(*a2aspec.TaskStatusUpdateEvent)
						if !ok {
							continue
						}
						status, terminal := terminalSteerStatus(sue.Status.State)
						if !terminal {
							continue
						}
						if status != SteerStatusCompleted {
							slog.Warn("A2A steer did not complete after delivery",
								"endpoint", p.Endpoint,
								"context_id", p.ContextID,
								"task_id", taskID,
								"status", status,
								"reason", statusUpdateMessageText(sue))
						}
						return
					}
				}()
			}
			return outcome, nil
		}
		status, terminal := terminalSteerStatus(sue.Status.State)
		if !terminal {
			continue
		}
		outcome.Status = status
		if sue.Status.Message != nil {
			outcome.Text = statusUpdateMessageText(sue)
		}
		return outcome, nil
	}
	if !accepted && outcome.Status == "" {
		return agent.DispatchSteerOutcome{}, fmt.Errorf("a2a: steer stream ended without a status")
	}
	return outcome, nil
}
