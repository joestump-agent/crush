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
	"reflect"
	"strconv"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
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
	"github.com/charmbracelet/crush/internal/ui/util"
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

	getSessions      []string
	listMessages     []string
	agentRuns        []string
	agentCancels     []string
	agentClearQueues []string
	listFailures     map[string]bool
}

func newInspectWorkspace() *inspectWorkspace {
	return &inspectWorkspace{
		sessions:     map[string]session.Session{},
		messages:     map[string][]message.Message{},
		listFailures: map[string]bool{},
	}
}

// AgentTask answers the inspect view's per-load handle lookup (#415):
// unknown sessions report not-found so the placeholder falls back to the
// child session's title.
func (w *inspectWorkspace) AgentTask(_ string) (workspace.AgentTask, bool) {
	return workspace.AgentTask{}, false
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
	if w.listFailures[id] {
		return nil, fmt.Errorf("list %q failed", id)
	}
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

func (w *inspectWorkspace) AgentCancel(sessionID string) {
	w.agentCancels = append(w.agentCancels, sessionID)
}

func (w *inspectWorkspace) AgentClearQueue(sessionID string) {
	w.agentClearQueues = append(w.agentClearQueues, sessionID)
}

func (w *inspectWorkspace) AgentIsReady() bool { return true }

// MCPGetStates satisfies the session-load MCP refresh upstream schedules
// (requestMCPRefresh); inspect tests have no MCP servers.
func (w *inspectWorkspace) MCPGetStates() map[string]mcp.ClientInfo          { return nil }
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
	case inspectSessionLoadedMsg, inspectRestoreMsg, inspectFetchFailedMsg, loadSessionMsg:
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

// deliverAgentResult delivers the tool result for the named call the way
// the live event path does: a Tool-role message created in the parent
// session, which routes to SetResult on the existing block.
func deliverAgentResult(t *testing.T, m *UI, toolCallID, content string) {
	t.Helper()
	m.Update(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID:        "toolresult-" + toolCallID,
			SessionID: inspectParentID,
			Role:      message.Tool,
			Parts: []message.ContentPart{
				message.ToolResult{ToolCallID: toolCallID, Name: agent.AgentToolName, Content: content},
			},
		},
	})
}

// requireBlockLive asserts the inspect liveness recorded for the block
// whose child session has the given ID.
func requireBlockLive(t *testing.T, m *UI, sessionID string, live bool, msgAndArgs ...any) {
	t.Helper()
	for _, ref := range m.agentBlocks() {
		if ref.sessionID == sessionID {
			require.Equal(t, live, ref.live, append([]any{"block %s liveness", sessionID}, msgAndArgs...)...)
			return
		}
	}
	t.Fatalf("no agent block for session %q", sessionID)
}

// TestInspectEditorReadOnly pins the read-only editor (#415): while the
// chat views a task session, the editor neither edits nor submits — an
// Enter press runs nothing, and typed keys reach no textarea, because a
// prompt would land in the parent the user cannot see. Leaving inspect
// mode restores the normal editor.
func TestInspectEditorReadOnly(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting(), "inspect mode must be active")
	require.Equal(t, inspectChildID, m.inspectingSessionID())
	require.Equal(t, inspectParentID, m.session.ID, "the parent must stay the active session")
	require.Contains(t, m.textarea.Placeholder, "Inspecting")

	// Enter submits nothing while inspecting.
	m.textarea.SetValue("keep working")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	runInspectCmds(m, cmd)
	require.Empty(t, ws.agentRuns,
		"a prompt submitted while inspecting must run nowhere: the editor is read-only")

	// Typing reaches no textarea: the draft the parent would inherit
	// stays whatever it was before the drill-in.
	m.textarea.SetValue("")
	m.Update(tea.KeyPressMsg{Code: 'h'})
	m.Update(tea.KeyPressMsg{Code: 'i'})
	require.Empty(t, m.textarea.Value(), "typed keys must not edit the draft while inspecting")

	// Leaving inspect mode restores the normal editor.
	runInspectCmds(m, m.exitInspect())
	require.False(t, m.isInspecting())
	m.textarea.SetValue("keep working")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	runInspectCmds(m, cmd)
	require.Equal(t, []string{inspectParentID}, ws.agentRuns,
		"after leaving inspect mode a submitted prompt runs against the parent session")
}

