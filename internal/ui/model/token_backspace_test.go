package model

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/completions"
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
func TestBackspacingMentionRemovesAttachment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "notes.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

	m := newCompletionBackspaceUI()
	m.textarea.SetValue("read @")
	m.completionsStartIndex = len("read ")
	runInsertCmdForTest(t, m, m.insertFileCompletion(path))
	require.Len(t, m.attachments.List(), 1, "precondition: the mention attached the file")

	// Cold prompt: no completion armed, exactly as after typing something
	// else and coming back to it.
	m.clearCompletionRange()
	m.textarea.SetValue("read @" + path)
	m.textarea.MoveToEnd()

	backspace(m)

	require.Equal(t, "read ", m.textarea.Value())
	require.Empty(t, m.attachments.List(),
		"deleting the mention must take its attachment with it")
}

// TestRepeatMentionKeepsAttachmentUntilLastGoes pins the dedupe case: one
// attachment serves both mentions, so it only goes when the last one does.
func TestRepeatMentionKeepsAttachmentUntilLastGoes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dup.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

	m := newCompletionBackspaceUI()
	m.textarea.SetValue("read @")
	m.completionsStartIndex = len("read ")
	runInsertCmdForTest(t, m, m.insertFileCompletion(path))
	require.Len(t, m.attachments.List(), 1)

	// Two mentions of the same file, one attachment between them.
	m.clearCompletionRange()
	m.textarea.SetValue("read @" + path + " and @" + path)
	m.textarea.MoveToEnd()

	backspace(m)
	require.Len(t, m.attachments.List(), 1,
		"one mention still names the file, so its attachment stays")

	// Remove the survivor too.
	m.textarea.SetValue("read @" + path)
	m.textarea.MoveToEnd()
	m.clearCompletionRange()
	backspace(m)
	require.Empty(t, m.attachments.List())
}

// TestLateAttachmentForDeletedMentionIsDropped pins the async case: a read
// that finishes after its mention was deleted must not land as an orphan chip
// with nothing in the prompt referring to it, and which no later edit could
// remove.
func TestLateAttachmentForDeletedMentionIsDropped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "slow.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

	m := newCompletionBackspaceUI()
	m.textarea.SetValue("read @")
	m.completionsStartIndex = len("read ")
	cmd := m.insertFileCompletion(path) // deliberately left undrained

	// Delete the mention before the read comes back.
	m.clearCompletionRange()
	m.textarea.SetValue("read @" + path)
	m.textarea.MoveToEnd()
	backspace(m)
	require.Equal(t, "read ", m.textarea.Value())

	// Now let the read land.
	runInsertCmdForTest(t, m, cmd)

	require.Empty(t, m.attachments.List(),
		"an attachment whose mention is already gone must not appear")
}

// TestEditingMentionLeavesAttachment pins a deliberate limitation.
//
// Removal is driven by deleting the token, not by reconciling the prompt
// after every keystroke. Reconciling cannot be done reliably — a path or
// resource title containing a space does not tokenize back to what was
// inserted, editing an MCP prompt's arguments changes the token, and the
// tokenizer's name set is re-primed asynchronously when MCP servers reload —
// and each of those made a live attachment vanish mid-composition. Erring
// toward keeping the attachment leaves a visible chip the user can delete;
// erring the other way silently drops data they believe they attached.
func TestEditingMentionLeavesAttachment(t *testing.T) {
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

	require.Len(t, m.attachments.List(), 1,
		"the chip stays and is removable by hand rather than being dropped silently")
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
			// Through Update, not straight into the toolbar: the drop rule
			// for a mention that is already gone lives on that path, and a
			// helper that bypasses it would test nothing.
			m.Update(att)
		}
	}
	run(cmd)
}

// TestBackspaceWhileCompletingDeletesOneChar pins that the whole-token rule
// stands down while the completions popup is open.
//
// Every "@word" tokenizes, including the query the user is still typing, so
// without this guard one backspace to fix a typo ate the entire mention and
// closed the popup — and the textarea has no undo to get it back.
func TestBackspaceWhileCompletingDeletesOneChar(t *testing.T) {
	t.Parallel()

	m := newCompletionBackspaceUI()
	m.textarea.SetValue("look at @int")
	m.textarea.MoveToEnd()
	m.clearCompletionRange()
	// The popup is filtering, as it would be while typing a mention.
	m.completions = completions.New(
		m.com.Styles.Completions.Normal,
		m.com.Styles.Completions.Focused,
		m.com.Styles.Completions.Match,
	)
	m.completionsOpen = true
	m.completionsTrigger = completions.TriggerFile
	m.completionsStartIndex = len("look at ")

	backspace(m)

	require.Equal(t, "look at @in", m.textarea.Value(),
		"a backspace mid-query must delete one character, not the whole query")
	require.True(t, m.completionsOpen, "the popup must stay open")
}
