package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// sidekickTestWorkspace is a [workspace.Workspace] stub that records the
// Sidekick calls the chat panel makes. Any main-agent method reached
// through the embedded nil interface panics, which doubles as an
// independence assertion: Sidekick interactions must never touch it.
type sidekickTestWorkspace struct {
	workspace.Workspace
	cfg *config.Config

	runPrompts   []string
	clearCalls   int
	cancelCalls  int
	events       chan pubsub.Event[message.Message]
	runErr       error
	unavailable  bool
	subscribeNil bool
}

func (w *sidekickTestWorkspace) Config() *config.Config { return w.cfg }

func (w *sidekickTestWorkspace) SidekickAvailable() bool { return !w.unavailable }

func (w *sidekickTestWorkspace) SidekickRun(_ context.Context, prompt string) error {
	w.runPrompts = append(w.runPrompts, prompt)
	return w.runErr
}

func (w *sidekickTestWorkspace) SidekickCancel() { w.cancelCalls++ }

func (w *sidekickTestWorkspace) SidekickIsBusy() bool { return false }

func (w *sidekickTestWorkspace) SidekickClear(context.Context) error {
	w.clearCalls++
	return nil
}

func (w *sidekickTestWorkspace) SidekickSubscribe(context.Context) <-chan pubsub.Event[message.Message] {
	if w.subscribeNil {
		return nil
	}
	if w.events == nil {
		w.events = make(chan pubsub.Event[message.Message], 8)
	}
	return w.events
}

// newSidekickPanelTestUI builds a chat-state UI focused on the Sidekick
// pane, backed by a recording workspace stub. The event subscription is
// already established (as focusSidekick would), so submits dispatch
// directly instead of batching a subscribe first.
func newSidekickPanelTestUI() (*UI, *sidekickTestWorkspace) {
	m := newSidekickTestUI()
	ws := &sidekickTestWorkspace{cfg: &config.Config{Options: &config.Options{}}}
	m.com.Workspace = ws
	m.setSidebarTab(sidebarTabSidekick)
	m.focus = uiFocusSidebar
	m.ensureSidekickInput()
	m.sidekick.input.Focus()
	m.subscribeSidekick()
	return m, ws
}

// drainCmd executes cmd (recursively through batches/sequences) and
// returns every non-nil message produced.
func drainCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			out = append(out, drainCmd(t, c)...)
		}
	default:
		out = append(out, msg)
	}
	return out
}

func TestSidekickSubmitDispatchesRun(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()

	m.sidekick.input.SetValue("what changed?")
	cmd := m.submitSidekickPrompt()
	require.NotNil(t, cmd)
	require.True(t, m.sidekick.busy, "submit must mark the Sidekick busy")
	require.Empty(t, m.sidekick.input.Value(), "submit must reset the input")

	msgs := drainCmd(t, cmd)
	require.Equal(t, []string{"what changed?"}, ws.runPrompts)

	var finished bool
	for _, msg := range msgs {
		if fin, ok := msg.(sidekickRunFinishedMsg); ok {
			finished = true
			require.NoError(t, fin.err)
			m.handleSidekickRunFinished(fin.err)
		}
	}
	require.True(t, finished, "the dispatch must produce a terminal sidekickRunFinishedMsg")
	require.False(t, m.sidekick.busy)

	// Independence (#50): a Sidekick run must never mark the main agent
	// or its session busy, and must not enqueue behind the main agent.
	require.False(t, m.isAgentBusy())
	require.False(t, m.isCurrentSessionBusy())
	require.Zero(t, m.promptQueue)
}

func TestSidekickEnterKeyRoutesToSubmit(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()

	m.sidekick.input.SetValue("hello")
	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, m.sidekick.busy, "Enter in the Sidekick pane must dispatch the prompt")
	require.Empty(t, m.sidekick.input.Value(), "Enter must reset the input")
}

func TestSidekickTypingFeedsInput(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: 'h', Text: "h"})
	m.handleKeyPressMsg(tea.KeyPressMsg{Code: 'i', Text: "i"})
	require.Equal(t, "hi", m.sidekick.input.Value())
	require.Empty(t, m.textarea.Value(), "typing must not leak into the main editor")
}

func TestSidekickClearWipesConversation(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()
	m.sidekick.msgs = []message.Message{{ID: "u1", SessionID: "sk", Role: message.User}}
	m.sidekick.errText = "boom"
	m.sidekick.scrollback = 3

	m.sidekick.input.SetValue("/clear")
	cmd := m.submitSidekickPrompt()
	require.Empty(t, m.sidekick.msgs, "/clear must wipe the local conversation immediately")
	require.Empty(t, m.sidekick.errText)
	require.Zero(t, m.sidekick.scrollback)
	require.False(t, m.sidekick.busy)

	drainCmd(t, cmd)
	require.Equal(t, 1, ws.clearCalls, "/clear must destroy the ephemeral session")
	require.Empty(t, ws.runPrompts, "/clear must not be sent to the agent as a prompt")
}