// TestInspectTabStillMovesFocus pins the one editor-focus key the
// read-only swallow (#415) must let through: Tab moves focus to the
// chat, which is the only keyboard way to scroll the inspect
// transcript when the drill-in started from the editor.
func TestInspectTabStillMovesFocus(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	require.Equal(t, uiFocusEditor, m.focus)
	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, uiFocusMain, m.focus,
		"Tab must still move focus to the chat while inspecting")
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

// TestInspectFirstCycleLandsOnFirstAgent pins the outside-ring entry
// (#416): a session that is not in the live ring (a finished agent
// block, or a task session picked in the sessions picker) enters
// inspect mode at ring position -1, so the first ctrl+] shows the
// first live agent instead of skipping it, and the placeholder claims
// no ring position until the view actually lands in the ring.
func TestInspectFirstCycleLandsOnFirstAgent(t *testing.T) {
	const (
		childA = "msg-1$$call-1"
		childB = "msg-1$$call-2"
		childC = "msg-2$$call-1"
		done   = "done-1"
	)
	setup := func(t *testing.T) (*UI, *inspectWorkspace) {
		t.Helper()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		addChild(ws, childA, inspectParentID, "Agent A", inspectChildMessages()...)
		addChild(ws, childB, inspectParentID, "Agent B", inspectChildMessages()...)
		addChild(ws, childC, inspectParentID, "Agent C", inspectChildMessages()...)
		addChild(ws, done, inspectParentID, "Finished Agent", inspectChildMessages()...)

		// Three live agent blocks: the ring is [A, B, C].
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		addAgentBlock(t, m, inspectMessageID, inspectCall2ID)
		addAgentBlock(t, m, "msg-2", inspectCallID)
		return m, ws
	}

	t.Run("entered outside the ring, the first ctrl+] shows the first live agent", func(t *testing.T) {
		m, _ := setup(t)
		m.textarea.SetWidth(100)

		runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: done}))
		require.True(t, m.isInspecting())
		require.Equal(t, done, m.inspectingSessionID())
		require.Equal(t, []string{childA, childB, childC}, m.inspectRing,
			"the ring captures the live agents at entry")
		require.Equal(t, -1, m.inspectRingPos,
			"a viewed session outside the ring must sit at ring position -1")

		// No position while the viewed session is outside the ring.
		require.NotContains(t, m.inspectPlaceholder(), "/3")

		runInspectCmds(m, m.handleInspectDrill())
		require.Equal(t, childA, m.inspectingSessionID(),
			"the first ctrl+] must land on the first live agent")
		require.Contains(t, m.inspectPlaceholder(), " (1/3)",
			"the placeholder shows the ring position once the view is in the ring")

		runInspectCmds(m, m.handleInspectDrill())
		require.Equal(t, childB, m.inspectingSessionID(),
			"the second ctrl+] must land on the second live agent")

		runInspectCmds(m, m.handleInspectDrill())
		require.Equal(t, childC, m.inspectingSessionID(),
			"the third ctrl+] must land on the third live agent")

		runInspectCmds(m, m.handleInspectDrill())
		require.Equal(t, childA, m.inspectingSessionID(),
			"cycling wraps around the entry ring")
	})

	t.Run("entered on a session in the ring, the first ctrl+] advances past it", func(t *testing.T) {
		m, _ := setup(t)

		runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: childB}))
		require.Equal(t, 1, m.inspectRingPos,
			"entry on a ring session must keep that session's position")
		require.Equal(t, childB, m.inspectingSessionID())

		runInspectCmds(m, m.handleInspectDrill())
		require.Equal(t, childC, m.inspectingSessionID(),
			"the first ctrl+] must advance past the entered session")
	})
}

// TestInspectSingleEntryRingFromOutside pins the degenerate ring (#416):
// a ring with one live agent, entered from a session outside it, reaches
// that agent on the first ctrl+].
func TestInspectSingleEntryRingFromOutside(t *testing.T) {
	const done = "done-2"
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
	addChild(ws, done, inspectParentID, "Finished Agent", inspectChildMessages()...)

	addAgentBlock(t, m, inspectMessageID, inspectCallID)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: done}))
	require.Equal(t, []string{inspectChildID}, m.inspectRing)
	require.Equal(t, -1, m.inspectRingPos)

	runInspectCmds(m, m.handleInspectDrill())
	require.Equal(t, inspectChildID, m.inspectingSessionID(),
		"the single ring entry must be reachable on the first ctrl+]")
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

