package model

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/completions"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// handleTokenChars are the bytes a @handle token may contain. Deliberately
// narrower than a file path: a handle never carries a slash or a dot, so
// "@main.go" and "@ui/model" are file mentions, never handle candidates.
const handleTokenChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

// isHandleTokenChar reports whether b can appear in a @handle token.
func isHandleTokenChar(b byte) bool {
	return strings.IndexByte(handleTokenChars, b) >= 0
}

// handleBoundary is the punctuation a @handle token may be closed by:
// the token ends at whitespace or the end of the line, or at one of
// these characters when it is itself followed by whitespace or the end
// of the line — "@tester: stop" and "ask @tester." address the tester,
// while "@main.go" and "@tester's" stay a file mention and prose.
const handleBoundary = ":,.?!"

// handleTokenClosed reports whether the handle token ending at end
// closes there: the next byte is whitespace, the end of the text, or a
// handleBoundary character that itself looks out at whitespace or the
// end of the text. skip is true when a boundary character was consumed,
// so the caller starts the rest past it — "@tester: stop" gives rest
// "stop", not ": stop".
func handleTokenClosed(text string, end int) (closed, skip bool) {
	if end >= len(text) {
		return true, false
	}
	switch b := text[end]; {
	case b == ' ' || b == '\t' || b == '\n':
		return true, false
	case strings.IndexByte(handleBoundary, b) >= 0:
		if end+1 < len(text) {
			n := text[end+1]
			if n != ' ' && n != '\t' && n != '\n' {
				return false, false
			}
		}
		return true, true
	}
	return false, false
}

// splitLeadingHandle splits a submitted prompt whose first token is a
// @handle candidate (#313, #415): it returns the candidate (without the
// "@") and the remaining prompt text. The candidate must be the very
// first token of the very first line, closed by whitespace, the end of
// the prompt, or a boundary character ("@tester: stop") — "why are
// @tester and ..." does not route, it mentions. ok=false when the
// prompt does not open with a handle-shaped token.
func splitLeadingHandle(prompt string) (handle, rest string, ok bool) {
	prompt = strings.TrimLeft(prompt, " \t")
	if !strings.HasPrefix(prompt, "@") {
		return "", "", false
	}
	end := 1
	for end < len(prompt) && isHandleTokenChar(prompt[end]) {
		end++
	}
	if end == 1 {
		return "", "", false
	}
	handle = prompt[1:end]
	// The token must close at whitespace, end of prompt, or a boundary
	// character that looks out at whitespace: a token glued to more text
	// ("@tester's work") is prose, not an address.
	closed, skip := handleTokenClosed(prompt, end)
	if !closed {
		return "", "", false
	}
	if skip {
		end++
	}
	rest = strings.TrimLeft(prompt[end:], " \t\n")
	return handle, rest, true
}

// mentionHandles returns every distinct @handle candidate in the prompt
// except the leading one, in first-appearance order (#313): a mid-sentence
// mention attaches an agent card rather than routing.
func mentionHandles(prompt string) []string {
	var (
		out  []string
		seen map[string]bool
	)
	for lineNo, line := range strings.Split(prompt, "\n") {
		for i := 0; i < len(line); i++ {
			atWordStart := i == 0 || line[i-1] == ' ' || line[i-1] == '\t'
			if !atWordStart || line[i] != '@' || i+1 >= len(line) {
				continue
			}
			end := i + 1
			for end < len(line) && isHandleTokenChar(line[end]) {
				end++
			}
			handle := line[i+1 : end]
			if handle == "" {
				continue
			}
			// The token must close at whitespace, end of line, or a
			// boundary character that looks out at whitespace: a token
			// glued to more text ("@main.go", "@tester's") is prose or a
			// file mention, never a handle.
			closed, _ := handleTokenClosed(line, end)
			if !closed {
				continue
			}
			// Skip the very first token of the very first line: it is the
			// leading-address candidate, not a mention.
			if lineNo == 0 && i == 0 {
				i = end - 1
				continue
			}
			if seen == nil {
				seen = map[string]bool{}
			}
			if !seen[handle] {
				seen[handle] = true
				out = append(out, handle)
			}
			i = end - 1
		}
	}
	return out
}

// AgentCardAttachment renders the agent-card attachment for one mentioned
// dispatched agent (#313): a live agent gets its status card (handle,
// status, current todo, session pointer, workspace); a finished agent gets
// the read-only transcript card (findings and diff summary from the
// terminal result) so "what did @tester find?" answers without a
// continuation. Mentioning never routes: the parent turn keeps the text.
func AgentCardAttachment(snap dispatch.TodoSnapshot) message.Attachment {
	entry := snap.Entry
	var b strings.Builder
	if entry.Status.IsTerminal() {
		b.WriteString("Read-only agent card (the agent has finished; its session is not continuable — dispatch a new agent to continue this work).\n\n")
	} else {
		b.WriteString("Live agent card (address it mid-run with the message_agent tool or by leading your message with @handle).\n\n")
	}
	b.WriteString("Handle: @" + entry.Handle + "\n")
	if entry.Role != "" {
		b.WriteString("Role: " + entry.Role + "\n")
	}
	b.WriteString("Status: " + string(entry.Status) + "\n")
	if !entry.Status.IsTerminal() && snap.CurrentTodo != "" {
		b.WriteString("Current todo: " + snap.CurrentTodo + "\n")
	}
	if entry.SessionID != "" {
		b.WriteString("Session: " + entry.SessionID + "\n")
	}
	if entry.Result != nil {
		if entry.Result.KeyFindings != "" {
			b.WriteString("\nKey findings:\n" + entry.Result.KeyFindings + "\n")
		}
		if entry.Result.DiffSummary != "" {
			b.WriteString("\nDiff summary:\n" + entry.Result.DiffSummary + "\n")
		}
	}
	return message.Attachment{
		FileName: "agent-" + entry.Handle + ".md",
		MimeType: "text/markdown",
		Content:  []byte(b.String()),
		Kind:     message.AttachmentKindAgentCard,
	}
}