func TestSidekickBusySubmitWarnsWithoutDispatch(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()
	m.sidekick.busy = true

	m.sidekick.input.SetValue("another question")
	msgs := drainCmd(t, m.submitSidekickPrompt())
	require.Empty(t, ws.runPrompts, "a busy Sidekick must not dispatch")
	require.Len(t, msgs, 1)
	info, ok := msgs[0].(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeWarn, info.Type)
}

func TestSidekickUnavailableSubmitWarns(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()
	ws.unavailable = true

	m.sidekick.input.SetValue("hi")
	msgs := drainCmd(t, m.submitSidekickPrompt())
	require.Empty(t, ws.runPrompts)
	require.Len(t, msgs, 1)
	info, ok := msgs[0].(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeWarn, info.Type)
}

func TestSidekickEscCancelsSidekickOnly(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()
	m.sidekick.busy = true
	// Pretend the main agent is busy too: Esc in the Sidekick pane must
	// cancel the Sidekick, not the main run. Reaching the main cancel
	// path would panic on the stub's embedded nil workspace.
	m.agentBusyCache.set(true)

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, 1, ws.cancelCalls, "Esc must cancel the in-flight Sidekick run")
	require.Equal(t, uiFocusSidebar, m.focus, "canceling must not leave the pane")
	require.False(t, m.isCanceling, "the main agent's double-esc cancel state must be untouched")
}

func TestSidekickEscLeavesPaneWhenIdle(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Zero(t, ws.cancelCalls)
	require.Equal(t, uiFocusEditor, m.focus, "Esc on an idle Sidekick returns to the editor")
}

func TestSidekickRunFailureShowsInlineError(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	m.sidekick.busy = true

	m.handleSidekickRunFinished(context.DeadlineExceeded)
	require.False(t, m.sidekick.busy)
	require.Equal(t, context.DeadlineExceeded.Error(), m.sidekick.errText)

	// Cancellation is not an error worth surfacing.
	m.sidekick.errText = ""
	m.handleSidekickRunFinished(context.Canceled)
	require.Empty(t, m.sidekick.errText)
}

func TestSidekickStreamingUpdatesConversation(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()

	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type: pubsub.CreatedEvent,
		Payload: message.Message{
			ID: "a1", SessionID: "sk", Role: message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "He"}},
		},
	})
	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type: pubsub.UpdatedEvent,
		Payload: message.Message{
			ID: "a1", SessionID: "sk", Role: message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "Hello there"}},
		},
	})

	require.Len(t, m.sidekick.msgs, 1, "streaming deltas must update in place")
	require.Equal(t, "Hello there", m.sidekick.msgs[0].Content().Text)
}

func TestSidekickNewSessionResetsConversation(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()

	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "u1", SessionID: "old", Role: message.User},
	})
	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "u2", SessionID: "new", Role: message.User},
	})

	require.Len(t, m.sidekick.msgs, 1, "a new session ID (post-/clear) must drop the old conversation")
	require.Equal(t, "u2", m.sidekick.msgs[0].ID)
}

func TestSidekickAssistantEventBumpsUnreadWhenHidden(t *testing.T) {
	t.Parallel()
	m, _ := newSidekickPanelTestUI()
	m.setSidebarTab(sidebarTabInfo) // Sidekick tab hidden

	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "a1", SessionID: "sk", Role: message.Assistant},
	})
	require.Equal(t, 1, m.sidekickUnread)

	m.setSidebarTab(sidebarTabSidekick)
	m.applySidekickEvent(pubsub.Event[message.Message]{
		Type:    pubsub.CreatedEvent,
		Payload: message.Message{ID: "a2", SessionID: "sk", Role: message.Assistant},
	})
	require.Zero(t, m.sidekickUnread, "content arriving on the visible tab is already seen")
}

func TestSidekickSubscribeOnce(t *testing.T) {
	t.Parallel()
	m := newSidekickTestUI()
	m.com.Workspace = &sidekickTestWorkspace{cfg: &config.Config{Options: &config.Options{}}}

	require.NotNil(t, m.subscribeSidekick())
	require.NotNil(t, m.sidekick.events)
	require.Nil(t, m.subscribeSidekick(), "resubscribing must be a no-op")
}