// TestInspectKeyBindings pins the binding surface: ctrl+] drills in,
// esc and ctrl+[ go back, and esc is not part of the drill-in binding.
func TestInspectKeyBindings(t *testing.T) {
	km := DefaultKeyMap()
	require.True(t, matchesCtrl(km.InspectDrill, ']'))
	require.True(t, matchesCtrl(km.InspectBack, '['))
	require.False(t, matchesCtrl(km.InspectDrill, '['))
	require.False(t, matchesCtrl(km.InspectBack, ']'))
	require.False(t, key.Matches(tea.KeyPressMsg{Code: tea.KeyEscape}, km.InspectDrill),
		"esc must stay out of inspect mode's drill-in binding")
	require.True(t, key.Matches(tea.KeyPressMsg{Code: tea.KeyEscape}, km.InspectBack),
		"esc is the back key on every terminal")
}

func matchesCtrl(b key.Binding, r rune) bool {
	return key.Matches(tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}, b)
}

func parentMessageID(i int) string {
	return "p" + strconv.Itoa(i)
}

// TestInspectEscAlwaysLeaves pins the #404 decision: esc and ctrl+[
// leave inspect mode on one press, on every terminal, whatever the
// terminal reports for key disambiguation and whatever the parent's
// queue holds, and the press that leaves never cancels or clears the
// parent.
func TestInspectEscAlwaysLeaves(t *testing.T) {
	sequences := []struct {
		name string
		raw  string
	}{
		{"legacy esc", "\x1b"},
		{"kitty esc", "\x1b[27u"},
		{"kitty ctrl+[", "\x1b[91;5u"},
		{"otherkeys ctrl+[", "\x1b[27;5;91~"},
	}
	keyMsg := func(t *testing.T, raw string) tea.KeyPressMsg {
		t.Helper()
		var decoder uv.EventDecoder
		n, ev := decoder.Decode([]byte(raw))
		require.Equal(t, len(raw), n, "decoding %q", raw)
		kpe, ok := ev.(uv.KeyPressEvent)
		require.True(t, ok, "decoding %q must yield a key press", raw)
		return tea.KeyPressMsg(kpe)
	}
	setup := func(t *testing.T, flags, promptQueue int) (*UI, *inspectWorkspace) {
		t.Helper()
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.keyenh = tea.KeyboardEnhancementsMsg{Flags: flags}
		m.agentReady = true
		m.agentBusyCache.val = true
		m.promptQueue = promptQueue
		addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
		runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
		require.True(t, m.isInspecting())
		return m, ws
	}

	for _, flags := range []int{0, 1} {
		for _, promptQueue := range []int{0, 2} {
			for _, seq := range sequences {
				t.Run(fmt.Sprintf("flags=%d queue=%d %s", flags, promptQueue, seq.name), func(t *testing.T) {
					m, ws := setup(t, flags, promptQueue)

					_, cmd := m.Update(keyMsg(t, seq.raw))
					runInspectCmds(m, cmd)
					require.False(t, m.isInspecting(),
						"one press of %s must leave inspect mode", seq.name)
					require.Empty(t, ws.agentCancels, "a back press must never cancel the parent")
					require.Empty(t, ws.agentClearQueues, "a back press must never clear the parent's queue")
					require.False(t, m.isCanceling, "a back press must not arm the parent's cancel")
				})
			}
		}
	}

	t.Run("alt+esc never cancels the parent from inspect mode", func(t *testing.T) {
		m, ws := setup(t, 0, 2)

		for i := 0; i < 3; i++ {
			_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModAlt})
			runInspectCmds(m, cmd)
		}
		require.True(t, m.isInspecting(), "alt+esc is not the back key")
		require.Empty(t, ws.agentCancels, "alt+esc must never cancel the parent")
		require.Empty(t, ws.agentClearQueues, "alt+esc must never clear the parent's queue")
		require.False(t, m.isCanceling, "alt+esc must not arm the parent's cancel")
	})

	t.Run("outside inspect mode esc still arms the cancel", func(t *testing.T) {
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		m.agentReady = true
		m.agentBusyCache.val = true

		_, cmd := m.Update(keyMsg(t, "\x1b"))
		runInspectCmds(m, cmd)
		require.False(t, m.isInspecting())
		require.True(t, m.isCanceling, "outside inspect mode the first esc must arm the cancel")
		require.Empty(t, ws.agentCancels, "arming is not canceling")
	})
}

