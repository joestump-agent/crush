package agent

// Cancel a single dispatched agent (#373): the coordinator resolves a
// dispatch ID, @handle, or child session ID to a running dispatch and
// follows the watchdog's kill path — record the reason, cancel the
// dispatched agent — while leaving the dispatch's root context alive so
// terminal assembly can still capture the salvage diff. The model-facing
// front door is the cancel_dispatch tool; the human's is the UI binding.

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/dispatch"
)

//go:embed templates/cancel_dispatch.md
var cancelDispatchToolDescription string

// CancelDispatchToolName is the registered name of the CancelDispatch tool.
const CancelDispatchToolName = "cancel_dispatch"

// CancelDispatchParams are the CancelDispatch tool's arguments. Address
// the agent by dispatch ID or by its @handle (#313); exactly one is
// needed.
type CancelDispatchParams struct {
	// DispatchID is the dispatch identifier — the "dispatch_id" field of
	// the running handle the dispatch_agent tool returned.
	DispatchID string `json:"dispatch_id,omitempty" description:"Dispatch ID of the running dispatched agent (the \"dispatch_id\" from its dispatch handle)"`
	// Handle is the dispatched agent's @handle (#313) — the "handle"
	// field of the dispatch handle, or the handle the user asked to stop.
	Handle string `json:"handle,omitempty" description:"@handle of the running dispatched agent (the \"handle\" from its dispatch handle)"`
}

// CancelDispatch stops one dispatched agent on demand (#373). ref is a
// dispatch ID, an @handle, or a dispatched agent's child session ID —
// the session ID is what the UI's dispatch card carries. The ref
// resolves through the registry; an unknown ref is an error and a
// finished dispatch refuses with "already finished". A running dispatch
// follows the watchdog's kill path: the kill reason is recorded first,
// then the dispatched agent is canceled — never the parent, and never
// the dispatch's root context, which terminal assembly still needs to
// capture the salvage diff. The dispatch ends killed with the reason and
// its workspace is preserved (#367).
func (c *coordinator) CancelDispatch(ctx context.Context, ref string) error {
	ref = strings.TrimSpace(ref)
	reg := c.dispatchRegistry()

	// Dispatch IDs and child session IDs are resolved verbatim — they
	// carry characters the handle slug would mangle ($$, length caps) —
	// and the fuzzy handle form goes last, slugged the way handles are
	// stored so "@Team Lead" finds the "team-lead" entry.
	entry, ok := reg.Get(ref)
	if !ok {
		entry, ok = reg.BySession(ref)
	}
	if !ok {
		entry, ok = reg.ByHandle(dispatch.HandleSlug(ref))
	}
	if !ok {
		return fmt.Errorf("no dispatch %q is known; dispatch one first", ref)
	}
	if entry.Status.IsTerminal() {
		return fmt.Errorf("dispatch %s already finished (%s); task sessions are never continuable — dispatch a new agent instead", entry.ID, entry.Status)
	}

	c.dispatchMu.Lock()
	live := c.liveDispatches[entry.ID]
	c.dispatchMu.Unlock()
	if live == nil {
		// A non-terminal entry with no live run: a dispatch from an
		// earlier process, or the window between the registry turning
		// terminal and teardown dropping the record. There is nothing in
		// this process to cancel.
		return fmt.Errorf("dispatch %s is not running in this process", entry.ID)
	}

	// The watchdog's order (#316): the kill reason lands before the
	// cancel, so the terminal result is assembled as killed with it. The
	// root context is deliberately left alone — runDispatch assembles the
	// salvage diff on it after the run returns. The kill travels the
	// protocol (#348): a tasks/cancel carrying the reason ends the served
	// task; the direct agent cancel is the fallback (an unserved
	// dispatch, or a task ID the stream never reported). The send is
	// asynchronous: the SDK resolves a tasks/cancel only when the run
	// ends, and this call returns to the tool loop immediately.
	live.kill.kill(dispatch.ReasonCanceled)
	c.killDispatch(reg, entry.ID, dispatch.ReasonCanceled, func() {
		if live.agent != nil {
			live.agent.Cancel(live.sessionID)
		}
	})
	slog.Debug("Dispatch canceled on demand", "dispatch_id", entry.ID, "session_id", live.sessionID)
	return nil
}

