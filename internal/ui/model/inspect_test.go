package model

// Tests for inspect mode (#314): the viewed-vs-active session split, the
// drill-in/back navigation keys, live-following of a child session, and
// the rule that a task session can never become the active session.
//
// Assertions are state-based (chat item counts, recorded workspace
// calls), never render-string comparisons, so they hold on Windows
// terminals too.

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/scheduler"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/attachments"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/crush/internal/ui/textarea"
	"github.com/charmbracelet/crush/internal/workspace"
)

// Deterministic agent-tool task session IDs (messageID$$toolCallID).
const (
	inspectParentID  = "parent-1"
	inspectChildID   = "msg-1$$call-1"
	inspectChild2ID  = "msg-1$$call-2"
	inspectMessageID = "msg-1"
	inspectCallID    = "call-1"
	inspectCall2ID   = "call-2"
)

// inspectWorkspace records the workspace calls inspect mode makes so
// tests can pin which session each call targeted. Unimplemented methods
// panic through the embedded interface, exactly like countingWorkspace.
type inspectWorkspace struct {
	workspace.Workspace

	sessions map[string]session.Session
	messages map[string][]message.Message

	getSessions  []string
	listMessages []string
	agentRuns    []string
}

func newInspectWorkspace() *inspectWorkspace {
	return &inspectWorkspace{
		sessions: map[string]session.Session{},
		messages: map[string][]message.Message{},
	}
}

func (w *inspectWorkspace) GetSession(_ context.Context, id string) (session.Session, error) {
	w.getSessions = append(w.getSessions, id)
	sess, ok := w.sessions[id]
	if !ok {
		return session.Session{}, fmt.Errorf("session %q not found", id)
	}
	return sess, nil
}

func (w *inspectWorkspace) ListMessages(_ context.Context, id string) ([]message.Message, error) {
	w.listMessages = append(w.listMessages, id)
	return w.messages[id], nil
}

func (w *inspectWorkspace) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func (w *inspectWorkspace) ListAllUserMessages(context.Context) ([]message.Message, error) {
	return nil, nil
}

func (w *inspectWorkspace) ListSessionHistory(context.Context, string) ([]history.File, error) {
	return nil, nil
}

func (w *inspectWorkspace) FileTrackerListReadFiles(context.Context, string) ([]string, error) {
	return nil, nil
}

func (w *inspectWorkspace) SetCurrentSession(context.Context, string) error { return nil }

func (w *inspectWorkspace) AgentRun(_ context.Context, sessionID, _ string, _ ...message.Attachment) error {
	w.agentRuns = append(w.agentRuns, sessionID)
	return nil
}

func (w *inspectWorkspace) AgentIsReady() bool                               { return true }
func (w *inspectWorkspace) AgentIsBusy() bool                                { return false }
func (w *inspectWorkspace) AgentIsSessionBusy(string) bool                   { return false }
func (w *inspectWorkspace) AgentReadyErr() error                             { return nil }
func (w *inspectWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *inspectWorkspace) AgentModel() workspace.AgentModel                 { return workspace.AgentModel{} }
func (w *inspectWorkspace) AgentQueuedPrompts(string) int                    { return 0 }
func (w *inspectWorkspace) AgentQueuedPromptsList(string) []string           { return nil }
func (w *inspectWorkspace) AgentListCronTasks(string) []scheduler.Task       { return nil }
func (w *inspectWorkspace) LSPStart(context.Context, string)                 {}
func (w *inspectWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }
func (w *inspectWorkspace) LSPGetDiagnosticCounts(string) lsp.DiagnosticCounts {
	return lsp.DiagnosticCounts{}
}

func (w *inspectWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	for i := 0; i+1 < len(sessionID); i++ {
		if sessionID[i] == '$' && sessionID[i+1] == '$' {
			return sessionID[:i], sessionID[i+2:], true
		}
	}
	return "", "", false
}

func (w *inspectWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return messageID + "$$" + toolCallID
}

