package model

// Inspect mode (#314): drill into a sub-agent's session and read its full
// transcript (reasoning, tool calls, results) in the normal chat window
// while the parent stays the active session. ctrl+] enters from an agent
// block (or opens the live-agents cycle when no block is focused) and
// cycles live agents; ctrl+[ returns to the chat with the scroll position
// preserved. Esc is untouched. Viewed ≠ active: nothing in this file ever
// assigns m.session, so prompts typed while inspecting land in the parent
// and a task session can never become the active/continuable session.

import (
	"context"
	"fmt"
	"slices"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// inspectPlaceholderWidth bounds the inspect placeholder so a long child
// session title cannot wrap the editor row.
const inspectPlaceholderWidth = 64

// agentBlockRef is one drill-in target found in the chat transcript: a
// dispatch block or a plain agent-tool block, its child session ID, and
// whether the child is believed to be live. Liveness is judged from
// in-memory block state only, never a workspace probe, which would be a
// synchronous HTTP round-trip in client/server mode.
type agentBlockRef struct {
	sessionID string
	index     int
	live      bool
}

// inspectSessionLoadedMsg carries the fetched child session and its
// transcript for the inspect view.
type inspectSessionLoadedMsg struct {
	sess     *session.Session
	messages []message.Message
}

// inspectRestoreMsg carries the parent transcript to restore on exit,
// plus the scroll position captured on entry.
type inspectRestoreMsg struct {
	messages   []message.Message
	scrollIdx  int
	scrollLine int
}

// isInspecting reports whether the UI is currently viewing a sub-agent
// session.
func (m *UI) isInspecting() bool {
	return m.inspecting != nil
}

// inspectingSessionID returns the viewed session's ID, or "" when not
// inspecting.
func (m *UI) inspectingSessionID() string {
	if m.inspecting == nil {
		return ""
	}
	return m.inspecting.ID
}

// clearInspectState drops the inspect state without touching the chat.
// Called whenever the active session changes underneath inspect mode
// (session switch, new session) so the view never claims to inspect a
// session it is not showing.
func (m *UI) clearInspectState() {
	m.inspecting = nil
	m.inspectRing = nil
	m.inspectRingPos = 0
	m.inspectLoadBusy = false
}

// agentBlocks enumerates the drill-in targets in the chat transcript in
// list order: dispatch blocks (dispatch_agent tool calls) and plain
// agent-tool blocks. A block with no resolvable child session is skipped.
func (m *UI) agentBlocks() []agentBlockRef {
	refs := make([]agentBlockRef, 0, 4)
	for i := range m.chat.Len() {
		item := m.chat.ItemAt(i)
		if item == nil {
			continue
		}
		switch block := item.(type) {
		case *chat.DispatchToolMessageItem:
			sid := block.DispatchSessionID()
			if sid == "" {
				continue
			}
			refs = append(refs, agentBlockRef{sessionID: sid, index: i, live: block.IsLive()})
		case *chat.AgentToolMessageItem:
			if block.ToolCall().ID == "" || block.MessageID() == "" {
				continue
			}
			sid := m.com.Workspace.CreateAgentToolSessionID(block.MessageID(), block.ToolCall().ID)
			live := block.Status() == chat.ToolStatusRunning ||
				block.Status() == chat.ToolStatusAwaitingPermission
			refs = append(refs, agentBlockRef{sessionID: sid, index: i, live: live})
		}
	}
	return refs
}

// isLiveChildSession reports whether the given session is one of the
// transcript's live drill-in targets. Runs on the Update goroutine from
// in-memory block state only.
func (m *UI) isLiveChildSession(sessionID string) bool {
	for _, ref := range m.agentBlocks() {
		if ref.sessionID == sessionID {
			return ref.live
		}
	}
	return false
}

// liveAgentSessionIDs returns the live drill-in targets' session IDs in
// transcript order.
func (m *UI) liveAgentSessionIDs() []string {
	ids := make([]string, 0, 4)
	for _, ref := range m.agentBlocks() {
		if ref.live {
			ids = append(ids, ref.sessionID)
		}
	}
	return ids
}

// agentBlockAt reports whether the chat item at the given index is a
// drill-in target.
func (m *UI) agentBlockAt(index int) (agentBlockRef, bool) {
	if index < 0 || index >= m.chat.Len() {
		return agentBlockRef{}, false
	}
	for _, ref := range m.agentBlocks() {
		if ref.index == index {
			return ref, true
		}
	}
	return agentBlockRef{}, false
}

// handleSelectSession routes a sessions-picker selection (#314): a task
// session opens in the read-only inspect view and can never become the
// active, continuable session; a parent session loads as active.
func (m *UI) handleSelectSession(sess session.Session) tea.Cmd {
	if sess.ParentSessionID != "" {
		return m.enterInspect(agentBlockRef{sessionID: sess.ID})
	}
	return m.loadSession(sess.ID)
}

// handleInspectKeys routes ctrl+] and ctrl+[ (#314). It runs after the
// dialog routing in handleKeyPressMsg, so open dialogs keep their keys,
// and before every other handler, so the bindings work from both editor
// and chat focus. Only these two chords match; Esc and every other key
// fall through untouched.
func (m *UI) handleInspectKeys(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.InspectDrill):
		return true, m.handleInspectDrill()
	case key.Matches(msg, m.keyMap.InspectBack):
		if !m.isInspecting() {
			return false, nil
		}
		return true, m.exitInspect()
	}
	return false, nil
}

