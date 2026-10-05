package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/skills"
)

// DispatchServerStarter starts an in-process A2A server for one
// dispatched agent (#70). It is the seam that keeps the dependency
// direction one-way — internal/a2a imports internal/agent for the
// Executor's runner, so the agent package only ever sees this
// interface. The production implementation is a2a.ServerFactory; tests
// substitute fakes through it, and a nil starter (the default until the
// app wires the factory) simply serves nothing.
type DispatchServerStarter interface {
	// StartDispatchServer stands up the loopback A2A server for one
	// dispatch and returns its endpoint, the AgentCard to stamp on the
	// registry entry (opaque here), and the stop function the dispatch
	// run defers.
	StartDispatchServer(ctx context.Context, params DispatchServerParams) (endpoint string, card any, stop func(), err error)
}

// DispatchServerParams is one dispatch's slice of the A2A server (#70):
// what to serve, on which session, and where its card's identity comes
// from. Todos is spelled structurally so the agent package stays
// import-clean of internal/a2a; the production value is the dispatch
// registry's todo collector.
type DispatchServerParams struct {
	// SessionID is the ephemeral task session backing the dispatch.
	SessionID string
	// Runner is the dispatched agent itself.
	Runner SessionAgent
	// Diff collects the completion artifact (the workspace diff).
	Diff func(ctx context.Context) (string, error)
	// Todos streams per-session progress snapshots for
	// TaskStatusUpdateEvents (#174).
	Todos interface {
		SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot
	}
	// Name is the card's agent name — the dispatch's assigned handle.
	Name string
	// Description is the card's one-line description — the dispatch's
	// role.
	Description string
	// Skills are the skills the dispatch was given.
	Skills []*skills.Skill
	// Call is the template every served turn runs with (#71): the
	// dispatch's full call shaping — model options, token budget,
	// content width, NonInteractive — with the prompt overridden per
	// message. Without it the server's executor runs a minimal call,
	// which is enough for tests, not for a production dispatched turn.
	Call SessionAgentCall
	// InactivityTimeout is the A2A-level backstop (#360): a served run
	// that yields no events for this long is ended with a Failed status
	// carrying the reason. 0 disables it.
	InactivityTimeout time.Duration
	// CancelReason reports why the dispatched run was killed (#316), for
	// the Canceled status an out-of-band cancel emits (#342). Nil-safe:
	// the production value is the run's kill reason, and a nil func or
	// an empty string falls back to "canceled".
	CancelReason func() string
}

// DispatchTransport drives one dispatch's initial task over the A2A
// protocol (#71): prompt out as a streaming message, the SSE event
// stream back to its terminal state. Implemented by the same a2a factory
// that starts the servers, so one wired object serves both halves of the
// protocol boundary; the coordinator falls back to the direct in-process
// run when no transport is wired or the dispatch is not served.
type DispatchTransport interface {
	// StreamDispatch sends the dispatch prompt to the served dispatch and
	// returns its terminal outcome.
	StreamDispatch(ctx context.Context, params DispatchTransportParams) (DispatchTransportOutcome, error)
}

// DispatchTransportParams is one dispatch's slice of the A2A client
// (#71): where to send it and what to say. Card is the registry entry's
// opaque AgentCard — the transport owns its concrete type.
type DispatchTransportParams struct {
	Endpoint string
	Card     any
	Prompt   string
	// OnTask, when set, receives the served task's ID once the stream's
	// first event names it (#349): the task outlives a dropped stream,
	// and with the ID the coordinator can stamp the registry entry so
	// the run stays recoverable (tasks/resubscribe, tasks/get) and
	// answerable after the fact. Called at most once. Nil-safe.
	OnTask func(taskID string)
}

// GetDispatchTaskParams is one A2A task query (#349): the served
// dispatch's endpoint and card, plus the task ID the transport reported
// through DispatchTransportParams.OnTask. Card is the registry entry's
// opaque AgentCard — the transport owns its concrete type.
type GetDispatchTaskParams struct {
	Endpoint string
	Card     any
	TaskID   string
}