// killDispatch routes one dispatched run's kill through the served
// dispatch's tasks/cancel (#348). The caller has already recorded the
// reason on the run's kill state; this sends the protocol cancel that
// carries it, so the terminal Canceled status — on the wire and in the
// task store — says why the run stopped, and an out-of-process agent
// (#72/#73) is killable at all. The fallback covers the paths no
// tasks/cancel can serve: an unserved dispatch, a task ID the stream has
// not reported yet (a kill before the first event), or a cancel that
// errors. The send runs on a detached context — the kill must land even
// if the dispatch's root is torn down under it — and a refused or
// already-terminal task falls back harmlessly. reg is the registry the
// kill's caller reads — the one its run was started against, passed in
// because the kill site owns it — not necessarily the coordinator's
// current one.
func (c *coordinator) killDispatch(reg *dispatch.AgentRegistry, entryID, reason string, fallback func()) {
	if entry, ok := reg.Get(entryID); ok && entry.Source != "" {
		// An external dispatch (#434) is killed through its kill switch:
		// its stream watches it, cuts itself, and sends the tasks/cancel
		// carrying the reason to the remote's own origin. The process
		// host's canceler cannot reach it, and there is no local agent
		// to fall back on. Callers record the reason first; recording it
		// here too keeps a caller that did not from losing the kill.
		c.dispatchMu.Lock()
		live := c.liveDispatches[entryID]
		c.dispatchMu.Unlock()
		if live != nil {
			live.kill.kill(reason)
		}
		return
	}
	if fallback == nil {
		fallback = func() { c.cancelDispatchRun(entryID) }
	}
	canceler, ok := c.a2aHost().(DispatchCanceler)
	if !ok || canceler == nil {
		fallback()
		return
	}
	entry, ok := reg.Get(entryID)
	if !ok || entry.Endpoint == "" || entry.AgentCard == nil || entry.TaskID == "" {
		fallback()
		return
	}
	go func() {
		err := canceler.CancelDispatch(context.WithoutCancel(context.Background()), DispatchCancelParams{
			Endpoint: entry.Endpoint,
			Card:     entry.AgentCard,
			TaskID:   entry.TaskID,
			Reason:   reason,
		})
		if err != nil {
			slog.Warn("Dispatch kill via tasks/cancel failed; using the direct cancel", "dispatch_id", entryID, "reason", reason, "error", err)
			fallback()
		}
	}()
}

// cancelDispatchRun is the direct in-process kill (#430): the
// dispatch's root cancel ends a run whose session the agent never
// registered — agent.Cancel alone is a no-op there — and the agent
// cancel ends the rest. No-op when there is no live run (the dispatch
// never started, or its teardown already ran).
func (c *coordinator) cancelDispatchRun(entryID string) {
	c.dispatchMu.Lock()
	live := c.liveDispatches[entryID]
	c.dispatchMu.Unlock()
	if live == nil {
		return
	}
	if live.cancel != nil {
		live.cancel()
	}
	if live.agent != nil {
		live.agent.Cancel(live.sessionID)
	}
}

// cancelDispatchTool builds the CancelDispatch tool (#373): the model's
// front door for stopping one dispatched agent mid-run, next to the
// dispatch_agent tool that starts them.
func (c *coordinator) cancelDispatchTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		CancelDispatchToolName,
		cancelDispatchToolDescription,
		func(ctx context.Context, params CancelDispatchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ref := params.DispatchID
			if ref == "" {
				ref = params.Handle
			}
			if ref == "" {
				return fantasy.NewTextErrorResponse("dispatch_id or handle is required"), nil
			}
			if err := c.CancelDispatch(ctx, ref); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			named := params.DispatchID
			if named == "" {
				named = "@" + dispatch.HandleSlug(params.Handle)
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Canceling dispatched agent %s. Its run ends killed with reason %q; the workspace is preserved for the re-dispatch-or-dismiss decision.", named, dispatch.ReasonCanceled)), nil
		},
	)
}