// escCloseDialog is the minimal dialog that closes on esc, for pinning
// key routing order.
type escCloseDialog struct{ closed bool }

func (d *escCloseDialog) ID() string { return "test-esc-close" }

func (d *escCloseDialog) HandleMsg(msg tea.Msg) dialog.Action {
	if msg, ok := msg.(tea.KeyPressMsg); ok && key.Matches(msg, dialog.CloseKey) {
		d.closed = true
		return dialog.ActionClose{}
	}
	return nil
}

func (d *escCloseDialog) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor { return nil }

// TestDialogKeepsEscOverInspect pins the routing order (#404): an open
// dialog receives esc before inspect mode does, so closing the dialog
// leaves the inspect view in place.
func TestDialogKeepsEscOverInspect(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	d := &escCloseDialog{}
	m.dialog.OpenDialog(d)
	require.True(t, m.dialog.HasDialogs())

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	runInspectCmds(m, cmd)
	require.True(t, d.closed, "the open dialog must take esc first")
	require.False(t, m.dialog.HasDialogs())
	require.True(t, m.isInspecting(), "closing the dialog must not leave inspect mode")
	require.False(t, m.isCanceling)
}

// TestInspectHidesEscCancelHelp pins the help bar (#404): while
// inspecting, the esc cancel/clear-queue entry is hidden even when the
// parent is busy, and it comes back once inspect mode is left.
func TestInspectHidesEscCancelHelp(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	m.agentReady = true
	m.agentBusyCache.val = true
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	require.NotContains(t, m.ShortHelp(), m.keyMap.Chat.Cancel,
		"the esc cancel entry must be hidden while inspecting")
	require.False(t, fullHelpHasBinding(m, m.keyMap.Chat.Cancel),
		"the esc cancel entry must be hidden from the full help while inspecting")

	runInspectCmds(m, m.exitInspect())
	require.False(t, m.isInspecting())
	require.Contains(t, m.ShortHelp(), m.keyMap.Chat.Cancel,
		"the esc cancel entry returns once inspect mode is left")
	require.True(t, fullHelpHasBinding(m, m.keyMap.Chat.Cancel),
		"the esc cancel entry returns to the full help once inspect mode is left")
}

