package agent

import (
	"context"
	"fmt"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
)

// DispatchLive returns the snapshots of every non-terminal dispatch
// (#313) — the live-agents source behind the editor's @ completions:
// handle, one-line role, status, current todo. Finished handles never
// appear, so nothing the popup offers can be addressed into a
// continuation. Only dispatches created from sessionID appear (#399):
// one session's agents are invisible to another's surfaces. Reading
// never provisions the workspace: before the first dispatch there is
// no registry and nothing is live (#370).
func (c *coordinator) DispatchLive(sessionID string) []dispatch.TodoSnapshot {
	c.dispatchMu.Lock()
	collector := c.dispatchCollector
	c.dispatchMu.Unlock()
	if collector == nil {
		return nil
	}
	return scopedSnapshots(collector.LiveSnapshots(), sessionID)
}

// DispatchByHandle resolves an @handle to its dispatch snapshot (#313):
// finished dispatches resolve too — the caller decides what a finished
// handle means (a read-only card for a mention, a routing refusal for a
// direct address). A handle dispatched from another session does not
// resolve (#399): ok=false, exactly like an unknown handle. Reading
// never provisions the workspace: before the first dispatch every
// handle is unknown (#370).
func (c *coordinator) DispatchByHandle(sessionID, handle string) (dispatch.TodoSnapshot, bool) {
	c.dispatchMu.Lock()
	collector := c.dispatchCollector
	c.dispatchMu.Unlock()
	if collector == nil {
		return dispatch.TodoSnapshot{}, false
	}
	snap, ok := collector.SnapshotByHandle(handle)
	if !ok || !inScope(snap.Entry, sessionID) {
		return dispatch.TodoSnapshot{}, false
	}
	return snap, true
}

// scopedSnapshots keeps only the snapshots whose dispatch was created
// from sessionID (#399).
func scopedSnapshots(snaps []dispatch.TodoSnapshot, sessionID string) []dispatch.TodoSnapshot {
	out := make([]dispatch.TodoSnapshot, 0, len(snaps))
	for _, snap := range snaps {
		if inScope(snap.Entry, sessionID) {
			out = append(out, snap)
		}
	}
	return out
}

// inScope reports whether entry belongs to sessionID's surfaces: the
// entry must carry the session as its parent, so an entry with no
// parent — one no session created — matches nothing (#399).
func inScope(entry dispatch.Entry, sessionID string) bool {
	return sessionID != "" && entry.ParentSessionID == sessionID
}

// dispatchCallerSession is the session a dispatch decision tool runs in
// (#559). cancel_dispatch, apply_dispatch and dismiss_dispatch act only
// on the calling session's dispatches, so a call with no session behind
// it refuses here instead of falling through to an unscoped lookup. The
// agent loop always stamps the session on a tool call's context; this
// is the backstop for anything that does not.
func dispatchCallerSession(ctx context.Context, toolName string) (string, error) {
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return "", fmt.Errorf("%s has no calling session; it acts only on dispatches of the session it runs in", toolName)
	}
	return sessionID, nil
}

// DeliverAgentMessageByHandle delivers a message to the dispatched agent
// carrying handle (#313): the editor's leading @handle routing front
// door over the #312 injection seam. The message's attachments (#414)
// ride with it: the editor's pasted images, files and long pastes.
// A finished handle refuses cleanly — dispatch a new agent instead —
// matching the seam's session-keyed contract. A handle dispatched from
// another session refuses exactly like an unknown one (#399); the
// caller's sessionID scopes the delivery, so session B cannot address —
// or even learn about — session A's agent. Delivery never provisions
// the workspace: before the first dispatch every handle is unknown
// (#370).
func (c *coordinator) DeliverAgentMessageByHandle(ctx context.Context, sessionID, handle, text string, attachments []message.Attachment) error {
	// Tolerate the addressed form ("@Tester") as well as the bare slug:
	// both the editor and the tool may pass either.
	handle = dispatch.HandleSlug(handle)
	entry, ok := c.dispatchRegistry().ByHandle(handle)
	if !ok {
		return fmt.Errorf("no agent with handle @%s is known; dispatch one first", handle)
	}
	if !inScope(entry, sessionID) {
		return fmt.Errorf("no agent with handle @%s in this session; dispatch one first", handle)
	}
	if entry.Status.IsTerminal() {
		return fmt.Errorf("agent @%s finished (%s); task sessions are never continuable — dispatch a new agent instead", handle, entry.Status)
	}
	return c.DeliverAgentMessage(ctx, AgentMessage{SessionID: entry.SessionID, FromSessionID: sessionID, Text: text, Attachments: attachments})
}
