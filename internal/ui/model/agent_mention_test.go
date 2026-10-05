package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// agentMentionWorkspace stubs the dispatch surfaces (#313) the routing
// and completion code reads: a handle registry keyed by handle, a live
// list, and a recording delivery sink.
type agentMentionWorkspace struct {
	slashCommandWorkspace
	byHandle   map[string]dispatch.TodoSnapshot
	delivered  []deliveredMessage
	deliverErr error
	agentRuns  int
}

type deliveredMessage struct {
	session, handle, text string
	attachments           []message.Attachment
}

func (w *agentMentionWorkspace) DispatchLive(sessionID string) []dispatch.TodoSnapshot {
	var out []dispatch.TodoSnapshot
	for _, snap := range w.byHandle {
		if !snap.Entry.Status.IsTerminal() && snap.Entry.ParentSessionID == sessionID {
			out = append(out, snap)
		}
	}
	return out
}

func (w *agentMentionWorkspace) DispatchByHandle(sessionID, handle string) (dispatch.TodoSnapshot, bool) {
	snap, ok := w.byHandle[handle]
	if !ok || snap.Entry.ParentSessionID != sessionID {
		return dispatch.TodoSnapshot{}, false
	}
	return snap, ok
}

func (w *agentMentionWorkspace) DeliverAgentMessageByHandle(ctx context.Context, sessionID, handle, text string, attachments []message.Attachment) error {
	if w.deliverErr != nil {
		return w.deliverErr
	}
	w.delivered = append(w.delivered, deliveredMessage{session: sessionID, handle: handle, text: text, attachments: attachments})
	return nil
}

// AgentRun records a parent-turn start; a routed or refused steer must
// never reach it (#414).
func (w *agentMentionWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) error {
	w.agentRuns++
	return nil
}

// compile-time proof the stub satisfies what the routing path uses.
var _ workspace.Workspace = (*agentMentionWorkspace)(nil)

func newAgentMentionUI(ws *agentMentionWorkspace) *UI {
	ui := newCompletionBackspaceUIWith(ws)
	ui.session = &session.Session{ID: testMentionSession}
	return ui
}

// testMentionSession is the session the mention-test UI is attached to;
// its dispatches carry it as their parent.
const testMentionSession = "sess-mention-parent"

func runningSnapshot(handle, role string) dispatch.TodoSnapshot {
	return dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:              "dispatch-" + handle,
			SessionID:       "msg$$call-" + handle,
			ParentSessionID: testMentionSession,
			Handle:          handle,
			Role:            role,
			Status:          dispatch.StatusRunning,
		},
		CurrentTodo: "wiring form validation",
	}
}

func finishedSnapshot(handle string) dispatch.TodoSnapshot {
	return dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:              "dispatch-" + handle,
			SessionID:       "msg$$call-" + handle,
			ParentSessionID: testMentionSession,
			Handle:          handle,
			Status:          dispatch.StatusCompleted,
			Result: &dispatch.DispatchResult{
				Status:      dispatch.StatusCompleted,
				KeyFindings: "Switched to Go.",
			},
		},
	}
}

// splitLeadingHandle recognizes exactly the leading-address form: a
// @handle as the very first token, closed by whitespace, end of prompt,
// or a boundary character that looks out at whitespace; glued or
// mid-sentence tokens are not leading addresses.
func TestSplitLeadingHandle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prompt string
		handle string
		rest   string
		ok     bool
	}{
		{"plain", "@tester stop writing Rust", "tester", "stop writing Rust", true},
		{"bare handle", "@tester", "tester", "", true},
		{"trailing space", "@tester ", "tester", "", true},
		{"across lines", "@tester\nplease stop", "tester", "please stop", true},
		{"leading spaces", "  @tester go", "tester", "go", true},
		{"dashes and digits", "@tester-2 again", "tester-2", "again", true},
		{"colon boundary", "@tester: stop", "tester", "stop", true},
		{"comma boundary", "@tester, stop", "tester", "stop", true},
		{"question mark boundary", "@tester? status", "tester", "status", true},
		{"period boundary", "@tester. fix it", "tester", "fix it", true},
		{"exclamation boundary", "@tester! now", "tester", "now", true},
		{"period at end", "@tester.", "tester", "", true},
		{"bare file token", "@Makefile", "Makefile", "", true},
		{"file mention", "@main.go", "", "", false},
		{"colon glued to word", "@tester:stop", "", "", false},
		{"ellipsis is prose", "@tester... hmm", "", "", false},
		{"dot mid-word", "@tester.go fix", "", "", false},
		{"glued prose", "@tester's work is done", "", "", false},
		{"file path", "@internal/ui/model/foo.go fix this", "", "", false},
		{"mid-sentence only", "why is @tester writing Rust?", "", "", false},
		{"no handle", "plain prompt", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handle, rest, ok := splitLeadingHandle(tc.prompt)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.handle, handle)
			require.Equal(t, tc.rest, rest)
		})
	}
}

