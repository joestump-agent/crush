package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// backspace presses backspace once through the real key path.
func backspace(m *UI) {
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
}

// TestBackspaceDeletesWholeTokenAfterIntervalEdits covers the first reported
// bug: a highlighted token should vanish in one backspace whenever the cursor
// sits at its end, not only in the instant after it was inserted.
//
// The existing atomic-backspace record (#250) is armed by an insertion and
// disarmed by the next keystroke, so a user who types a mention, keeps
// writing, then comes back and backspaces into it deletes one character at a
// time — even though the token is still rendered as a single highlighted
// unit. What the editor draws as one thing has to delete as one thing.
func TestBackspaceDeletesWholeTokenAfterIntervalEdits(t *testing.T) {
	// The known-name set is process-wide, so this test and its neighbours
	// that touch it do not run in parallel.
	common.SetPromptSkillNames([]string{"code-review", "gitea:review"})
	t.Cleanup(func() { common.SetPromptSkillNames(nil) })

	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"file mention", "look at @internal/foo.go", "look at "},
		{"skill token", "please /code-review", "please "},
		{"prompt token with arguments", "do /gitea:review(id=42)", "do "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newCompletionBackspaceUI()
			m.textarea.SetValue(tc.value)
			m.textarea.MoveToEnd()
			// No completion is armed: this is a cold prompt, exactly as it
			// would be after typing something else in between.
			m.clearCompletionRange()

			backspace(m)

			require.Equal(t, tc.want, m.textarea.Value())
		})
	}
}

// TestBackspaceLeavesOrdinaryTextAlone is the control for the above: backspace
// must still delete one character when the cursor is not at a token's end.
func TestBackspaceLeavesOrdinaryTextAlone(t *testing.T) {
	common.SetPromptSkillNames([]string{"code-review"})
	t.Cleanup(func() { common.SetPromptSkillNames(nil) })

	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"plain prose", "hello world", "hello worl"},
		{"unknown slash word", "read /tmp/out.log", "read /tmp/out.lo"},
		{"trailing space after a token", "see @a.go ", "see @a.go"},
		{"cursor past a token deletes one char", "see @a/b.go now", "see @a/b.go no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newCompletionBackspaceUI()
			m.textarea.SetValue(tc.value)
			m.textarea.MoveToEnd()
			m.clearCompletionRange()

			backspace(m)

			require.Equal(t, tc.want, m.textarea.Value())
		})
	}
}

// TestDeletingMentionCharByCharRemovesAttachment covers the second reported
// bug: an @file attachment should go when its mention is gone from the
// prompt, however the mention was removed.
//
// Attachment removal was wired only to the atomic-backspace path, so a user
// who deletes the mention one character at a time — or edits it into
// something else — ends up with a chip whose mention no longer exists. The
// file is then still sent to the model with nothing in the message referring
// to it.
func TestDeletingMentionCharByCharRemovesAttachment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "notes.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

	m := newCompletionBackspaceUI()
	m.textarea.SetValue("read @")
	m.completionsStartIndex = len("read ")
	runInsertCmdForTest(t, m, m.insertFileCompletion(path))
	require.Len(t, m.attachments.List(), 1, "precondition: the mention attached the file")

	// Trim the mention one character at a time, the way a user editing the
	// middle of their prompt would, and press a key after each edit. The
	// attachment must not outlive the mention becoming incomplete.
	full := m.textarea.Value()
	m.clearCompletionRange()
	for cut := 1; cut <= 3; cut++ {
		m.textarea.SetValue(strings.TrimSuffix(full, " ")[:len(strings.TrimSuffix(full, " "))-cut])
		m.textarea.MoveToEnd()
		m.clearCompletionRange()
		m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	}

	require.Empty(t, m.attachments.List(),
		"an attachment whose mention is no longer intact must not still be sent")
}

// TestEditingMentionIntoSomethingElseRemovesAttachment is the same rule
// reached by a different edit: the mention is not deleted but changed, so it
// no longer refers to the attached file.
func TestEditingMentionIntoSomethingElseRemovesAttachment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "notes.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

	m := newCompletionBackspaceUI()
	m.textarea.SetValue("read @")
	m.completionsStartIndex = len("read ")
	runInsertCmdForTest(t, m, m.insertFileCompletion(path))
	require.Len(t, m.attachments.List(), 1)

	m.clearCompletionRange()
	m.textarea.SetValue("read something else entirely")
	m.textarea.MoveToEnd()
	backspace(m)

	require.Empty(t, m.attachments.List(),
		"the attachment's mention is gone, so the attachment must go too")
}

// TestUnrelatedAttachmentsSurviveEditing pins the guard rail: attachments that
// never came from a mention — a pasted image, a dropped file — have no token
// in the prompt to match, and must not be swept up by the reconciliation.
func TestUnrelatedAttachmentsSurviveEditing(t *testing.T) {
	t.Parallel()

	m := newCompletionBackspaceUI()
	m.attachments.Update(message.Attachment{
		FilePath: "/pasted/image.png",
		FileName: "image.png",
		MimeType: "image/png",
	})
	require.Len(t, m.attachments.List(), 1)

	m.textarea.SetValue("describe this")
	m.textarea.MoveToEnd()
	m.clearCompletionRange()
	backspace(m)

	require.Len(t, m.attachments.List(), 1,
		"a pasted attachment has no mention and must survive prompt edits")
}

// runInsertCmdForTest drains an insert command and feeds any attachment it
// produces into the toolbar, so tests exercise the real fileCmd rather than a
// fabricated attachment.
func runInsertCmdForTest(t *testing.T, m *UI, cmd tea.Cmd) {
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
