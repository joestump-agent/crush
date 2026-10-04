package model

// Inspect mode (#314): drill into a sub-agent's session and read its full
// transcript (reasoning, tool calls, results) in the normal chat window
// while the parent stays the active session. ctrl+] enters from an agent
// block (or opens the live-agents cycle when no block is focused) and
// cycles live agents; ctrl+[ returns to the chat with the scroll position
// preserved. Esc is untouched wherever the terminal can tell it apart from
// ctrl+[ (see escIsInspectBack). Viewed ≠ active: nothing in this file ever
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

// inspectPlaceholderWidth bounds the inspect placeholder before the
// editor has been sized, so a long child session title cannot wrap the
// editor row.
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
// transcript for the inspect view. seq is the inspect transition the
// fetch started under; a later transition makes it stale.
type inspectSessionLoadedMsg struct {
	seq      int
	sess     *session.Session
	messages []message.Message
}

// inspectRestoreMsg carries the parent transcript to restore on exit,
// plus the scroll state captured on entry.
type inspectRestoreMsg struct {
	sessionID  string
	messages   []message.Message
	scrollIdx  int
	scrollLine int
	follow     bool
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
// session it is not showing. Bumping inspectSeq drops any child load
// still in flight.
func (m *UI) clearInspectState() {
	m.inspecting = nil
	m.inspectRing = nil
	m.inspectRingPos = 0
	m.inspectSeq++
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
//
// A task session whose parent is not the active session (another one, or
// none yet, on the landing screen) loads that parent first and opens in
// inspect mode once it is on screen: prompts then land in the session
// the transcript came from, and the ring holds that parent's agents.
func (m *UI) handleSelectSession(sess session.Session) tea.Cmd {
	if sess.ParentSessionID == "" {
		return m.loadSession(sess.ID)
	}
	if sess.ParentSessionID != m.currentSessionID() {
		m.inspectPending = &sess
		return m.loadSession(sess.ParentSessionID)
	}
	return m.enterInspect(agentBlockRef{sessionID: sess.ID})
}

// takeInspectPending returns the task session waiting for the given
// parent to load, if any, and clears it either way so a failed load
// cannot fire it against a later, unrelated session.
func (m *UI) takeInspectPending(loadedID string) *session.Session {
	pending := m.inspectPending
	m.inspectPending = nil
	if pending == nil || pending.ParentSessionID != loadedID {
		return nil
	}
	return pending
}

// handleInspectKeys routes ctrl+] and ctrl+[ (#314). It runs after the
// dialog routing in handleKeyPressMsg, so open dialogs keep their keys,
// and before every other handler, so the bindings work from both editor
// and chat focus. Only these two chords match; Esc and every other key
// fall through untouched, except where Esc is ctrl+[ (escIsInspectBack).
func (m *UI) handleInspectKeys(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.InspectDrill):
		return true, m.handleInspectDrill()
	case key.Matches(msg, m.keyMap.InspectBack), m.escIsInspectBack(msg):
		if !m.isInspecting() {
			return false, nil
		}
		return true, m.exitInspect()
	}
	return false, nil
}

// escIsInspectBack reports whether an esc press is really ctrl+[. A
// terminal without key disambiguation sends the same byte for both, so
// there ctrl+[ only ever arrives as esc; read as esc it reaches the
// cancel handler, and a second press would cancel the parent's run.
// Terminals that tell the two apart keep esc's own meaning.
func (m *UI) escIsInspectBack(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyEscape && msg.Mod == 0 &&
		!m.keyenh.SupportsKeyDisambiguation()
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
	return tea.Batch(m.chat.ScrollToSelected(), m.enterInspect(liveRefs[0]))
}

