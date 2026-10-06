package model

// Inspect mode (#314): drill into a sub-agent's session and read its full
// transcript (reasoning, tool calls, results) in the normal chat window
// while the parent stays the active session. ctrl+] enters from an agent
// block (or opens the live-agents cycle when no block is focused) and
// cycles live agents; esc or ctrl+[ returns to the chat with the scroll
// position preserved, on every terminal (#404). Esc never cancels the
// parent from inspect mode: leave inspect mode first to cancel. Viewed ≠
// active: nothing in this file ever assigns m.session, so prompts typed
// while inspecting land in the parent and a task session can never become
// the active/continuable session.

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
// plus the scroll state captured on entry. seq is the exit transition
// the fetch started under; a later transition makes it stale.
type inspectRestoreMsg struct {
	seq        int
	sessionID  string
	messages   []message.Message
	scrollIdx  int
	scrollLine int
	follow     bool
}

// inspectWindow holds one in-flight inspect transition: the seq it was
// issued under, the session whose snapshot will paint the chat, and the
// message events published while the fetch ran. Buffering the target's
// events closes the enter/exit snapshot race (#406): nothing is painted
// into a transcript the snapshot is about to replace, and events the
// snapshot cannot contain are replayed on top of it.
type inspectWindow struct {
	seq       int
	sessionID string
	events    []pubsub.Event[message.Message]
}