func (w *inspectWorkspace) WorkingDir() string     { return "" }
func (w *inspectWorkspace) Config() *config.Config { return nil }

// newInspectUI builds a UI on the stub workspace with the parent session
// active, ready for inspect mode tests.
func newInspectUI(t *testing.T, ws *inspectWorkspace) *UI {
	t.Helper()
	com := common.DefaultCommon(ws)
	m := &UI{
		com:         com,
		status:      NewStatus(com, nil),
		chat:        NewChat(com, config.ScrollbarDefault),
		textarea:    textarea.New(),
		state:       uiChat,
		focus:       uiFocusEditor,
		width:       140,
		height:      45,
		keyMap:      DefaultKeyMap(),
		dialog:      dialog.NewOverlay(),
		attachments: attachments.New(nil, attachments.Keymap{}),
	}
	m.session = &session.Session{ID: inspectParentID}
	ws.sessions[inspectParentID] = *m.session
	return m
}

// addChild registers a child task session with an optional transcript.
func addChild(ws *inspectWorkspace, id, parentID, title string, msgs ...message.Message) {
	ws.sessions[id] = session.Session{
		ID:              id,
		ParentSessionID: parentID,
		Title:           title,
	}
	ws.messages[id] = msgs
}

// runInspectCmds executes a command tree the way the Bubble Tea runtime
// would, feeding inspect mode's own messages back into Update. Other
// leaf commands run for their side effects on the stub; their messages
// are dropped.
func runInspectCmds(m *UI, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runInspectCmds(m, c)
		}
	case inspectSessionLoadedMsg, inspectRestoreMsg, loadSessionMsg:
		_, next := m.Update(msg)
		runInspectCmds(m, next)
	}
}

// inspectChildMessages builds a small child transcript: the dispatch
// prompt and an assistant reply.
func inspectChildMessages() []message.Message {
	return []message.Message{
		{ID: "c1", SessionID: inspectChildID, Role: message.User},
		{ID: "c2", SessionID: inspectChildID, Role: message.Assistant},
	}
}

// addAgentBlock appends an assistant message carrying a plain agent-tool
// call into the parent transcript through the live event path, which is
// exactly how the chat builds the block.
func addAgentBlock(t *testing.T, m *UI, messageID, toolCallID string) {
	t.Helper()
	m.Update(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        messageID,
			SessionID: inspectParentID,
			Role:      message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    toolCallID,
					Name:  agent.AgentToolName,
					Input: `{"prompt":"do things"}`,
				},
			},
		},
	})
}

// TestInspectSubmitGoesToActiveSession pins the split invariant (#314):
// while the chat views a task session, a prompt submitted from the
// editor lands in the parent (the active session), never in the viewed
// task session.
func TestInspectSubmitGoesToActiveSession(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting(), "inspect mode must be active")
	require.Equal(t, inspectChildID, m.inspectingSessionID())
	require.Equal(t, inspectParentID, m.session.ID, "the parent must stay the active session")
	require.Contains(t, m.textarea.Placeholder, "Inspecting")

	// Submit a prompt through the real editor path.
	m.textarea.SetValue("keep working")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	runInspectCmds(m, cmd)

	require.Equal(t, []string{inspectParentID}, ws.agentRuns,
		"a prompt submitted while inspecting must run against the parent session")
}

// TestTaskSessionNeverBecomesActive pins the no-editor-route rule: the
// picker returning a task session opens the read-only inspect view; the
// active session is never replaced and never reloaded.
func TestTaskSessionNeverBecomesActive(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	runInspectCmds(m, m.handleSelectSession(ws.sessions[inspectChildID]))

	require.True(t, m.isInspecting(), "a task session must open in inspect mode")
	require.Equal(t, inspectParentID, m.session.ID,
		"a task session must never replace the active session")
	require.Equal(t, []string{inspectChildID}, ws.listMessages,
		"the child transcript loads for viewing; the active session is not reloaded")
	require.NotNil(t, m.chat.MessageItem("c1"),
		"the child transcript renders in the chat")
}