// agentMentionAttachments builds the agent-card attachments for every
// mid-sentence @handle mention in the prompt that resolves to a known
// dispatch (#313). The router never multiplexes a mid-sentence question
// to the mentioned agents: the parent turn keeps the text verbatim, and
// the cards are what the mentioned agents contribute. Handles resolve
// through their slug, so the user's casing never matters — "@Tester"
// and "@tester" are the same agent, and one card per agent.
func (m *UI) agentMentionAttachments(prompt string) []message.Attachment {
	handles := mentionHandles(prompt)
	if len(handles) == 0 {
		return nil
	}
	var (
		cards []message.Attachment
		seen  = map[string]bool{}
	)
	for _, handle := range handles {
		slug := dispatch.HandleSlug(handle)
		if seen[slug] {
			continue
		}
		snap, ok := m.com.Workspace.DispatchByHandle(m.currentSessionID(), slug)
		if !ok {
			// Not a dispatch: a file mention, a typo, or prose. The
			// normal prompt path handles @file tokens.
			continue
		}
		seen[slug] = true
		cards = append(cards, AgentCardAttachment(snap))
	}
	return cards
}

// routeLeadingAgentHandle handles a prompt that opens with a @handle
// (#313): the message routes directly to that agent's injection queue
// (#312) instead of starting a parent turn. handled reports whether the
// prompt was consumed — routed, refused, or invalid — and the caller must
// not also send it to the parent session. delivered reports whether the
// message actually reached the agent's queue; a consumed prompt that was
// refused or failed to deliver is handled but not delivered, and the
// caller restores the editor's pre-submit state for it (#414).
// attachments are the editor's captured chips, delivered with the steer.
// An unresolvable leading token (a file path, an unknown name) falls
// back to the normal prompt path.
func (m *UI) routeLeadingAgentHandle(prompt string, attachments []message.Attachment) (cmd tea.Cmd, handled, delivered bool) {
	handle, rest, ok := splitLeadingHandle(prompt)
	if !ok {
		return nil, false, false
	}
	// Handles are stored slugged; resolve through the slug so the user's
	// casing never matters, exactly like the message_agent tool path.
	handle = dispatch.HandleSlug(handle)
	snap, found := m.com.Workspace.DispatchByHandle(m.currentSessionID(), handle)
	if !found {
		// Not a dispatch handle: most likely a file mention, an unknown
		// bare token, or a typo. The normal prompt path owns it, whatever
		// the message is — an unknown token must never be swallowed.
		return nil, false, false
	}
	if rest == "" {
		return util.ReportWarn("Nothing to send @handle — write the message after the handle, e.g. \"@" + handle + " stop writing Rust\"."), true, false
	}
	if snap.Entry.Status.IsTerminal() {
		return util.ReportError(fmt.Errorf("agent @%s finished (%s); task sessions are never continuable — dispatch a new agent instead", handle, snap.Entry.Status)), true, false
	}
	if err := m.com.Workspace.DeliverAgentMessageByHandle(context.Background(), m.currentSessionID(), handle, rest, attachments); err != nil {
		return util.ReportError(err), true, false
	}
	// The steer and the agent's response appear on its dispatch block in
	// the chat (the child session's message events); nothing lands in the
	// parent session.
	return nil, true, true
}

// agentCompletionValues composes the live-agents source for the @
// completions (#313): handle, and a "role · status · current todo" detail
// with a status dot. Live agents only — finished handles never appear.
func (m *UI) agentCompletionValues() []completions.AgentCompletionValue {
	live := m.com.Workspace.DispatchLive(m.currentSessionID())
	if len(live) == 0 {
		return nil
	}
	out := make([]completions.AgentCompletionValue, 0, len(live))
	for _, snap := range live {
		if snap.Entry.Handle == "" {
			continue
		}
		var parts []string
		if snap.Entry.Role != "" {
			parts = append(parts, snap.Entry.Role)
		}
		parts = append(parts, dispatchStateDot(snap.Entry.Status), completionStateLabel(snap.Entry.Status))
		if snap.CurrentTodo != "" {
			parts = append(parts, snap.CurrentTodo)
		}
		out = append(out, completions.AgentCompletionValue{
			Handle: snap.Entry.Handle,
			Detail: strings.Join(parts, " · "),
		})
	}
	return out
}

// dispatchStateDot maps a dispatch status to its status-dot glyph for the
// completion row: filled while the agent works, hollow while queued.
func dispatchStateDot(status dispatch.Status) string {
	switch status {
	case dispatch.StatusRunning:
		return "●"
	case dispatch.StatusProvisioned:
		return "○"
	default:
		return "·"
	}
}

// completionStateLabel is the completion row's status word.
func completionStateLabel(status dispatch.Status) string {
	switch status {
	case dispatch.StatusRunning:
		return "working"
	case dispatch.StatusProvisioned:
		return "queued"
	case dispatch.StatusCompleted:
		return "complete"
	case dispatch.StatusFailed:
		return "failed"
	case dispatch.StatusKilled:
		return "killed"
	default:
		return string(status)
	}
}
