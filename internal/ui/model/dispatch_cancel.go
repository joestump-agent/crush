package model

// Canceling a dispatched agent from the UI (#373): ctrl+x ends one
// dispatched agent's run. The target is the dispatch being viewed in
// inspect mode, or the selected chat item when it is a live dispatch
// card. The kill goes through the workspace's CancelAgentTask — A2A
// tasks/cancel (#421) — which refuses unknown, finished and not yet
// started agents; the card flips to "canceled" once the agent surface
// reports it. This is the human's path and is deliberately not scoped
// to a session (#559): the workspace resolves any dispatch it follows.
// The model's cancel_dispatch tool is the scoped one — see the
// coordinator's CancelDispatch — and this binding never routes through
// it.

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// focusedLiveDispatchSessionID returns the child session ID of the live
// dispatch ctrl+x would target: the inspected dispatch while inspecting,
// else the selected chat item when it is a live dispatch card. Empty when
// there is no target.
//
// While inspecting, the transcript on screen is the child's and the
// parent's dispatch blocks are not in the chat list, so the target check
// uses the dispatch set captured when inspect mode was entered — the same
// entry snapshot the ctrl+] ring rides on. A dispatch that finished after
// entry still appears targetable; the workspace refuses the kill with a
// clear error, mirroring how the ring treats agents that end mid-cycle.
func (m *UI) focusedLiveDispatchSessionID() string {
	if m.isInspecting() {
		sid := m.inspectingSessionID()
		if m.inspectDispatchTargets[sid] {
			return sid
		}
		return ""
	}
	if m.state != uiChat || m.focus != uiFocusMain {
		return ""
	}
	block, ok := m.chat.ItemAt(m.chat.Selected()).(*chat.DispatchToolMessageItem)
	if !ok || !block.IsLive() {
		return ""
	}
	return block.DispatchSessionID()
}

// cancelDispatchAgent kills the dispatched agent behind the given child
// session ID (#373) and reports the outcome the way the parent agent
// reports: errors surface as error toasts, success as an info toast. The
// card itself flips to "canceled" when the agent surface reports it.
func (m *UI) cancelDispatchAgent(sessionID string) tea.Cmd {
	if err := m.com.Workspace.CancelAgentTask(context.Background(), sessionID); err != nil {
		return util.ReportError(err)
	}
	return util.ReportInfo(fmt.Sprintf("Canceling dispatched agent %s", sessionID))
}