// DispatchCancelParams is one A2A task cancel (#348): the served
// dispatch's endpoint, card, and task ID, plus the reason the run is
// being killed. The reason rides the request as declared metadata and
// comes back on the terminal Canceled status message. Card is the
// registry entry's opaque AgentCard — the transport owns its concrete
// type.
type DispatchCancelParams struct {
	Endpoint string
	Card     any
	TaskID   string
	Reason   string
}

// DispatchCanceler is the cancel half of the A2A client seam (#348):
// route one dispatched run's kill through the protocol's tasks/cancel,
// carrying the kill reason, instead of the in-process SessionAgent
// cancel an out-of-process agent (#72/#73) could never see. Implemented
// by the same a2a factory that streams and serves the dispatch;
// coordinators assert to it and fall back to the direct cancel when the
// wired starter does not implement it.
type DispatchCanceler interface {
	// CancelDispatch sends one tasks/cancel for the served dispatch.
	// It returns once the server accepted the cancel; the run's actual
	// termination is observed on the stream, not here.
	CancelDispatch(ctx context.Context, params DispatchCancelParams) error
}

// DispatchTaskStatus is the observed state of one dispatched task
// (#349). Status is "working" while the task is still in flight and
// otherwise the terminal outcome vocabulary — "completed", "failed",
// "canceled", the same tokens DispatchTransportOutcome.Status carries.
// Text is the task status' message when one arrived.
type DispatchTaskStatus struct {
	Status string
	Text   string
}

// DispatchTransportOutcome is the terminal outcome of one A2A-driven
// dispatch, in transport vocabulary; the coordinator maps it onto the
// DispatchResult. Status is one of "completed", "failed", "canceled".
// Text is the agent's final message (findings, or the failure reason).
// Diff is the reassembled diff artifact when one arrived. DiffError is
// the capture error the remote agent put on the wire (#361) — it
// replaces the stage-1 in-process re-diff. DiffTruncated marks a
// reassembled diff that was cut at the client's byte cap. WorkingEvents
// counts the non-terminal progress events observed on the wire —
// consumed, not re-published: in-process the agent block renders from
// the todo collector, and this count is the seam #72/#73 pick up.
// TodoProgress carries the typed todo progress decoded from the
// declared todos/v1 extension metadata, when the remote agent declared
// and emitted it.
type DispatchTransportOutcome struct {
	Status        string
	Text          string
	Diff          string
	DiffError     string
	DiffTruncated bool
	WorkingEvents int
	// TodoProgress is the decoded todo progress from the dispatch's
	// TaskStatusUpdateEvent metadata (extension
	// https://crush.charm.land/ext/todos/v1), from the last progress
	// event that carried it. Nil when the agent did not declare the
	// extension, never emitted it, or emitted a value that failed to
	// decode — a malformed extension value is logged and dropped, not a
	// stream failure.
	TodoProgress *TodoProgress
}

// TodoItem is one entry of the todos/v1 extension's todo list, the
// transport-vocabulary mirror of session.Todo (json tag names kept
// identical) so the wire shape stays stable as the session type evolves.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form"`
}

// TodoProgress is the statically typed payload the todos/v1 extension
// carries in TaskStatusUpdateEvent metadata: the in-progress todo, an
// N/M completed count, and the full structured todo list, so a consumer
// renders a checklist without parsing the message prose.
type TodoProgress struct {
	// Current is the in-progress todo's active form when set, its
	// content otherwise. Empty when no todo is in progress.
	Current string `json:"current"`
	// Completed and Total count the todos.
	Completed int `json:"completed"`
	Total     int `json:"total"`
	// Todos is the full structured todo list.
	Todos []TodoItem `json:"todos"`
}

// Transport status tokens (#71), spelled identically to the a2a
// package's — the mapping in dispatchNaturalOutcomeFromTransport keys on them.
const (
	transportStatusCompleted = "completed"
	transportStatusFailed    = "failed"
	transportStatusCanceled  = "canceled"
)