// inspectFetchFailedMsg reports a failed inspect transition fetch. seq
// matches it against the pending window, so a superseded failure can
// never clear a live one.
type inspectFetchFailedMsg struct {
	seq int
	err error
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

// clearInspectState drops the inspect state. Called whenever the active
// session changes underneath inspect mode (session switch, new session)
// or the view leaves, so it never claims to inspect a session it is not
// showing. Bumping inspectSeq drops any child load still in flight, and
// with it the transition's buffered events. The chat's read-only flag
// goes off with it (#407): every transcript rebuilt after a clear is
// the parent's — or the landing screen's — and must be interactive
// again.
func (m *UI) clearInspectState() {
	m.inspecting = nil
	m.inspectRing = nil
	m.inspectRingPos = 0
	m.inspectDispatchTargets = nil
	m.inspectSeq++
	m.inspectWindow = nil
	m.chat.SetA2UIReadOnly(false)
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
			// Live only while the result has not arrived: the status
			// field stays Running after the result lands (the result path
			// never calls SetStatus), so a finished agent would ride the
			// ring and the "(n/N)" counter forever. A canceled block is
			// never live (#405).
			live := !block.HasResult() &&
				(block.Status() == chat.ToolStatusRunning ||
					block.Status() == chat.ToolStatusAwaitingPermission)
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

// liveDispatchSessionIDs returns the session IDs of the transcript's live
// dispatch blocks in transcript order, as a set. ctrl+x is a dispatch
// cancel (#373), so plain agent-tool children never become targets even
// though they ride the same inspect ring.
func (m *UI) liveDispatchSessionIDs() map[string]bool {
	ids := make(map[string]bool, 4)
	for i := range m.chat.Len() {
		block, ok := m.chat.ItemAt(i).(*chat.DispatchToolMessageItem)
		if ok && block.IsLive() && block.DispatchSessionID() != "" {
			ids[block.DispatchSessionID()] = true
		}
	}
	return ids
}

// selectedAgentBlock reports whether the chat's currently selected item
// is a drill-in target: a dispatch block or a plain agent-tool block. It
// type-checks only the selected item — the help views are rebuilt on
// every render, so they must never walk the transcript (#412).
func (m *UI) selectedAgentBlock() bool {
	switch m.chat.ItemAt(m.chat.Selected()).(type) {
	case *chat.DispatchToolMessageItem, *chat.AgentToolMessageItem:
		return true
	default:
		return false
	}
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

// handleInspectKeys routes ctrl+], esc/ctrl+[ and ctrl+x (#314, #373,
// #404). It runs after the dialog routing in handleKeyPressMsg, so open
// dialogs keep their keys, and before every other handler, so the
// bindings work from both editor and chat focus. ctrl+] drills in, esc
// or ctrl+[ leaves inspect mode on every terminal, and ctrl+x cancels
// the dispatch being viewed (or the selected live dispatch card in the
// chat). Esc means "back" only inside inspect mode; outside it falls
// through untouched and keeps its chat-cancel meaning, as does every
// other key.
func (m *UI) handleInspectKeys(msg tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case key.Matches(msg, m.keyMap.InspectDrill):
		return true, m.handleInspectDrill()
	case key.Matches(msg, m.keyMap.InspectBack):
		if !m.isInspecting() {
			return false, nil
		}
		// Leaving from the editor drops any @ completion popup open over
		// the input: its own close key is esc, which this press no longer
		// reaches.
		if m.completionsOpen {
			m.closeCompletions()
		}
		return true, m.exitInspect()
	case key.Matches(msg, m.keyMap.CancelAgent):
		if sid := m.focusedLiveDispatchSessionID(); sid != "" {
			return true, m.cancelDispatchAgent(sid)
		}
		return false, nil
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
	return tea.Batch(m.chat.ScrollToSelected(), m.enterInspect(liveRefs[0]))
}

// enterInspect captures the parent's scroll position and live-agent ring
// (only on the transition into inspect mode) and starts the child
// transcript load. The ring position names the viewed session; when the
// session is not in the ring (a finished block, or a task session picked
// in the sessions picker) it is -1, so the first ctrl+] cycles to the
// first live agent instead of skipping it.
func (m *UI) enterInspect(ref agentBlockRef) tea.Cmd {
	if !m.isInspecting() {
		idx, line := m.chat.ScrollPosition()
		m.inspectScroll = [2]int{idx, line}
		m.inspectFollow = m.chat.Follow()
		m.inspectRing = m.liveAgentSessionIDs()
		m.inspectDispatchTargets = m.liveDispatchSessionIDs()
	}
	m.inspectRingPos = slices.Index(m.inspectRing, ref.sessionID)
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
// Each call opens a transition window targeting sessionID: events
// published for it while the fetch runs are buffered and replayed when
// the snapshot lands, closing the entry race (#406). Each call
// supersedes the loads before it: only the latest one lands, so rapid
// cycling settles on the last press and the ring position always names
// the session on screen.
func (m *UI) loadInspectSession(sessionID string) tea.Cmd {
	m.inspectSeq++
	seq := m.inspectSeq
	m.inspectWindow = &inspectWindow{seq: seq, sessionID: sessionID}
	ws := m.com.Workspace
	return func() tea.Msg {
		sess, err := ws.GetSession(context.Background(), sessionID)
		if err != nil {
			return inspectFetchFailedMsg{seq: seq, err: err}
		}
		msgs, err := ws.ListMessages(context.Background(), sessionID)
		if err != nil {
			return inspectFetchFailedMsg{seq: seq, err: err}
		}
		return inspectSessionLoadedMsg{seq: seq, sess: &sess, messages: msgs}
	}
}

// handleInspectLoaded swaps the chat over to the child transcript, then
// replays the events buffered while the fetch ran on top of it (#406).
// The active session, sidebar, prompt queue, prompt history, and every
// other active-session concern keep pointing at the parent: this
// handler only touches the chat view.
func (m *UI) handleInspectLoaded(msg inspectSessionLoadedMsg) tea.Cmd {
	if m.inspectWindow == nil || msg.seq != m.inspectWindow.seq {
		// Superseded by a later cycle, ctrl+[, or session switch.
		return nil
	}
	buffer := m.inspectWindow.events
	m.inspectWindow = nil
	m.inspecting = msg.sess

	// Liveness comes from the ring captured off the parent's blocks at
	// entry: when cycling, the chat holds the previous child, not them.
	live := slices.Contains(m.inspectRing, msg.sess.ID)

	cmd := m.setSessionMessages(msg.messages)
	// The viewed transcript is read-only (#407): a form it carries must
	// not take focus or start a turn on the parent. Applied after
	// setSessionMessages so any surface the rebuild focused is blurred.
	m.chat.SetA2UIReadOnly(true)
	if live {
		// A running child keeps its spinners going; setSessionMessages
		// gates the animation clock on the parent's busy state.
		m.chat.SetAnimationsAllowed(true)
	}
	for _, event := range buffer {
		if replayed := m.upsertSessionMessage(event); replayed != nil {
			cmd = tea.Batch(cmd, replayed)
		}
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
// Clearing the inspect state drops any child load still in flight; a
// fresh window then buffers parent events until the restore lands, so
// nothing is painted into the child transcript it is about to replace
// (#406).
func (m *UI) exitInspect() tea.Cmd {
	m.clearInspectState()
	scroll, follow := m.inspectScroll, m.inspectFollow
	parentID := m.currentSessionID()
	m.inspectSeq++
	seq := m.inspectSeq
	m.inspectWindow = &inspectWindow{seq: seq, sessionID: parentID}
	ws := m.com.Workspace
	return func() tea.Msg {
		msgs, err := ws.ListMessages(context.Background(), parentID)
		if err != nil {
			return inspectFetchFailedMsg{seq: seq, err: err}
		}
		return inspectRestoreMsg{
			seq:        seq,
			sessionID:  parentID,
			messages:   msgs,
			scrollIdx:  scroll[0],
			scrollLine: scroll[1],
			follow:     follow,
		}
	}
}

// handleInspectRestore swaps the parent transcript back in, replays
// the parent events buffered while the fetch ran on top of it (#406),
// and restores the captured scroll state: a parent that was following
// the stream comes back at the bottom, still following; otherwise the
// captured position, clamped to whatever the reloaded transcript can
// honor. A restore that lands after a re-entry, or after the active
// session changed, would paint the wrong transcript and is dropped.
func (m *UI) handleInspectRestore(msg inspectRestoreMsg) tea.Cmd {
	if m.isInspecting() || msg.sessionID != m.currentSessionID() {
		return nil
	}
	if m.inspectWindow == nil || msg.seq != m.inspectWindow.seq {
		return nil
	}
	buffer := m.inspectWindow.events
	m.inspectWindow = nil
	cmds := []tea.Cmd{m.setSessionMessages(msg.messages)}
	for _, event := range buffer {
		if cmd := m.upsertSessionMessage(event); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
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

// upsertSessionMessage applies one message event buffered during an
// inspect transition on top of the snapshot that just landed (#406): a
// create for a message the snapshot already carries updates it, an
// update for one it never saw appends it, and a delete removes it. It
// reuses the live session-message pipeline so replayed messages render
// exactly like streamed ones.
func (m *UI) upsertSessionMessage(event pubsub.Event[message.Message]) tea.Cmd {
	present := m.chat.MessageItem(event.Payload.ID) != nil
	switch event.Type {
	case pubsub.CreatedEvent:
		if present {
			return m.updateSessionMessage(event.Payload)
		}
		return m.appendSessionMessage(event.Payload)
	case pubsub.UpdatedEvent:
		if present {
			return m.updateSessionMessage(event.Payload)
		}
		return m.appendSessionMessage(event.Payload)
	case pubsub.DeletedEvent:
		m.chat.RemoveMessage(event.Payload.ID)
	}
	return nil
}

// handleInspectFetchFailed closes the pending transition window on a
// failed fetch, so later events apply normally and the next drill-in
// starts clean. A superseded failure is dropped with its transition.
func (m *UI) handleInspectFetchFailed(msg inspectFetchFailedMsg) tea.Cmd {
	if m.inspectWindow == nil || msg.seq != m.inspectWindow.seq {
		return nil
	}
	m.inspectWindow = nil
	return util.ReportError(msg.err)
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
	if len(m.inspectRing) > 1 && m.inspectRingPos >= 0 {
		pos = fmt.Sprintf(" (%d/%d)", m.inspectRingPos+1, len(m.inspectRing))
	}
	const prefix = "Inspecting "
	suffix := pos + " · esc returns · prompts go to the parent"
	width := m.textarea.Width() - 1
	if width <= 0 {
		width = inspectPlaceholderWidth
	}
	room := max(1, width-ansi.StringWidth(prefix)-ansi.StringWidth(suffix))
	text := prefix + ansi.Truncate(title, room, "…") + suffix
	return ansi.Truncate(text, width, "…")
}
