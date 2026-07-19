package model

import (
	"context"
	"errors"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// sidekickClearCommand wipes the ephemeral Sidekick conversation (#48):
// nothing is persisted, so clearing simply starts a fresh session.
const sidekickClearCommand = "/clear"

// Sidekick input textarea height bounds, in rows.
const (
	sidekickInputMinHeight = 1
	sidekickInputMaxHeight = 4
)

// sidekickPlaceholder is the input placeholder for the Sidekick prompt.
const sidekickPlaceholder = "Ask the sidekick..."

type (
	// sidekickEventMsg carries one event from the Sidekick's private
	// message stream. It is a distinct type from
	// pubsub.Event[message.Message] so Sidekick traffic can never enter
	// the main chat's message handling (or its busy/queue plumbing).
	sidekickEventMsg struct {
		event pubsub.Event[message.Message]
	}

	// sidekickRunFinishedMsg is sent when a RunSidekick dispatch
	// returns.
	sidekickRunFinishedMsg struct {
		err error
	}
)

// sidekickState is the Sidekick chat panel: an ephemeral conversation
// with the independent Sidekick agent, rendered inside the sidebar's
// Sidekick tab. All state lives in process memory and dies with Crush.
type sidekickState struct {
	// input is the prompt textarea. Lazily constructed by
	// ensureSidekickInput so struct-literal test UIs stay cheap.
	input       textarea.Model
	initialized bool

	// msgs mirrors the Sidekick's ephemeral conversation, oldest first,
	// fed exclusively by the agent's private message event stream.
	msgs []message.Message

	// busy is true while a RunSidekick dispatch is in flight. This is
	// panel-local state: it never touches the main agent's busy caches.
	busy bool

	// errText holds the last run failure, rendered inline in the list.
	errText string

	// scrollback is how many lines the message list is scrolled up from
	// the bottom; 0 follows the live tail. Clamped at render time.
	scrollback int

	// events is the subscription to the Sidekick's private message
	// broker; nil until the first successful subscribe.
	events <-chan pubsub.Event[message.Message]
}

// messageIndex returns the index of the message with the given ID, or -1.
func (s *sidekickState) messageIndex(id string) int {
	for i := range s.msgs {
		if s.msgs[i].ID == id {
			return i
		}
	}
	return -1
}

// ensureSidekickInput lazily constructs the Sidekick prompt textarea.
func (m *UI) ensureSidekickInput() {
	if m.sidekick.initialized {
		return
	}
	ta := textarea.New()
	if m.com != nil && m.com.Styles != nil {
		ta.SetStyles(m.com.Styles.Editor.Textarea)
	}
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.DynamicHeight = true
	ta.MinHeight = sidekickInputMinHeight
	ta.MaxHeight = sidekickInputMaxHeight
	ta.Placeholder = sidekickPlaceholder
	m.sidekick.input = ta
	m.sidekick.initialized = true
}

// sidekickPaneFocused reports whether keys currently route to the
// Sidekick chat panel: sidebar focus with the Sidekick tab in view.
func (m *UI) sidekickPaneFocused() bool {
	return m.focus == uiFocusSidebar && m.sidekickTabInView()
}

// subscribeSidekick starts listening to the Sidekick's private message
// stream. Safe to call repeatedly: it is a no-op once subscribed, when
// the workspace has no Sidekick, or in struct-literal test UIs without a
// workspace.
func (m *UI) subscribeSidekick() tea.Cmd {
	if m.sidekick.events != nil {
		return nil
	}
	if m.com == nil || m.com.Workspace == nil || !m.com.Workspace.SidekickAvailable() {
		return nil
	}
	ch := m.com.Workspace.SidekickSubscribe(context.Background())
	if ch == nil {
		return nil
	}
	m.sidekick.events = ch
	return m.awaitSidekickEvent()
}

// awaitSidekickEvent returns a command that blocks on the next Sidekick
// message event. The handler re-issues it to keep the stream draining.
func (m *UI) awaitSidekickEvent() tea.Cmd {
	ch := m.sidekick.events
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return sidekickEventMsg{event: ev}
	}
}