// addParentTranscript registers count parent messages in the stub store
// and streams them into the chat the way the live event path would.
func addParentTranscript(t *testing.T, m *UI, ws *inspectWorkspace, count int) {
	t.Helper()
	for i := range count {
		msg := message.Message{
			ID:        parentMessageID(i),
			SessionID: inspectParentID,
			Role:      message.User,
		}
		ws.messages[inspectParentID] = append(ws.messages[inspectParentID], msg)
		m.Update(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: msg})
	}
}

// TestInspectBackRestoresParentAndScroll pins ctrl+[: leaving inspect
// mode rebuilds the parent transcript and restores the scroll position
// captured on entry.
func TestInspectBackRestoresParentAndScroll(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	// A short viewport over a long transcript, scrolled to the middle, so
	// the position being restored is neither the top nor the bottom.
	m.chat.SetSize(80, 6)
	addParentTranscript(t, m, ws, 30)
	m.chat.ScrollToTop()
	m.chat.ScrollBy(7)
	wantIdx, wantLine := m.chat.ScrollPosition()
	require.NotZero(t, wantIdx+wantLine, "the test position must be off the top")
	require.False(t, m.chat.AtBottom(), "the test position must be off the bottom")

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())
	require.Nil(t, m.chat.MessageItem(parentMessageID(0)),
		"the parent transcript is off screen while inspecting")

	handled, backCmd := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	require.True(t, handled, "ctrl+[ must be consumed while inspecting")
	runInspectCmds(m, backCmd)

	require.False(t, m.isInspecting())
	gotIdx, gotLine := m.chat.ScrollPosition()
	require.Equal(t, wantIdx, gotIdx, "the parent scroll index must be restored")
	require.Equal(t, wantLine, gotLine, "the parent scroll line must be restored")
	require.NotNil(t, m.chat.MessageItem(parentMessageID(0)),
		"the parent transcript is back in the chat")
}

// TestInspectCyclesLiveAgents pins repeated ctrl+]: while inspecting,
// it cycles the live agents captured on entry, staying in inspect mode.
func TestInspectCyclesLiveAgents(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
	addChild(ws, inspectChild2ID, inspectParentID, "Agent B", inspectChildMessages()...)

	// Two live agent-tool blocks in the parent transcript give the ring
	// something to cycle.
	addAgentBlock(t, m, inspectMessageID, inspectCallID)
	addAgentBlock(t, m, inspectMessageID, inspectCall2ID)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.Equal(t, inspectChildID, m.inspectingSessionID())
	require.Equal(t, []string{inspectChildID, inspectChild2ID}, m.inspectRing,
		"the ring captures the live agents at entry")

	runInspectCmds(m, m.handleInspectDrill())
	require.True(t, m.isInspecting(), "cycling stays in inspect mode")
	require.Equal(t, inspectChild2ID, m.inspectingSessionID(),
		"ctrl+] must advance to the next live agent")

	runInspectCmds(m, m.handleInspectDrill())
	require.Equal(t, inspectChildID, m.inspectingSessionID(),
		"cycling wraps around the entry ring")
	require.Equal(t, inspectParentID, m.session.ID)
}

// TestInspectLiveFollowsChild pins live-follow: messages published by
// the viewed child session append to the chat while inspecting.
func TestInspectLiveFollowsChild(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent")

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())
	before := m.chat.Len()

	m.Update(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "live-1", SessionID: inspectChildID, Role: message.User},
	})
	require.Equal(t, before+1, m.chat.Len(), "a created child message must append to the chat")
	require.NotNil(t, m.chat.MessageItem("live-1"))

	m.Update(pubsub.Event[message.Message]{
		Type:    pubsub.UpdatedEvent,
		Payload: message.Message{ID: "live-1", SessionID: inspectChildID, Role: message.User},
	})
	require.Equal(t, before+1, m.chat.Len(), "an update must not duplicate the item")

	// Parent traffic keeps landing in the parent's state without
	// touching the chat.
	m.Update(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "parent-live", SessionID: inspectParentID, Role: message.User},
	})
	require.Equal(t, before+1, m.chat.Len(),
		"parent messages must not render while the child is on screen")
	require.Nil(t, m.chat.MessageItem("parent-live"))
}