func fullHelpHasBinding(m *UI, b key.Binding) bool {
	for _, row := range m.FullHelp() {
		for _, got := range row {
			if reflect.DeepEqual(got, b) {
				return true
			}
		}
	}
	return false
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

// TestInspectPlaceholderKeepsTheWayBack pins the editor hint: a long
// child title is what gets truncated, never the esc hint.
func TestInspectPlaceholderKeepsTheWayBack(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	m.textarea.SetWidth(100)
	long := strings.Repeat("Fix message queueing when todos exist ", 4)
	addChild(ws, inspectChildID, inspectParentID, long, inspectChildMessages()...)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	placeholder := m.inspectPlaceholder()
	require.Contains(t, placeholder, "esc returns")
	require.LessOrEqual(t, ansi.StringWidth(placeholder), m.textarea.Width())
}

// TestInitialTaskSessionOpensParentInInspect pins the crush -s routing
// (#413): starting on the landing screen with a task session ID loads
// the parent as the active session and opens the task session in the
// read-only inspect view, whose editor submits nothing (#415).
func TestInitialTaskSessionOpensParentInInspect(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	// A fresh start: landing state, no active session, the task
	// session as the initial one, as crush -s <task-session-id> does.
	m.session = nil
	m.state = uiLanding
	m.initialSessionID = inspectChildID

	runInspectCmds(m, m.loadInitialSession())

	require.Equal(t, inspectParentID, m.session.ID,
		"the parent must become the active session, never the task session")
	require.Equal(t, uiChat, m.state)
	require.True(t, m.isInspecting(), "the task session must open in inspect mode")
	require.Equal(t, inspectChildID, m.inspectingSessionID())

	// The inspect view's editor is read-only: an Enter press runs
	// nothing until the user leaves the view.
	m.textarea.SetValue("keep working")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	runInspectCmds(m, cmd)

	require.Empty(t, ws.agentRuns,
		"a prompt submitted from the inspect view must run nowhere: the editor is read-only")
}

// TestInitialTopLevelSessionLoadsActive pins the unchanged -s behavior:
// a top-level session ID still loads as the active session.
func TestInitialTopLevelSessionLoadsActive(t *testing.T) {
	const otherID = "other-top"
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	ws.sessions[otherID] = session.Session{ID: otherID, Title: "Other"}
	ws.messages[otherID] = []message.Message{{ID: "t0", SessionID: otherID, Role: message.User}}

	m.session = nil
	m.state = uiLanding
	m.initialSessionID = otherID

	runInspectCmds(m, m.loadInitialSession())

	require.Equal(t, otherID, m.session.ID, "a top-level session loads as active")
	require.Equal(t, uiChat, m.state)
	require.False(t, m.isInspecting())
}

// TestFinishedAgentBlockIsNotLive pins #405: an agent-tool block whose
// result arrived is not live, on either path that can deliver a result:
// the live event stream, or a transcript loaded from history.
func TestFinishedAgentBlockIsNotLive(t *testing.T) {
	t.Run("live result event", func(t *testing.T) {
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
		addAgentBlock(t, m, inspectMessageID, inspectCallID)
		requireBlockLive(t, m, inspectChildID, true)

		deliverAgentResult(t, m, inspectCallID, "done")

		requireBlockLive(t, m, inspectChildID, false,
			"a finished block must drop out of the live set")
		require.Empty(t, m.liveAgentSessionIDs(),
			"no live agents remain once the result lands")

		// ctrl+] with nothing selected must now report instead of
		// opening the oldest, long-finished block.
		m.chat.SetSelected(-1)
		cmd := m.handleInspectDrill()
		require.NotNil(t, cmd)
		info, ok := cmd().(util.InfoMsg)
		require.True(t, ok, "the report is an info toast")
		require.Equal(t, "No live sub-agents to inspect", info.Msg)
	})

	t.Run("history transcript", func(t *testing.T) {
		ws := newInspectWorkspace()
		m := newInspectUI(t, ws)
		addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
		runInspectCmds(m, m.setSessionMessages([]message.Message{
			{
				ID:        inspectMessageID,
				SessionID: inspectParentID,
				Role:      message.Assistant,
				Parts: []message.ContentPart{
					message.ToolCall{
						ID:    inspectCallID,
						Name:  agent.AgentToolName,
						Input: `{"prompt":"do things"}`,
					},
				},
			},
			{
				ID:        "toolresult-" + inspectCallID,
				SessionID: inspectParentID,
				Role:      message.Tool,
				Parts: []message.ContentPart{
					message.ToolResult{ToolCallID: inspectCallID, Name: agent.AgentToolName, Content: "done"},
				},
			},
		}))

		requireBlockLive(t, m, inspectChildID, false,
			"a block rebuilt from a loaded transcript is finished too")
	})
}

// TestCanceledAgentBlockIsNotLive pins that a block from a canceled turn
// is never live, whether or not a result ever arrived.
func TestCanceledAgentBlockIsNotLive(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
	runInspectCmds(m, m.setSessionMessages([]message.Message{
		{
			ID:        inspectMessageID,
			SessionID: inspectParentID,
			Role:      message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{
					ID:    inspectCallID,
					Name:  agent.AgentToolName,
					Input: `{"prompt":"do things"}`,
				},
				message.Finish{Reason: message.FinishReasonCanceled},
			},
		},
	}))

	requireBlockLive(t, m, inspectChildID, false, "a canceled block is never live")
}

// TestCtrlDrillSkipsFinishedBlocks pins the selection path: with one
// finished and one running agent block and nothing selected, ctrl+]
// opens the running one and the ring holds only it.
func TestCtrlDrillSkipsFinishedBlocks(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
	addChild(ws, inspectChild2ID, inspectParentID, "Agent B", inspectChildMessages()...)

	addAgentBlock(t, m, inspectMessageID, inspectCallID)
	addAgentBlock(t, m, inspectMessageID, inspectCall2ID)
	deliverAgentResult(t, m, inspectCallID, "done")

	m.chat.SetSelected(-1)
	runInspectCmds(m, m.handleInspectDrill())

	require.True(t, m.isInspecting())
	require.Equal(t, inspectChild2ID, m.inspectingSessionID(),
		"the running block must be the one inspected")
	require.Equal(t, []string{inspectChild2ID}, m.inspectRing,
		"the finished block must not ride the ring")
}

