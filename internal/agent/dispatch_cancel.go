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
	c.dispatchMu.Lock()
	workspace := c.dispatchWS
	c.dispatchMu.Unlock()
	if workspace == nil {
		return fmt.Errorf("no dispatch %q is known; dispatch one first", ref)
	}

	// Dispatch IDs and child session IDs are resolved verbatim — they
	// carry characters the handle slug would mangle ($$, length caps) —
	// and the fuzzy handle form goes last, slugged the way handles are
	// stored so "@Team Lead" finds the "team-lead" entry.
	entry, ok := workspace.Get(ref)
	if !ok {
		entry, ok = workspace.BySession(ref)
	}
	if !ok {
		entry, ok = workspace.ByHandle(dispatch.HandleSlug(ref))
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
	// salvage diff on it after the run returns.
	live.kill.kill(dispatch.ReasonCanceled)
	live.agent.Cancel(live.sessionID)
	slog.Debug("Dispatch canceled on demand", "dispatch_id", entry.ID, "session_id", live.sessionID)
	return nil
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
