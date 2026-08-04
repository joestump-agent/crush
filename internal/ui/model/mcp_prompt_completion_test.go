package model

import (
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
	gotArgs  map[string]string
	gotName  string
	gotPromp string
}

func (w *promptWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	w.gotName, w.gotPromp, w.gotArgs = clientID, promptID, args
	return w.body, nil
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
	require.Equal(t, "cairn:run_capture", att.FilePath)
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