// TestFocusedFinishedBlockStillOpens pins that a finished block drops
// out of the live ring, but focusing it and pressing ctrl+] still opens
// its read-only transcript.
func TestFocusedFinishedBlockStillOpens(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Agent A", inspectChildMessages()...)
	addAgentBlock(t, m, inspectMessageID, inspectCallID)
	deliverAgentResult(t, m, inspectCallID, "done")

	idx := -1
	for _, ref := range m.agentBlocks() {
		if ref.sessionID == inspectChildID {
			idx = ref.index
		}
	}
	require.NotEqual(t, -1, idx, "the finished block is still a drill-in target")
	m.chat.SetSelected(idx)

	runInspectCmds(m, m.handleInspectDrill())

	require.True(t, m.isInspecting())
	require.Equal(t, inspectChildID, m.inspectingSessionID(),
		"focusing a finished block must still open its transcript")
}

// TestInspectExitBuffersLateParentCreate pins the exit half of the
// snapshot race (#406): a parent message created after the exit's
// snapshot read and before the restore lands must not paint into the
// child transcript still on screen, and must survive the restore.
func TestInspectExitBuffersLateParentCreate(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	addParentTranscript(t, m, ws, 3)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	_, exit := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	restore, ok := exit().(inspectRestoreMsg)
	require.True(t, ok)

	late := message.Message{ID: "late", SessionID: inspectParentID, Role: message.User}
	m.Update(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: late})
	require.Nil(t, m.chat.MessageItem("late"),
		"a parent message created during the exit window must not paint into the child view")

	_, next := m.Update(restore)
	runInspectCmds(m, next)
	require.False(t, m.isInspecting())
	require.NotNil(t, m.chat.MessageItem("late"),
		"a parent message created after the snapshot read must survive the restore")
	require.Equal(t, 4, m.chat.Len(),
		"the restored transcript must hold the snapshot plus the late message exactly once")

	m.Update(pubsub.Event[message.Message]{Type: pubsub.UpdatedEvent, Payload: late})
	require.Equal(t, 4, m.chat.Len(), "a later update must not duplicate the late message")
}

// TestInspectExitReplaysUpdateForUnknownMessage pins the update-only
// interleaving (#406): an UpdatedEvent for a message the restored
// snapshot never carried appends it instead of dropping it.
func TestInspectExitReplaysUpdateForUnknownMessage(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	addParentTranscript(t, m, ws, 3)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	_, exit := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	restore, ok := exit().(inspectRestoreMsg)
	require.True(t, ok)

	upd := message.Message{ID: "p9", SessionID: inspectParentID, Role: message.User}
	m.Update(pubsub.Event[message.Message]{Type: pubsub.UpdatedEvent, Payload: upd})
	require.Nil(t, m.chat.MessageItem("p9"))

	_, next := m.Update(restore)
	runInspectCmds(m, next)
	require.NotNil(t, m.chat.MessageItem("p9"),
		"an update for a message the snapshot missed must append it")
	require.Equal(t, 4, m.chat.Len())
}

// TestInspectEntryBuffersLateChildCreate pins the entry half of the
// snapshot race (#406): a child message created after the load's
// snapshot read and before it lands is replayed onto the inspected view.
func TestInspectEntryBuffersLateChildCreate(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	load := m.enterInspect(agentBlockRef{sessionID: inspectChildID})
	loaded, ok := load().(inspectSessionLoadedMsg)
	require.True(t, ok)

	clate := message.Message{ID: "clate", SessionID: inspectChildID, Role: message.User}
	m.Update(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: clate})
	require.Nil(t, m.chat.MessageItem("clate"),
		"a child message created before the load lands must not paint yet")
	require.False(t, m.isInspecting())

	_, next := m.Update(loaded)
	runInspectCmds(m, next)
	require.True(t, m.isInspecting())
	require.NotNil(t, m.chat.MessageItem("clate"),
		"the late child message must be replayed onto the loaded transcript")
	require.NotNil(t, m.chat.MessageItem("c1"))
	require.Equal(t, 3, m.chat.Len(),
		"the inspected view must hold the snapshot plus the late message exactly once")
}