// mentionHandles finds every distinct mid-sentence handle candidate,
// skipping the leading one and non-handle tokens.
func TestMentionHandles(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		[]string{"tester", "docs"},
		mentionHandles("why are @tester and @docs writing Rust? @tester twice, @main.go is a file"))
	require.Equal(t, []string{"tester"}, mentionHandles("how is @tester?"))
	require.Equal(t, []string{"tester"}, mentionHandles("ask @tester."))
	require.Equal(t, []string{"tester"}, mentionHandles("ping @tester, then"))
	require.Nil(t, mentionHandles("@tester.go fix the build"))
	require.Nil(t, mentionHandles("@tester leads so it does not mention"))
	require.Nil(t, mentionHandles("no mentions here"))
	require.Equal(t, []string{"tester"}, mentionHandles("line one\n@tester on the next line"))
}

// AgentCardAttachment composes the live card from a running snapshot and
// the read-only transcript card from a terminal one.
func TestAgentCardAttachment(t *testing.T) {
	t.Parallel()

	live := AgentCardAttachment(runningSnapshot("tester", "writes tests"))
	require.Equal(t, message.AttachmentKindAgentCard, live.Kind)
	require.Contains(t, string(live.Content), "@tester")
	require.Contains(t, string(live.Content), "writes tests")
	require.Contains(t, string(live.Content), "wiring form validation")
	require.Contains(t, string(live.Content), "msg$$call-tester")
	require.Contains(t, string(live.Content), "Live agent card")

	done := AgentCardAttachment(finishedSnapshot("tester"))
	require.Contains(t, string(done.Content), "not continuable")
	require.Contains(t, string(done.Content), "Switched to Go.")
	require.Contains(t, string(done.Content), "Read-only agent card")
}

// The leading @handle routes to the agent's injection queue and consumes
// the prompt; a finished handle refuses cleanly; an unresolvable leading
// token falls back to the normal prompt path; a bare handle with no
// message is invalid. The editor's attachments ride the delivered steer
// (#414); a refused or failed steer is handled but not delivered.
func TestRouteLeadingAgentHandle(t *testing.T) {
	t.Parallel()

	atts := []message.Attachment{{FileName: "note.txt", MimeType: "text/plain", Content: []byte("hello")}}
	ws := &agentMentionWorkspace{byHandle: map[string]dispatch.TodoSnapshot{
		"tester": runningSnapshot("tester", "writes tests"),
		"done":   finishedSnapshot("done"),
	}}
	m := newAgentMentionUI(ws)

	// Routing: consumed, delivered with the rest of the prompt and the
	// editor's attachments, scoped to the UI's session.
	_, handled, delivered := m.routeLeadingAgentHandle("@tester stop writing Rust", atts)
	require.True(t, handled)
	require.True(t, delivered)
	require.Equal(t, []deliveredMessage{{session: testMentionSession, handle: "tester", text: "stop writing Rust", attachments: atts}}, ws.delivered)

	// Finished: refused, not delivered.
	cmd, handled, delivered := m.routeLeadingAgentHandle("@done one more thing", atts)
	require.True(t, handled)
	require.False(t, delivered)
	require.NotNil(t, cmd, "a refusal must surface")
	require.Len(t, ws.delivered, 1)

	// Unknown leading token: falls back to the normal prompt path.
	_, handled, delivered = m.routeLeadingAgentHandle("@internal/ui/foo.go explain this", atts)
	require.False(t, handled)
	require.False(t, delivered)

	// Bare handle, no message: consumed with a warning, nothing sent.
	cmd, handled, delivered = m.routeLeadingAgentHandle("@tester", atts)
	require.True(t, handled)
	require.False(t, delivered)
	require.NotNil(t, cmd)
	require.Len(t, ws.delivered, 1)

	// Unknown bare token: it parses as a leading handle candidate, but
	// with no such agent it falls through to the normal prompt path —
	// the empty-message warning must not swallow it.
	_, handled, _ = m.routeLeadingAgentHandle("@Makefile", atts)
	require.False(t, handled)
	require.Len(t, ws.delivered, 1)

	// Boundary punctuation is dropped from the message: the agent gets
	// the rest verbatim.
	for _, prompt := range []string{"@tester: stop", "@tester, stop", "@tester? status"} {
		_, handled, _ = m.routeLeadingAgentHandle(prompt, atts)
		require.True(t, handled)
	}
	require.Equal(t,
		[]deliveredMessage{
			{session: testMentionSession, handle: "tester", text: "stop writing Rust", attachments: atts},
			{session: testMentionSession, handle: "tester", text: "stop", attachments: atts},
			{session: testMentionSession, handle: "tester", text: "stop", attachments: atts},
			{session: testMentionSession, handle: "tester", text: "status", attachments: atts},
		},
		ws.delivered)

	// Delivery failure surfaces as an error, and nothing new is delivered.
	ws.delivered = nil
	ws.deliverErr = context.DeadlineExceeded
	cmd, handled, delivered = m.routeLeadingAgentHandle("@tester try again", atts)
	require.True(t, handled)
	require.False(t, delivered)
	require.NotNil(t, cmd)
	require.Empty(t, ws.delivered)
}

