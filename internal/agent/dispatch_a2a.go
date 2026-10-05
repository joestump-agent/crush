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
// interface. The production implementation is a2a.ServerFactory, whose
// process-wide host serves every dispatch on one per-process unix
// socket (#346); tests substitute fakes through it, and a nil starter
// (the default until the app wires the factory) simply serves nothing.
type DispatchServerStarter interface {
	// StartDispatchServer stands up the A2A server for one dispatch on
	// the process host and returns its endpoint, the AgentCard to stamp
	// on the registry entry (opaque here), and the stop function the
	// dispatch run defers.
	StartDispatchServer(ctx context.Context, params DispatchServerParams) (endpoint string, card any, stop func(), err error)
}

// DispatchServerParams is one dispatch's slice of the A2A server (#70):
// what to serve, on which session, and where its card's identity comes
// from. Todos is spelled structurally so the agent package stays
// import-clean of internal/a2a; the production value is the dispatch
// registry's todo collector.
type DispatchServerParams struct {
	// DispatchID is the registry entry's id the served dispatch answers
	// as (#346): the process host routes /agents/<DispatchID> to it.
	DispatchID string
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
}

// DispatchTransportOutcome is the terminal outcome of one A2A-driven
// dispatch, in transport vocabulary; the coordinator maps it onto the
// DispatchResult. Status is one of "completed", "failed", "canceled".
// Text is the agent's final message (findings, or the failure reason).
// Diff is the artifact text when one arrived. WorkingEvents counts the
// non-terminal progress events observed on the wire — consumed, not
// re-published: in-process the agent block renders from the todo
// collector, and this count is the seam #72/#73 pick up.
type DispatchTransportOutcome struct {
	Status        string
	Text          string
	Diff          string
	WorkingEvents int
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
// is reserved for wander kill (#316). A loop stop arrives as the kill
// state's tool-loop reason, recorded in-process by the served agent's
// observer. A completed outcome with no diff on the wire falls back to
// an in-process capture so a capture error surfaces as "(diff
// unavailable: ...)" instead of "(no changes)" (#361 puts the error on
// the wire and deletes this).
func dispatchNaturalOutcomeFromTransport(run dispatchRun, outcome DispatchTransportOutcome) dispatchNaturalOutcome {
	natural := dispatchNaturalOutcome{
		diff: func(ctx context.Context) (string, error) {
			return run.workspace.Diff(ctx, run.entry.ID)
		},
	}
	if outcome.Diff != "" {
		natural.diff = func(context.Context) (string, error) {
			return outcome.Diff, nil
		}
	}
	switch outcome.Status {
	case transportStatusCompleted:
		natural.completed = true
		natural.findings = outcome.Text
	case transportStatusCanceled:
		natural.runErr = fmt.Errorf("dispatch canceled: %s", cmp.Or(outcome.Text, "no reason given"))
	default:
		natural.runErr = errors.New(cmp.Or(outcome.Text, "dispatch failed without a reason"))
	}
	return natural
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
	entry, ok := run.workspace.Get(run.entry.ID)
	if !ok || entry.Endpoint == "" || entry.AgentCard == nil {
		return dispatch.DispatchResult{}, false
	}
	outcome, err := transport.StreamDispatch(ctx, DispatchTransportParams{
		Endpoint: entry.Endpoint,
		Card:     entry.AgentCard,
		Prompt:   run.prompt,
	})
	if err != nil {
		slog.Error("Dispatch A2A stream failed", "dispatch_id", run.entry.ID, "session_id", run.sessionID, "error", err)
		outcome = DispatchTransportOutcome{
			Status: transportStatusFailed,
			Text:   err.Error(),
		}
	}
	return c.assembleTerminalDispatchResult(ctx, run, dispatchNaturalOutcomeFromTransport(run, outcome)), true
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
func (c *coordinator) startDispatchServer(ctx context.Context, workspace *dispatch.Workspace, entryID, sessionID, handle, role string, runner SessionAgent, loaded []*skills.Skill, call SessionAgentCall, inactivityTimeout time.Duration, cancelReason func() string) (stop func()) {
	starter := c.dispatchServerStarter()
	if starter == nil {
		return nil
	}

	endpoint, card, stop, err := starter.StartDispatchServer(ctx, DispatchServerParams{
		DispatchID:        entryID,
		SessionID:         sessionID,
		Runner:            runner,
		Diff:              func(ctx context.Context) (string, error) { return workspace.Diff(ctx, entryID) },
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
	workspace.SetEndpoint(entryID, endpoint, card)
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
func (c *coordinator) stopDispatchServer(workspace *dispatch.Workspace, entryID string, stop func()) {
	if stop != nil {
		stop()
	}
	workspace.SetEndpoint(entryID, "", nil)
}