// TestInspectReplayRendersSnapshotMessagesOnce pins the dedup rule
// (#406): a message that is both in the snapshot and in the buffer
// renders exactly once.
func TestInspectReplayRendersSnapshotMessagesOnce(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)

	load := m.enterInspect(agentBlockRef{sessionID: inspectChildID})
	loaded, ok := load().(inspectSessionLoadedMsg)
	require.True(t, ok)

	c1 := message.Message{ID: "c1", SessionID: inspectChildID, Role: message.User}
	m.Update(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: c1})

	_, next := m.Update(loaded)
	runInspectCmds(m, next)
	require.True(t, m.isInspecting())
	require.NotNil(t, m.chat.MessageItem("c1"))
	require.Equal(t, 2, m.chat.Len(),
		"a message both in the snapshot and the buffer must render exactly once")
}

// TestInspectSupersededTransitionDiscardsBuffer pins the supersession
// rule (#406): a transition replaced by a later one drops its buffered
// events and its snapshot, and the later transition owns the chat.
func TestInspectSupersededTransitionDiscardsBuffer(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	addParentTranscript(t, m, ws, 3)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting())

	_, exit := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	staleRestore, ok := exit().(inspectRestoreMsg)
	require.True(t, ok)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting(), "re-entering supersedes the exit window")

	late := message.Message{ID: "late", SessionID: inspectParentID, Role: message.User}
	m.Update(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: late})
	require.Nil(t, m.chat.MessageItem("late"),
		"parent events must not paint into the re-entered child view")

	_, next := m.Update(staleRestore)
	runInspectCmds(m, next)
	require.True(t, m.isInspecting(), "a restore superseded by re-entry must be dropped")
	require.Nil(t, m.chat.MessageItem(parentMessageID(0)),
		"the superseded restore must not swap the parent transcript in")
	require.NotNil(t, m.chat.MessageItem("c1"), "the child view stays")
}

// TestInspectFailedFetchClearsPendingWindow pins the failure rule
// (#406): a failed fetch closes the pending window, so later events
// apply normally and the next transition starts clean.
func TestInspectFailedFetchClearsPendingWindow(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	addParentTranscript(t, m, ws, 3)

	load := m.enterInspect(agentBlockRef{sessionID: "missing$$call"})
	failed, ok := load().(inspectFetchFailedMsg)
	require.True(t, ok)
	_, next := m.Update(failed)
	runInspectCmds(m, next)
	require.Nil(t, m.inspectWindow, "a failed fetch must clear the pending window")

	after := message.Message{ID: "after", SessionID: inspectParentID, Role: message.User}
	m.Update(pubsub.Event[message.Message]{Type: pubsub.CreatedEvent, Payload: after})
	require.NotNil(t, m.chat.MessageItem("after"),
		"events must apply normally once the window is closed")
}

// TestInspectFailedExitClearsPendingWindow pins the same rule for the
// exit transition: a failed restore fetch clears the window, and a
// later exit still restores the parent transcript.
func TestInspectFailedExitClearsPendingWindow(t *testing.T) {
	ws := newInspectWorkspace()
	m := newInspectUI(t, ws)
	addChild(ws, inspectChildID, inspectParentID, "Dispatched Agent", inspectChildMessages()...)
	addParentTranscript(t, m, ws, 3)

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	_, exit := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	ws.listFailures[inspectParentID] = true
	failed, ok := exit().(inspectFetchFailedMsg)
	require.True(t, ok)
	_, next := m.Update(failed)
	runInspectCmds(m, next)
	require.Nil(t, m.inspectWindow, "a failed restore fetch must clear the pending window")

	runInspectCmds(m, m.enterInspect(agentBlockRef{sessionID: inspectChildID}))
	require.True(t, m.isInspecting(), "a failed exit must not block the next drill-in")
	_, exit2 := m.handleInspectKeys(tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl})
	delete(ws.listFailures, inspectParentID)
	runInspectCmds(m, exit2)
	require.False(t, m.isInspecting())
	require.NotNil(t, m.chat.MessageItem(parentMessageID(0)),
		"a later exit must restore the parent transcript")
}