// A leading handle typed with different casing routes to the same
// stored slug — "@Tester stop" steers the "tester" agent, exactly like
// the message_agent tool path tolerates the addressed form.
func TestRouteLeadingAgentHandleCaseInsensitive(t *testing.T) {
	t.Parallel()

	ws := &agentMentionWorkspace{byHandle: map[string]dispatch.TodoSnapshot{
		"tester": runningSnapshot("tester", "writes tests"),
	}}
	m := newAgentMentionUI(ws)

	_, handled, _ := m.routeLeadingAgentHandle("@Tester stop writing Rust", nil)
	require.True(t, handled)
	require.Equal(t, []deliveredMessage{{session: testMentionSession, handle: "tester", text: "stop writing Rust"}}, ws.delivered)

	// An unknown name still falls back to the normal prompt path, upper-
	// or lower-case alike.
	_, handled, _ = m.routeLeadingAgentHandle("@Nope hello", nil)
	require.False(t, handled)
}

// Mid-sentence mentions compose agent-card attachments for every handle
// that resolves, live or finished, and skip files and unknowns.
func TestAgentMentionAttachments(t *testing.T) {
	t.Parallel()

	ws := &agentMentionWorkspace{byHandle: map[string]dispatch.TodoSnapshot{
		"tester": runningSnapshot("tester", "writes tests"),
		"done":   finishedSnapshot("done"),
	}}
	m := newAgentMentionUI(ws)

	cards := m.agentMentionAttachments("why are @tester and @done and @main.go like this?")
	require.Len(t, cards, 2)
	require.Equal(t, message.AttachmentKindAgentCard, cards[0].Kind)
	require.Contains(t, string(cards[0].Content), "@tester")
	require.Contains(t, string(cards[1].Content), "not continuable")

	require.Empty(t, m.agentMentionAttachments("no mentions"))

	// Casing never matters and one card lands per agent: "@Tester" is
	// the stored "tester" slug, however the user typed it.
	cards = m.agentMentionAttachments("ask @Tester and @tester what happened")
	require.Len(t, cards, 1)
	require.Contains(t, string(cards[0].Content), "@tester")
}

// The completions' live-agents source carries handle, role, a status
// dot, and the current todo — live agents only.
func TestAgentCompletionValues(t *testing.T) {
	t.Parallel()

	ws := &agentMentionWorkspace{byHandle: map[string]dispatch.TodoSnapshot{
		"tester": runningSnapshot("tester", "writes tests"),
		"done":   finishedSnapshot("done"),
	}}
	m := newAgentMentionUI(ws)

	values := m.agentCompletionValues()
	require.Len(t, values, 1, "finished handles never appear")
	require.Equal(t, "tester", values[0].Handle)
	require.Contains(t, values[0].Detail, "writes tests")
	require.Contains(t, values[0].Detail, "●")
	require.Contains(t, values[0].Detail, "wiring form validation")
}

// The UI's dispatch surfaces are scoped to the session the UI is
// attached to (#399): an agent another session dispatched is invisible
// to routing, mentions, and completions, and a leading @handle for one
// falls back to the normal prompt path.
func TestAgentMentionOtherSessionInvisible(t *testing.T) {
	t.Parallel()

	foreign := runningSnapshot("foreign", "other session's agent")
	foreign.Entry.ParentSessionID = "sess-other-session"
	ws := &agentMentionWorkspace{byHandle: map[string]dispatch.TodoSnapshot{
		"foreign": foreign,
	}}
	m := newAgentMentionUI(ws)

	_, handled, _ := m.routeLeadingAgentHandle("@foreign stop", nil)
	require.False(t, handled, "a foreign-session handle falls back to the prompt path")
	require.Empty(t, ws.delivered)
	require.Empty(t, m.agentMentionAttachments("why is @foreign stuck?"))
	require.Empty(t, m.agentCompletionValues())
}