// handleInspectDrill implements ctrl+]: while inspecting, cycle to the
// next live agent; otherwise enter the inspect view of the focused agent
// block, or of the first live agent when no block is focused.
func (m *UI) handleInspectDrill() tea.Cmd {
	if m.isInspecting() {
		return m.cycleInspectAgent()
	}

	if ref, ok := m.agentBlockAt(m.chat.Selected()); ok && ref.sessionID != "" {
		return m.enterInspect(ref)
	}

	var liveRefs []agentBlockRef
	for _, ref := range m.agentBlocks() {
		if ref.live {
			liveRefs = append(liveRefs, ref)
		}
	}
	if len(liveRefs) == 0 {
		return util.ReportInfo("No live sub-agents to inspect")
	}
	m.chat.SetSelected(liveRefs[0].index)
	m.chat.ScrollToSelected()
	return m.enterInspect(liveRefs[0])
}

// enterInspect captures the parent's scroll position and live-agent ring
// (only on the transition into inspect mode) and starts the child
// transcript load.
func (m *UI) enterInspect(ref agentBlockRef) tea.Cmd {
	if !m.isInspecting() {
		idx, line := m.chat.ScrollPosition()
		m.inspectScroll = [2]int{idx, line}
		m.inspectRing = m.liveAgentSessionIDs()
		m.inspectRingPos = 0
	}
	if pos := slices.Index(m.inspectRing, ref.sessionID); pos >= 0 {
		m.inspectRingPos = pos
	}
	return m.loadInspectSession(ref.sessionID)
}

// cycleInspectAgent moves to the next live agent in the entry ring,
// wrapping around. The ring is the snapshot taken when inspect mode was
// entered; agents dispatched after that are picked up on the next entry.
func (m *UI) cycleInspectAgent() tea.Cmd {
	if len(m.inspectRing) == 0 {
		return util.ReportInfo("No live sub-agents to cycle through")
	}
	m.inspectRingPos = (m.inspectRingPos + 1) % len(m.inspectRing)
	return m.loadInspectSession(m.inspectRing[m.inspectRingPos])
}

