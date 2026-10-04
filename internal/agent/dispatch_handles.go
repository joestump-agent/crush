package agent

import (
	"context"
	"fmt"

	"github.com/charmbracelet/crush/internal/dispatch"
)

// DispatchLive returns the snapshots of every non-terminal dispatch
// (#313) — the live-agents source behind the editor's @ completions:
// handle, one-line role, status, current todo. Finished handles never
// appear, so nothing the popup offers can be addressed into a
// continuation.
func (c *coordinator) DispatchLive() []dispatch.TodoSnapshot {
	collector := c.activeDispatchCollector()
	if collector == nil {
		return nil
	}
	return collector.LiveSnapshots()
}

// DispatchByHandle resolves an @handle to its dispatch snapshot (#313):
// the registry is the handle namespace until an entry is removed, so
// finished dispatches resolve too — the caller decides what a finished
// handle means (a read-only card for a mention, a routing refusal for a
// direct address).
func (c *coordinator) DispatchByHandle(handle string) (dispatch.TodoSnapshot, bool) {
	collector := c.activeDispatchCollector()
	if collector == nil {
		return dispatch.TodoSnapshot{}, false
	}
	return collector.SnapshotByHandle(handle)
}

// activeDispatchCollector returns the todo collector, creating the
// dispatch workspace (and with it the collector) on first use so the
// handle surfaces work even before the first dispatch tool call. A
// working directory with no git repository has no dispatches: the
// creation error is cached by dispatchWorkspace and the collector stays
// nil, so the live/handle surfaces report empty rather than failing.
func (c *coordinator) activeDispatchCollector() *dispatch.TodoCollector {
	if _, err := c.dispatchWorkspace(); err != nil {
		return nil
	}
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	return c.dispatchCollector
}

// DeliverAgentMessageByHandle delivers a message to the dispatched agent
// carrying handle (#313): the editor's leading @handle routing front
// door over the #312 injection seam. A finished handle refuses cleanly —
// dispatch a new agent instead — matching the seam's session-keyed
// contract.
func (c *coordinator) DeliverAgentMessageByHandle(ctx context.Context, handle, text string) error {
	workspace, err := c.dispatchWorkspace()
	if err != nil {
		return err
	}
	// Tolerate the addressed form ("@Tester") as well as the bare slug:
	// both the editor and the tool may pass either.
	handle = dispatch.HandleSlug(handle)
	entry, ok := workspace.ByHandle(handle)
	if !ok {
		return fmt.Errorf("no agent with handle @%s is known; dispatch one first", handle)
	}
	if entry.Status.IsTerminal() {
		return fmt.Errorf("agent @%s finished (%s); task sessions are never continuable — dispatch a new agent instead", handle, entry.Status)
	}
	return c.DeliverAgentMessage(ctx, AgentMessage{SessionID: entry.SessionID, Text: text})
}