// newAgentMentionSubmitUI is newAgentMentionUI with a real keymap, for
// tests that drive the send path through a key press.
func newAgentMentionSubmitUI(ws *agentMentionWorkspace) *UI {
	ui := newAgentMentionUI(ws)
	ui.keyMap = DefaultKeyMap()
	return ui
}

// A leading @handle submitted with Enter delivers the editor's
// attachments with the steer (#414) and clears the editor exactly like
// a sent prompt; the parent session never runs.
func TestSubmitLeadingHandleDeliversAttachments(t *testing.T) {
	t.Parallel()

	atts := []message.Attachment{
		{FileName: "screenshot.png", MimeType: "image/png", Content: []byte("png")},
		{FileName: "notes.txt", MimeType: "text/plain", Content: []byte("notes")},
	}
	ws := &agentMentionWorkspace{byHandle: map[string]dispatch.TodoSnapshot{
		"tester": runningSnapshot("tester", "writes tests"),
	}}
	m := newAgentMentionSubmitUI(ws)
	for _, att := range atts {
		require.True(t, m.attachments.Update(att))
	}
	m.textarea.SetValue("@tester look at this")

	m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Equal(t, []deliveredMessage{{
		session:     testMentionSession,
		handle:      "tester",
		text:        "look at this",
		attachments: atts,
	}}, ws.delivered)
	require.Empty(t, m.textarea.Value(), "a delivered steer clears the editor")
	require.Empty(t, m.attachments.List())
	require.Zero(t, ws.agentRuns, "a routed steer never starts a parent turn")
}

// A refused or failed leading-handle submit restores the editor's
// pre-submit state (#414): the typed text, the attachment chips and the
// mention tracking, with the warning or error still surfaced. Nothing
// is sent to the parent.
func TestSubmitLeadingHandleRefusalRestoresEditor(t *testing.T) {
	t.Parallel()

	newCase := func(t *testing.T, byHandle map[string]dispatch.TodoSnapshot) (*UI, *agentMentionWorkspace, message.Attachment) {
		t.Helper()
		ws := &agentMentionWorkspace{byHandle: byHandle}
		m := newAgentMentionSubmitUI(ws)
		att := message.Attachment{FileName: "screenshot.png", MimeType: "image/png", Content: []byte("png")}
		require.True(t, m.attachments.Update(att))
		return m, ws, att
	}

	t.Run("finished agent", func(t *testing.T) {
		t.Parallel()
		m, ws, att := newCase(t, map[string]dispatch.TodoSnapshot{"done": finishedSnapshot("done")})
		m.textarea.SetValue("@done one more thing")

		m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

		require.Equal(t, "@done one more thing", m.textarea.Value())
		require.Equal(t, []message.Attachment{att}, m.attachments.List())
		require.Empty(t, ws.delivered)
		require.Zero(t, ws.agentRuns, "a refused steer never starts a parent turn")
	})

	t.Run("delivery error", func(t *testing.T) {
		t.Parallel()
		m, ws, att := newCase(t, map[string]dispatch.TodoSnapshot{"tester": runningSnapshot("tester", "writes tests")})
		ws.deliverErr = context.DeadlineExceeded
		m.mentionAttachments = map[string][]string{"@notes.txt": {"notes-key"}}
		m.discardedMentions = map[string]bool{"old.txt": true}
		m.textarea.SetValue("@tester try again")

		m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

		require.Equal(t, "@tester try again", m.textarea.Value())
		require.Equal(t, []message.Attachment{att}, m.attachments.List())
		require.Equal(t, map[string][]string{"@notes.txt": {"notes-key"}}, m.mentionAttachments)
		require.Equal(t, map[string]bool{"old.txt": true}, m.discardedMentions)
		require.Empty(t, ws.delivered)
		require.Zero(t, ws.agentRuns, "a failed steer never starts a parent turn")
	})

	t.Run("no message", func(t *testing.T) {
		t.Parallel()
		m, ws, att := newCase(t, map[string]dispatch.TodoSnapshot{"tester": runningSnapshot("tester", "writes tests")})
		m.textarea.SetValue("@tester")

		m.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

		require.Equal(t, "@tester", m.textarea.Value())
		require.Equal(t, []message.Attachment{att}, m.attachments.List())
		require.Empty(t, ws.delivered)
		require.Zero(t, ws.agentRuns, "an empty steer never starts a parent turn")
	})
}