// TestSessionSwitchClearsInspect pins that loading another session (the
// picker, a new session) tears the inspect view down with the old
// transcript instead of leaving it pointing at a session that is no
// longer on screen.
func TestSessionSwitchClearsInspect(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	next := session.Session{ID: "parent-2"}
	ws.sessions[next.ID] = next
	runInspectCmds(m, m.loadSession(next.ID))

	require.False(t, m.isInspecting(), "a session switch must clear the inspect view")
	require.Equal(t, next.ID, m.session.ID)
}

// TestInspectKeyBindings pins the binding surface: ctrl+] and ctrl+[ are
// bound, and Esc is not part of inspect mode's bindings.
func TestInspectKeyBindings(t *testing.T) {
	km := DefaultKeyMap()
	require.True(t, matchesCtrl(km.InspectDrill, ']'))
	require.True(t, matchesCtrl(km.InspectBack, '['))
	require.False(t, matchesCtrl(km.InspectDrill, '['))
	require.False(t, matchesCtrl(km.InspectBack, ']'))
	require.False(t, key.Matches(tea.KeyPressMsg{Code: tea.KeyEscape}, km.InspectDrill),
		"esc must stay out of inspect mode's bindings")
	require.False(t, key.Matches(tea.KeyPressMsg{Code: tea.KeyEscape}, km.InspectBack))
}

func matchesCtrl(b key.Binding, r rune) bool {
	return key.Matches(tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}, b)
}

func parentMessageID(i int) string {
	return "p" + strconv.Itoa(i)
}

// TestInspectEscIsBackOnLegacyTerminals pins ctrl+[ on terminals without
// key disambiguation, which send it as the same byte as esc: while
// inspecting, that esc must leave inspect mode instead of reaching the
// cancel handler, where a second press cancels the parent's run. A
// terminal that tells the two keys apart keeps esc's own meaning.
func TestInspectEscIsBackOnLegacyTerminals(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	runInspectCmds(m, cmd)
	require.False(t, m.isInspecting(),
		"ctrl+[ arrives as esc on a legacy terminal and must leave inspect mode")
	require.False(t, m.isCanceling, "the back press must not arm the parent's cancel")

	m.keyenh = tea.KeyboardEnhancementsMsg{Flags: 1}
	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())
	handled, _ := m.handleInspectKeys(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, handled, "with key disambiguation esc is not ctrl+[")
	require.True(t, m.isInspecting())
}