// dispatchNaturalOutcomeFromTransport maps an A2A transport outcome onto
// the path-neutral natural outcome (#343), mirroring the direct path's
// semantics: completed carries its findings and diff, failed and
// canceled become a run error carrying the transport's text — parity
// with the direct path, where a canceled run records failed; StatusKilled
// is reserved for wander kill (#316) — the in-process kill state is the
// first witness, and a terminal Canceled whose status message is a kill
// reason (#348, a kill this process never recorded) maps through the
// same kill assembly. A loop stop arrives as the kill
// state's tool-loop reason, recorded in-process by the served agent's
// observer. The diff comes from the wire only (#361): a capture error
// arrives as the outcome's DiffError and maps onto the same "(diff
// unavailable: ...)" summary the direct path produces, an arrived diff
// is used as-is, and nothing on the wire means "(no changes)" — there is
// no in-process re-diff.
func dispatchNaturalOutcomeFromTransport(outcome DispatchTransportOutcome) dispatchNaturalOutcome {
	natural := dispatchNaturalOutcome{}
	switch {
	case outcome.DiffError != "":
		natural.diff = func(context.Context) (string, error) {
			return "", errors.New(outcome.DiffError)
		}
	case outcome.Diff != "":
		natural.diff = func(context.Context) (string, error) {
			return outcome.Diff, nil
		}
	default:
		natural.diff = func(context.Context) (string, error) {
			return "", nil
		}
	}
	switch outcome.Status {
	case transportStatusCompleted:
		natural.completed = true
		natural.findings = outcome.Text
	case transportStatusCanceled:
		if isKillReason(outcome.Text) {
			natural.killReason = outcome.Text
		}
		natural.runErr = fmt.Errorf("dispatch canceled: %s", cmp.Or(outcome.Text, "no reason given"))
	default:
		natural.runErr = errors.New(cmp.Or(outcome.Text, "dispatch failed without a reason"))
	}
	return natural
}

// isKillReason reports whether text is one of the kill reasons
// (#316) a terminal Canceled status can carry over the wire (#348).
func isKillReason(text string) bool {
	switch text {
	case dispatch.ReasonIgnoredNudges,
		dispatch.ReasonStalledTodos,
		dispatch.ReasonToolLoop,
		dispatch.ReasonHardTimeout,
		dispatch.ReasonCanceled,
		dispatch.ReasonShutdown:
		return true
	}
	return false
}

// runDispatchOverTransport drives one dispatch through the A2A client
// (#71): the served endpoint is read from the registry entry the server
// stamped, the prompt goes out as a streaming message, and the SSE
// stream runs to its terminal state. Returns (nil, nil) when this
// dispatch is not transport-driven — no endpoint, no card, or no wired
// transport — so the caller falls back to the direct in-process run.
// The outcome maps through the same terminal assembly as the direct
// path (#343): the kill state decides killed-vs-natural on both.
func (c *coordinator) runDispatchOverTransport(ctx context.Context, run dispatchRun) (dispatch.DispatchResult, bool) {
	transport, ok := c.dispatchServerStarter().(DispatchTransport)
	if !ok || transport == nil {
		return dispatch.DispatchResult{}, false
	}
	entry, ok := run.reg.Get(run.entry.ID)
	if !ok || entry.Endpoint == "" || entry.AgentCard == nil {
		return dispatch.DispatchResult{}, false
	}
	outcome, err := transport.StreamDispatch(ctx, DispatchTransportParams{
		Endpoint: entry.Endpoint,
		Card:     entry.AgentCard,
		Prompt:   run.prompt,
		// The served task outlives a dropped stream (#349): stamp the
		// task ID the moment the stream names it, so the run stays
		// recoverable and answerable through the registry.
		OnTask: func(taskID string) {
			run.reg.SetTaskID(run.entry.ID, taskID)
		},
	})
	if err != nil {
		slog.Error("Dispatch A2A stream failed", "dispatch_id", run.entry.ID, "session_id", run.sessionID, "error", err)
		// The served task runs on a detached context (#344): a stream
		// error before a terminal state leaves the agent running
		// unsupervised with its result headed for the trash. Cancel it
		// now, before the run's teardown closes the toolchain and the
		// permission bridge, and wait — bounded — for the run to end so
		// teardown never orphans a live agent.
		c.cancelOrphanedDispatchRun(ctx, run)
		outcome = DispatchTransportOutcome{
			Status: transportStatusFailed,
			Text:   err.Error(),
		}
	}
	// Kill the run's background jobs before terminal assembly, so the
	// in-process salvage-diff fallback cannot race a job still writing
	// the workspace (#385).
	c.killDispatchSessionJobs(ctx, run)
	return c.assembleTerminalDispatchResult(ctx, run, dispatchNaturalOutcomeFromTransport(outcome)), true
}

