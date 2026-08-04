package model

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// promptWorkspace resolves MCP prompts to a canned body.
type promptWorkspace struct {
	slashCommandWorkspace
	body     string
	err      error
	gotArgs  map[string]string
	gotName  string
	gotPromp string
}

func (w *promptWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	w.gotName, w.gotPromp, w.gotArgs = clientID, promptID, args
	return w.body, w.err
}

func runPromptCmd(t *testing.T, m *UI, cmd tea.Cmd) {
	t.Helper()
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				run(sub)
			}
			return
		}
		if att, ok := msg.(message.Attachment); ok {
			m.attachments.Update(att)
		}
	}
	run(cmd)
}

// TestAttachMCPPromptWithoutArguments pins the no-argument path: a compact
// token in the editor and the resolved body as an attachment, rather than the
// body being spliced into the prompt where it would bury what the user wrote.
func TestAttachMCPPromptWithoutArguments(t *testing.T) {
	t.Parallel()

	ws := &promptWorkspace{body: "Resolved prompt body, potentially very long."}
	ws.ready = true
	m := newCompletionBackspaceUIWith(ws)
	m.textarea.SetValue("please /")
	m.completionsStartIndex = len("please ")

	runPromptCmd(t, m, m.attachMCPPrompt("cairn:run_capture", "cairn", "run_capture", nil))

	require.Equal(t, "please /cairn:run_capture ", m.textarea.Value(),
		"the editor keeps a compact token, not the body")
	require.Len(t, m.attachments.List(), 1)
	att := m.attachments.List()[0]
	require.Equal(t, message.AttachmentKindMCPPrompt, att.Kind)
	require.Equal(t, "/cairn:run_capture", att.FileName)
	// FilePath is the removal key, made unique per insertion so two
	// insertions of one prompt are independently removable.
	require.Equal(t, "cairn:run_capture#1", att.FilePath)
	require.Equal(t, ws.body, string(att.Content), "the model must receive the body")
	require.Zero(t, att.PromptArgCount)
}

// TestAttachMCPPromptWithArguments pins the token's argument form. It has to
// be space-free: ScanPromptTokens ends a token at the first space, so a
// spaced form would highlight only the name and leave the arguments rendering
// as loose prose, and one atomic backspace would no longer remove the whole
// thing.
func TestAttachMCPPromptWithArguments(t *testing.T) {
	t.Parallel()

	ws := &promptWorkspace{body: "body"}
	ws.ready = true
	m := newCompletionBackspaceUIWith(ws)
	m.textarea.SetValue("do /")
	m.completionsStartIndex = len("do ")

	runPromptCmd(t, m, m.attachMCPPrompt("gitea:review", "gitea", "review",
		map[string]string{"repo": "stump.wtf/crush", "id": "42"}))

	got := m.textarea.Value()
	require.Equal(t, "do /gitea:review(id=42,repo=stump.wtf/crush) ", got)
	token := strings.TrimSpace(got[strings.Index(got, "/"):])
	require.NotContains(t, token, " ",
		"the token itself must contain no spaces, or ScanPromptTokens ends it early")

	require.Equal(t, "gitea", ws.gotName)
	require.Equal(t, "review", ws.gotPromp)
	require.Equal(t, map[string]string{"repo": "stump.wtf/crush", "id": "42"}, ws.gotArgs)

	require.Len(t, m.attachments.List(), 1)
	require.Equal(t, 2, m.attachments.List()[0].PromptArgCount,
		"the chip needs the argument count")
}

// TestAtomicBackspaceRemovesPromptChip pins that the prompt token behaves
// like an @file mention on backspace: deleting the token takes its chip with
// it, so the resolved body stops being sent along with a message that no
// longer refers to it.
func TestAtomicBackspaceRemovesPromptChip(t *testing.T) {
	t.Parallel()

	ws := &promptWorkspace{body: "body"}
	ws.ready = true
	m := newCompletionBackspaceUIWith(ws)
	m.textarea.SetValue("do /")
	m.completionsStartIndex = len("do ")

	runPromptCmd(t, m, m.attachMCPPrompt("gitea:review", "gitea", "review",
		map[string]string{"id": "42"}))
	require.Len(t, m.attachments.List(), 1)

	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})

	require.Equal(t, "do ", m.textarea.Value(), "the whole token goes at once")
	require.Empty(t, m.attachments.List(),
		"the chip must go with the token that referenced it")
}

