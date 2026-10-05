package model

import (
	"context"
	"testing"

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
}

type deliveredMessage struct {
	session, handle, text string
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

func (w *agentMentionWorkspace) DeliverAgentMessageByHandle(ctx context.Context, sessionID, handle, text string) error {
	if w.deliverErr != nil {
		return w.deliverErr
	}
	w.delivered = append(w.delivered, deliveredMessage{session: sessionID, handle: handle, text: text})
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
// @handle as the very first token, closed by whitespace or end of
// prompt; glued or mid-sentence tokens are not leading addresses.
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
// message is invalid.
func TestRouteLeadingAgentHandle(t *testing.T) {
	t.Parallel()

	ws := &agentMentionWorkspace{byHandle: map[string]dispatch.TodoSnapshot{
		"tester": runningSnapshot("tester", "writes tests"),
		"done":   finishedSnapshot("done"),
	}}
	m := newAgentMentionUI(ws)

	// Routing: consumed, delivered with the rest of the prompt, scoped
	// to the UI's session.
	_, handled := m.routeLeadingAgentHandle("@tester stop writing Rust")
	require.True(t, handled)
	require.Equal(t, []deliveredMessage{{session: testMentionSession, handle: "tester", text: "stop writing Rust"}}, ws.delivered)

	// Finished: refused, not delivered.
	cmd, handled := m.routeLeadingAgentHandle("@done one more thing")
	require.True(t, handled)
	require.NotNil(t, cmd, "a refusal must surface")
	require.Len(t, ws.delivered, 1)

	// Unknown leading token: falls back to the normal prompt path.
	_, handled = m.routeLeadingAgentHandle("@internal/ui/foo.go explain this")
	require.False(t, handled)

	// Bare handle, no message: consumed with a warning, nothing sent.
	cmd, handled = m.routeLeadingAgentHandle("@tester")
	require.True(t, handled)
	require.NotNil(t, cmd)
	require.Len(t, ws.delivered, 1)

	// Delivery failure surfaces as an error.
	ws.deliverErr = context.DeadlineExceeded
	cmd, handled = m.routeLeadingAgentHandle("@tester try again")
	require.True(t, handled)
	require.NotNil(t, cmd)
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

	_, handled := m.routeLeadingAgentHandle("@Tester stop writing Rust")
	require.True(t, handled)
	require.Equal(t, []deliveredMessage{{session: testMentionSession, handle: "tester", text: "stop writing Rust"}}, ws.delivered)

	// An unknown name still falls back to the normal prompt path, upper-
	// or lower-case alike.
	_, handled = m.routeLeadingAgentHandle("@Nope hello")
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

	_, handled := m.routeLeadingAgentHandle("@foreign stop")
	require.False(t, handled, "a foreign-session handle falls back to the prompt path")
	require.Empty(t, ws.delivered)
	require.Empty(t, m.agentMentionAttachments("why is @foreign stuck?"))
	require.Empty(t, m.agentCompletionValues())
}