// applySidekickEvent folds one Sidekick message event into the panel's
// conversation mirror.
func (m *UI) applySidekickEvent(ev pubsub.Event[message.Message]) {
	sk := &m.sidekick
	switch ev.Type {
	case pubsub.CreatedEvent:
		// A different session ID means the conversation restarted
		// (e.g. a /clear from this or another view): drop stale
		// entries before appending.
		if len(sk.msgs) > 0 && sk.msgs[0].SessionID != ev.Payload.SessionID {
			sk.msgs = nil
		}
		if sk.messageIndex(ev.Payload.ID) >= 0 {
			return
		}
		sk.msgs = append(sk.msgs, ev.Payload)
		if ev.Payload.Role == message.Assistant {
			m.bumpSidekickUnread()
		}
	case pubsub.UpdatedEvent:
		if i := sk.messageIndex(ev.Payload.ID); i >= 0 {
			sk.msgs[i] = ev.Payload
			return
		}
		// Missed create (e.g. subscribe raced the first token): treat
		// as an append when it belongs to the current conversation.
		if len(sk.msgs) == 0 || sk.msgs[0].SessionID == ev.Payload.SessionID {
			sk.msgs = append(sk.msgs, ev.Payload)
		}
	case pubsub.DeletedEvent:
		if i := sk.messageIndex(ev.Payload.ID); i >= 0 {
			sk.msgs = append(sk.msgs[:i], sk.msgs[i+1:]...)
		}
	}
}