// TestAttachMCPPromptDropsBlankArguments pins that a blank optional argument
// is "not supplied": absent from the token, absent from the chip's count, and
// never sent to the server. The arguments dialog returns every declared
// argument, blank ones included, so without filtering the chip claimed
// "(3 args)" beside a token showing one, and the server was handed
// empty-string values it never asked for.
func TestAttachMCPPromptDropsBlankArguments(t *testing.T) {
	t.Parallel()

	ws := &promptWorkspace{body: "body"}
	ws.ready = true
	m := newCompletionBackspaceUIWith(ws)
	m.textarea.SetValue("go /")
	m.completionsStartIndex = len("go ")

	runPromptCmd(t, m, m.attachMCPPrompt("gitea:review", "gitea", "review",
		map[string]string{"id": "42", "title": "", "body": "   "}))

	require.Equal(t, "go /gitea:review(id=42) ", m.textarea.Value())
	require.Equal(t, map[string]string{"id": "42"}, ws.gotArgs,
		"blank arguments must not reach the server")
	require.Len(t, m.attachments.List(), 1)
	require.Equal(t, 1, m.attachments.List()[0].PromptArgCount,
		"the count must match what the token shows")
}

// TestAttachMCPPromptEncodesWhitespaceInValues pins the token grammar against
// free-text argument values. ScanPromptTokens ends a token at the first
// space, so an unescaped value like "fix the bug" split the token in half —
// the exact failure the design claims to prevent.
func TestAttachMCPPromptEncodesWhitespaceInValues(t *testing.T) {
	t.Parallel()

	ws := &promptWorkspace{body: "body"}
	ws.ready = true
	m := newCompletionBackspaceUIWith(ws)
	m.textarea.SetValue("go /")
	m.completionsStartIndex = len("go ")

	runPromptCmd(t, m, m.attachMCPPrompt("gitea:review", "gitea", "review",
		map[string]string{"title": "fix the bug"}))

	got := m.textarea.Value()
	token := strings.TrimSpace(got[strings.Index(got, "/"):])
	require.NotContains(t, token, " ", "the token must survive a spaced value")
	require.Contains(t, token, "%20")
	// The server still gets the real value, not the encoded one.
	require.Equal(t, map[string]string{"title": "fix the bug"}, ws.gotArgs)
}

// TestAttachMCPPromptRollsBackOnResolveFailure pins that a prompt whose body
// cannot be resolved does not leave its token stranded in the editor. A
// stranded token is sent to the model as literal prose with none of the
// prompt behind it — a silently degraded turn rather than a blocked one.
func TestAttachMCPPromptRollsBackOnResolveFailure(t *testing.T) {
	t.Parallel()

	ws := &promptWorkspace{err: errors.New("server unavailable")}
	ws.ready = true
	m := newCompletionBackspaceUIWith(ws)
	m.textarea.SetValue("please /")
	m.completionsStartIndex = len("please ")

	cmd := m.attachMCPPrompt("gitea:review", "gitea", "review", nil)
	require.Equal(t, "please /gitea:review ", m.textarea.Value(),
		"the token is inserted optimistically")

	// Drain the command and feed the failure back through Update.
	var failure tea.Msg
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				run(sub)
			}
			return
		}
		if f, ok := msg.(promptResolveFailedMsg); ok {
			failure = f
		}
	}
	run(cmd)
	require.NotNil(t, failure, "a failed resolve must report back")
	m.Update(failure)

	require.Equal(t, "please ", m.textarea.Value(),
		"the token must be rolled back when its body never arrives")
	require.Empty(t, m.attachments.List())
}