// enterInspect captures the parent's scroll position and live-agent ring
// (only on the transition into inspect mode) and starts the child
// transcript load.
func (m *UI) enterInspect(ref agentBlockRef) tea.Cmd {
	if !m.isInspecting() {
		idx, line := m.chat.ScrollPosition()
		m.inspectScroll = [2]int{idx, line}
		m.inspectFollow = m.chat.Follow()
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
// Each call supersedes the loads before it: only the latest one lands,
// so rapid cycling settles on the last press and the ring position
// always names the session on screen.
func (m *UI) loadInspectSession(sessionID string) tea.Cmd {
	m.inspectSeq++
	seq := m.inspectSeq
	ws := m.com.Workspace
	return func() tea.Msg {
		sess, err := ws.GetSession(context.Background(), sessionID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		msgs, err := ws.ListMessages(context.Background(), sessionID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return inspectSessionLoadedMsg{seq: seq, sess: &sess, messages: msgs}
	}
}

// handleInspectLoaded swaps the chat over to the child transcript. The
// active session, sidebar, prompt queue, prompt history, and every other
// active-session concern keep pointing at the parent: this handler only
// touches the chat view.
func (m *UI) handleInspectLoaded(msg inspectSessionLoadedMsg) tea.Cmd {
	if msg.seq != m.inspectSeq {
		// Superseded by a later cycle, ctrl+[, or session switch.
		return nil
	}
	m.inspecting = msg.sess

	// Liveness comes from the ring captured off the parent's blocks at
	// entry: when cycling, the chat holds the previous child, not them.
	live := slices.Contains(m.inspectRing, msg.sess.ID)

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
// were not on screen) and restores the scroll state captured on entry.
// Clearing the inspect state drops any child load still in flight.
func (m *UI) exitInspect() tea.Cmd {
	m.clearInspectState()
	scroll, follow := m.inspectScroll, m.inspectFollow
	parentID := m.currentSessionID()
	ws := m.com.Workspace
	return func() tea.Msg {
		msgs, err := ws.ListMessages(context.Background(), parentID)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return inspectRestoreMsg{
			sessionID:  parentID,
			messages:   msgs,
			scrollIdx:  scroll[0],
			scrollLine: scroll[1],
			follow:     follow,
		}
	}
}

// handleInspectRestore swaps the parent transcript back in and restores
// the captured scroll state: a parent that was following the stream
// comes back at the bottom, still following; otherwise the captured
// position, clamped to whatever the reloaded transcript can honor. A
// restore that lands after a re-entry, or after the active session
// changed, would paint the wrong transcript and is dropped.
func (m *UI) handleInspectRestore(msg inspectRestoreMsg) tea.Cmd {
	if m.isInspecting() || msg.sessionID != m.currentSessionID() {
		return nil
	}
	cmds := []tea.Cmd{m.setSessionMessages(msg.messages)}
	if msg.follow {
		cmds = append(cmds, m.chat.ScrollToBottom())
	} else {
		cmds = append(cmds, m.chat.ScrollToIndex(msg.scrollIdx))
		if msg.scrollLine > 0 {
			cmds = append(cmds, m.chat.ScrollBy(msg.scrollLine))
		}
	}
	m.invalidateFrames()
	return tea.Batch(cmds...)
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
// land, and how to leave. The title is what gets truncated, so the way
// back stays visible however long the child session's title is.
func (m *UI) inspectPlaceholder() string {
	title := "sub-agent"
	if m.inspecting != nil && m.inspecting.Title != "" {
		title = m.inspecting.Title
	}
	pos := ""
	if len(m.inspectRing) > 1 {
		pos = fmt.Sprintf(" (%d/%d)", m.inspectRingPos+1, len(m.inspectRing))
	}
	const prefix = "Inspecting "
	suffix := pos + " · ctrl+[ returns · prompts go to the parent"
	width := m.textarea.Width() - 1
	if width <= 0 {
		width = inspectPlaceholderWidth
	}
	room := max(1, width-ansi.StringWidth(prefix)-ansi.StringWidth(suffix))
	text := prefix + ansi.Truncate(title, room, "…") + suffix
	return ansi.Truncate(text, width, "…")
}