// TestInspectStaleLoadsAreDropped pins the async ordering of inspect
// transitions: only the latest requested view may paint the chat, in
// whichever order the fetches land.
func TestInspectStaleLoadsAreDropped(t *testing.T) {
	setup := func(t *testing.T) (*UI, *inspectWorkspace) {
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
		addChild(ws, inspectChild2ID, inspectParentID, "Agent B", inspectChildMessages()...)
		addParentTranscript(t, m, ws, 3)
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		addAgentBlock(t, m, inspectMessageID, inspectCall2ID)
		runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
		require.Equal(t, inspectChildID, m.inspectingSessionID())
		return m, ws
	}
	back := tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl}

	t.Run("cycle superseded by back, cycle lands last", func(t *testing.T) {
		m, _ := setup(t)
		cycle := m.handleInspectDrill()
		_, exit := m.handleInspectKeys(back)
		runInspectCmds(m, exit)
		runInspectCmds(m, cycle)
		require.False(t, m.isInspecting(), "a load superseded by ctrl+[ must not re-enter inspect mode")
		require.NotNil(t, m.chat.MessageItem(parentMessageID(0)))
	})

	t.Run("cycle superseded by back, cycle lands first", func(t *testing.T) {
		m, _ := setup(t)
		cycle := m.handleInspectDrill()
		_, exit := m.handleInspectKeys(back)
		runInspectCmds(m, cycle)
		runInspectCmds(m, exit)
		require.False(t, m.isInspecting(),
			"the inspect state must match the parent transcript on screen")
		require.NotNil(t, m.chat.MessageItem(parentMessageID(0)))
	})

	t.Run("rapid cycling settles on the last press", func(t *testing.T) {
		m, _ := setup(t)
		first := m.handleInspectDrill()
		second := m.handleInspectDrill()
		runInspectCmds(m, first)
		runInspectCmds(m, second)
		require.Equal(t, m.inspectRing[m.inspectRingPos], m.inspectingSessionID(),
			"the viewed session must be the ring position the indicator shows")
		require.Equal(t, inspectChildID, m.inspectingSessionID())
	})

	t.Run("restore after a session switch is dropped", func(t *testing.T) {
		m, ws := setup(t)
		_, exit := m.handleInspectKeys(back)
		next := session.Session{ID: "parent-2"}
		ws.sessions[next.ID] = next
		ws.messages[next.ID] = []message.Message{{ID: "n0", SessionID: next.ID, Role: message.User}}
		runInspectCmds(m, m.loadSession(next.ID))
		runInspectCmds(m, exit)
		require.NotNil(t, m.chat.MessageItem("n0"))
		require.Nil(t, m.chat.MessageItem(parentMessageID(0)),
			"a restore of the old parent must not paint over the new session")
	})
}

// TestInspectFailedLoadDoesNotWedge pins that a child fetch that fails
// leaves drill-in usable: the next ctrl+] still loads.
func TestInspectFailedLoadDoesNotWedge(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: "missing$$call"}))
	require.False(t, m.isInspecting())

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting(), "a failed load must not block the next drill-in")
}

// TestInspectBackResumesFollow pins that a parent which was following the
// stream at entry comes back following, at the bottom, including messages
// that landed while it was off screen.
func TestInspectBackResumesFollow(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	m.chat.SetSize(80, 6)
	addParentTranscript(t, m, ws, 20)
	m.chat.ScrollToBottom()
	require.True(t, m.chat.Follow())

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	// The parent keeps streaming while the child is on screen.
	for i := 20; i < 30; i++ {
		ws.messages[inspectParentID] = append(ws.messages[inspectParentID], message.Message{
			ID: parentMessageID(i), SessionID: inspectParentID, Role: message.User,
		})
	}
	_, exit := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	runInspectCmds(m, exit)

	require.True(t, m.chat.AtBottom(), "a following parent must come back at the bottom")
	require.True(t, m.chat.Follow(), "and keep following the stream")
}

// TestPickerTaskSessionOpensUnderItsParent pins the picker route for a
// task session whose parent is not the active session (another session,
// or none loaded yet): the parent loads as the active session first, so
// prompts land where the transcript came from, then the child opens in
// inspect mode. The task session itself never becomes active.
func TestPickerTaskSessionOpensUnderItsParent(t *testing.T) {
	const otherParentID, otherChildID = "parent-2", "msg-9$$call-9"
	for name, landing := range map[string]bool{"from another session": false, "from landing": true} {
		t.Run(name, func(t *testing.T) {
			ws := newInspectWorkspace()
			m := newInspectUI(t, ws)
			if landing {
				m.session = nil
				m.state = uiLanding
			}
			ws.sessions[otherParentID] = session.Session{ID: otherParentID}
			ws.messages[otherParentID] = []message.Message{{ID: "o0", SessionID: otherParentID, Role: message.User}}
			addChild(ws, otherChildID, otherParentID, "Other Agent", inspectChildMessages()...)

			runInspectCmds(m, m.handleSelectSession(ws.sessions[otherChildID]))

			require.NotNil(t, m.session)
			require.Equal(t, otherParentID, m.session.ID,
				"the task session's parent must become the active session")
			require.Equal(t, uiChat, m.state)
			require.True(t, m.isInspecting())
			require.Equal(t, otherChildID, m.inspectingSessionID())
		})
	}
}