// loadInspectSession fetches a child session and its transcript
// off-thread and returns a command delivering inspectSessionLoadedMsg.
func (m *UI) loadInspectSession(sessionID string) tea.Cmd {
	if m.inspectLoadBusy {
		return nil
	}
	m.inspectLoadBusy = true
	return func() tea.Msg {
		sess, err := m.com.Workspace.GetSession(context.Background(), sessionID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		msgs, err := m.com.Workspace.ListMessages(context.Background(), sessionID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return inspectSessionLoadedMsg{sess: &sess, messages: msgs}
	}
}

// handleInspectLoaded swaps the chat over to the child transcript. The
// active session, sidebar, prompt queue, prompt history, and every other
// active-session concern keep pointing at the parent: this handler only
// touches the chat view.
func (m *UI) handleInspectLoaded(msg inspectSessionLoadedMsg) tea.Cmd {
	m.inspectLoadBusy = false
	m.inspecting = msg.sess

	// Liveness is read from the parent's blocks before the transcript
	// swap takes them out of the chat.
	live := m.isLiveChildSession(msg.sess.ID)

	cmd := m.setSessionMessages(msg.messages)
	if live {
		// A running child keeps its spinners going; setSessionMessages
		// gates the animation clock on the parent's busy state.
		m.chat.SetAnimationsAllowed(true)
	}
	// Start at the end of the transcript and follow the stream: a live
	// child keeps the view pinned to the bottom as messages land.
	m.chat.ScrollToBottom()
	m.chat.SelectLast()
	m.invalidateFrames()
	return cmd
}

// exitInspect leaves inspect mode: it reloads the parent transcript
// off-thread (the parent may have streamed messages while its blocks
// were not on screen) and restores the scroll position captured on
// entry.
func (m *UI) exitInspect() tea.Cmd {
	m.inspecting = nil
	m.inspectRing = nil
	m.inspectRingPos = 0
	m.inspectLoadBusy = false
	scroll := m.inspectScroll
	parentID := m.currentSessionID()
	return func() tea.Msg {
		msgs, err := m.com.Workspace.ListMessages(context.Background(), parentID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return inspectRestoreMsg{messages: msgs, scrollIdx: scroll[0], scrollLine: scroll[1]}
	}
}

// handleInspectRestore swaps the parent transcript back in and restores
// the captured scroll position, clamped to whatever the reloaded
// transcript can honor.
func (m *UI) handleInspectRestore(msg inspectRestoreMsg) tea.Cmd {
	cmd := m.setSessionMessages(msg.messages)
	m.chat.ScrollToIndex(msg.scrollIdx)
	if msg.scrollLine > 0 {
		m.chat.ScrollBy(msg.scrollLine)
	}
	m.invalidateFrames()
	return cmd
}

// handleInspectChildMessage applies a live message from the inspected
// child session to the chat. It reuses the session message pipeline so
// the transcript renders exactly like it would loaded.
func (m *UI) handleInspectChildMessage(event pubsub.Event[message.Message]) tea.Cmd {
	switch event.Type {
	case pubsub.CreatedEvent:
		return m.appendSessionMessage(event.Payload)
	case pubsub.UpdatedEvent:
		return m.updateSessionMessage(event.Payload)
	case pubsub.DeletedEvent:
		m.chat.RemoveMessage(event.Payload.ID)
	}
	return nil
}

// inspectPlaceholder renders the editor placeholder shown while
// inspecting: a persistent reminder of what is on screen, where prompts
// land, and how to leave.
func (m *UI) inspectPlaceholder() string {
	title := "sub-agent"
	if m.inspecting != nil && m.inspecting.Title != "" {
		title = m.inspecting.Title
	}
	pos := ""
	if len(m.inspectRing) > 1 {
		pos = fmt.Sprintf(" (%d/%d)", m.inspectRingPos+1, len(m.inspectRing))
	}
	text := fmt.Sprintf("Inspecting %s%s · ctrl+[ returns · prompts go to the session", title, pos)
	return ansi.Truncate(text, inspectPlaceholderWidth, "…")
}