// cancelOrphanedDispatchRun cancels a dispatched agent whose transport
// stream failed before a terminal state (#344) and waits, bounded at
// ten seconds by polling IsSessionBusy, for the run to actually end:
// the served task runs on context.WithoutCancel, so without the
// explicit Cancel the agent keeps burning tokens after the stream is
// gone and nothing would ever reap it.
func (c *coordinator) cancelOrphanedDispatchRun(ctx context.Context, run dispatchRun) {
	if run.agent == nil {
		return
	}
	run.agent.Cancel(run.sessionID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if !run.agent.IsSessionBusy(run.sessionID) {
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("Dispatched agent still busy after stream-error cancel", "dispatch_id", run.entry.ID, "session_id", run.sessionID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// SetDispatchServerStarter wires the A2A server factory (#70). Call once
// at app construction, before the first dispatch; nil disables serving
// (tests, or a build without the factory wired). When the same object
// also implements [DispatchTransport] — the production factory does —
// served dispatches are driven over the protocol (#71).
func (c *coordinator) SetDispatchServerStarter(starter DispatchServerStarter) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	c.dispatchServer = starter
}

// dispatchServerStarter returns the wired A2A server starter, if any.
func (c *coordinator) dispatchServerStarter() DispatchServerStarter {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	return c.dispatchServer
}

// startDispatchServer stands up the dispatch's A2A server (#70) and
// stamps its endpoint and card on the registry entry — the in-memory
// discovery surface. A start failure is logged and swallowed: the
// dispatch itself does not depend on being served, and Phase 1 has no
// A2A client in the loop yet (#71 adds it); failing the dispatch over a
// loopback server would trade working dispatches for protocol purity.
func (c *coordinator) startDispatchServer(ctx context.Context, provider *dispatch.GitWorktreeProvider, reg *dispatch.AgentRegistry, entryID, sessionID, handle, role string, runner SessionAgent, loaded []*skills.Skill, call SessionAgentCall, inactivityTimeout time.Duration, cancelReason func() string) (stop func()) {
	starter := c.dispatchServerStarter()
	if starter == nil {
		return nil
	}

	endpoint, card, stop, err := starter.StartDispatchServer(ctx, DispatchServerParams{
		SessionID: sessionID,
		Runner:    runner,
		Diff: func(ctx context.Context) (string, error) {
			entry, ok := reg.Get(entryID)
			if !ok {
				return "", fmt.Errorf("unknown dispatch %q", entryID)
			}
			return provider.Diff(ctx, entry)
		},
		Todos:             c.dispatchCollector,
		Name:              handle,
		Description:       role,
		Skills:            loaded,
		Call:              call,
		InactivityTimeout: inactivityTimeout,
		CancelReason:      cancelReason,
	})
	if err != nil {
		slog.Warn("Dispatch A2A server failed to start", "dispatch_id", entryID, "error", err)
		return nil
	}
	reg.SetEndpoint(entryID, endpoint, card)
	slog.Debug("Dispatch A2A server started", "dispatch_id", entryID, "endpoint", endpoint)
	return stop
}

// resolvedSkills loads the skills a dispatch runs with from its scoped
// store: every discovered skill when nothing was requested, else exactly
// the requested ones. The card advertises what the agent actually has.
func resolvedSkills(store *config.ConfigStore, requested []string) []*skills.Skill {
	if store == nil {
		return nil
	}
	all, _ := discoverSkills(store)
	if len(requested) == 0 {
		return all
	}
	var out []*skills.Skill
	for _, s := range all {
		if slices.Contains(requested, s.Name) {
			out = append(out, s)
		}
	}
	return out
}

// stopDispatchServer tears a dispatch's A2A server down and clears the
// registry entry's endpoint and card: a finished dispatch serves
// nothing, and discovery must not hand out a dead endpoint (#70's
// teardown half).
func (c *coordinator) stopDispatchServer(reg *dispatch.AgentRegistry, entryID string, stop func()) {
	if stop != nil {
		stop()
	}
	reg.SetEndpoint(entryID, "", nil)
}