// handleSidekickKey routes a key press to the Sidekick chat panel. Tab
// keeps cycling the sidebar tabs (#52); Esc is handled earlier in
// handleKeyPressMsg (cancel-or-leave). Everything else feeds the prompt
// textarea, which keeps the sidebar's global-key-inertness contract.
func (m *UI) handleSidekickKey(msg tea.KeyPressMsg) tea.Cmd {
	m.ensureSidekickInput()
	var cmds []tea.Cmd
	switch {
	case key.Matches(msg, m.keyMap.Tab):
		m.cycleSidebarTab()
	case key.Matches(msg, m.keyMap.Editor.Newline):
		m.sidekick.input.InsertRune('\n')
	case key.Matches(msg, m.keyMap.Editor.SendMessage):
		if cmd := m.submitSidekickPrompt(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	default:
		var cmd tea.Cmd
		m.sidekick.input, cmd = m.sidekick.input.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// handleSidekickEscape implements Esc inside the Sidekick pane: cancel a
// streaming Sidekick run, otherwise hand focus back to the editor. It
// deliberately never reaches the main agent's cancel path — Sidekick
// activity and the main run are independent (#50).
func (m *UI) handleSidekickEscape() tea.Cmd {
	if m.sidekick.busy {
		if m.com != nil && m.com.Workspace != nil {
			m.com.Workspace.SidekickCancel()
		}
		return util.ReportInfo("Canceling sidekick...")
	}
	if m.sidekick.initialized {
		m.sidekick.input.Blur()
	}
	m.focus = uiFocusEditor
	return m.textarea.Focus()
}

// submitSidekickPrompt dispatches the textarea content to the Sidekick.
// Enter submits; /clear wipes the ephemeral conversation. Dispatch goes
// through RunSidekick, which never queues behind — or marks busy — the
// main agent.
func (m *UI) submitSidekickPrompt() tea.Cmd {
	value := strings.TrimSpace(m.sidekick.input.Value())
	if value == "" {
		return nil
	}
	m.sidekick.input.Reset()

	if value == sidekickClearCommand {
		return m.clearSidekickConversation()
	}

	ws := m.com.Workspace
	if ws == nil || !ws.SidekickAvailable() {
		return util.ReportWarn("Sidekick is not available in this workspace")
	}
	if m.sidekick.busy {
		return util.ReportWarn("Sidekick is busy, please wait...")
	}

	m.sidekick.busy = true
	m.sidekick.errText = ""
	m.sidekick.scrollback = 0

	run := func() tea.Msg {
		return sidekickRunFinishedMsg{err: ws.SidekickRun(context.Background(), value)}
	}
	if sub := m.subscribeSidekick(); sub != nil {
		return tea.Batch(sub, run)
	}
	return run
}

// clearSidekickConversation implements /clear: the panel forgets the
// conversation immediately and the workspace destroys the ephemeral
// session (canceling any in-flight run) so the next prompt starts fresh.
func (m *UI) clearSidekickConversation() tea.Cmd {
	sk := &m.sidekick
	sk.msgs = nil
	sk.errText = ""
	sk.busy = false
	sk.scrollback = 0

	ws := m.com.Workspace
	if ws == nil || !ws.SidekickAvailable() {
		return nil
	}
	return func() tea.Msg {
		if err := ws.SidekickClear(context.Background()); err != nil {
			return util.InfoMsg{Type: util.InfoTypeError, Msg: err.Error()}
		}
		return nil
	}
}

// handleSidekickRunFinished folds the terminal result of a Sidekick run
// back into the panel.
func (m *UI) handleSidekickRunFinished(err error) {
	m.sidekick.busy = false
	if err != nil && !errors.Is(err, context.Canceled) {
		m.sidekick.errText = err.Error()
	}
}

// renderSidekickPanel renders the Sidekick chat panel — message
// scrollback, prompt input, and footer (model + tool state) — sized to
// fill exactly height rows at the given width.
func (m *UI) renderSidekickPanel(width, height int) string {
	t := m.com.Styles
	if width <= 0 || height <= 0 {
		return ""
	}

	ws := m.com.Workspace
	if ws == nil || !ws.SidekickAvailable() {
		return t.Sidebar.TabPlaceholder.Width(width).Render("Sidekick is not available in this workspace.")
	}

	m.ensureSidekickInput()
	m.sidekick.input.SetWidth(width)
	input := m.sidekick.input.View()
	footer := m.renderSidekickFooter(width)

	listHeight := height - lipgloss.Height(input) - lipgloss.Height(footer) - 2
	parts := make([]string, 0, 5)
	if listHeight > 0 {
		parts = append(parts, m.renderSidekickMessages(width, listHeight), "")
	}
	parts = append(parts, input, "", footer)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// renderSidekickMessages renders the conversation bottom-aligned into
// exactly height rows, honoring (and clamping) the scrollback offset.
func (m *UI) renderSidekickMessages(width, height int) string {
	t := m.com.Styles
	sk := &m.sidekick

	var blocks []string
	for i := range sk.msgs {
		if b := renderSidekickMessage(t, &sk.msgs[i], width); b != "" {
			blocks = append(blocks, b)
		}
	}
	if sk.busy && !sidekickStreaming(sk.msgs) {
		blocks = append(blocks, t.Sidebar.SidekickThinking.Render("Thinking..."))
	}
	if sk.errText != "" {
		blocks = append(blocks, t.Sidebar.SidekickError.Width(width).Render(sk.errText))
	}
	if len(blocks) == 0 {
		blocks = append(blocks, t.Sidebar.TabPlaceholder.Width(width).Render(
			"Ask the sidekick anything about this workspace. Enter sends, /clear starts over."))
	}

	lines := strings.Split(strings.Join(blocks, "\n\n"), "\n")

	// Window the last `height` lines, offset by the scrollback, then pad
	// the top so the conversation hugs the input.
	maxScrollback := max(0, len(lines)-height)
	sk.scrollback = min(sk.scrollback, maxScrollback)
	end := len(lines) - sk.scrollback
	start := max(0, end-height)
	lines = lines[start:end]
	if pad := height - len(lines); pad > 0 {
		lines = append(make([]string, pad), lines...)
	}
	return strings.Join(lines, "\n")
}

// sidekickStreaming reports whether assistant output for the in-flight
// turn has started arriving (text or a tool call), which retires the
// "Thinking..." indicator.
func sidekickStreaming(msgs []message.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	return last.Role == message.Assistant &&
		(strings.TrimSpace(last.Content().Text) != "" || len(last.ToolCalls()) > 0)
}

// renderSidekickMessage renders one conversation entry: user prompts as
// "> ..." lines, assistant turns as compact tool-call lines plus the
// response text. Assistant replies carrying A2UI render their surfaces
// inline through the shared a2tea pipeline at sidebar width (#55);
// surfaces are display-only in v1. Tool-result messages render nothing
// themselves.
func renderSidekickMessage(t *styles.Styles, msg *message.Message, width int) string {
	switch msg.Role {
	case message.User:
		text := strings.TrimSpace(msg.Content().Text)
		if text == "" {
			return ""
		}
		return t.Sidebar.SidekickUser.Width(width).Render("> " + text)
	case message.Assistant:
		var parts []string
		for _, tc := range msg.ToolCalls() {
			label := "⚙ " + tc.Name
			if !tc.Finished {
				label += "..."
			}
			parts = append(parts, t.Sidebar.SidekickTool.MaxWidth(width).Render(label))
		}
		if text := strings.TrimSpace(msg.Content().Text); text != "" {
			if out, ok := chat.RenderA2UIInline(t, text, width, msg.IsFinished()); ok {
				parts = append(parts, out)
			} else {
				parts = append(parts, t.Sidebar.SidekickAssistant.Width(width).Render(text))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// renderSidekickFooter renders the one-line panel footer: the Sidekick's
// active model and its tool state (e.g. `◇ Model · bash ✓ · grep ✓`).
func (m *UI) renderSidekickFooter(width int) string {
	t := m.com.Styles
	cfg := m.com.Config()
	if cfg == nil {
		return ""
	}

	agentCfg, ok := cfg.Agents[config.AgentSidekick]
	if !ok {
		return ""
	}
	var parts []string
	if sel, ok := cfg.Models[agentCfg.Model]; ok {
		name := sel.Model
		if mdl := cfg.GetModel(sel.Provider, sel.Model); mdl != nil && mdl.Name != "" {
			name = mdl.Name
		}
		if name != "" {
			parts = append(parts, "◇ "+name)
		}
	}
	for _, tool := range agentCfg.AllowedTools {
		parts = append(parts, tool+" ✓")
	}
	if len(parts) == 0 {
		return ""
	}
	return t.Sidebar.SidekickFooter.MaxWidth(width).Render(strings.Join(parts, " · "))
}
