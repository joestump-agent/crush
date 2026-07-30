package model

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// TestHandlePasteMsg_EmptyContentRoutesToClipboardImage is a regression test
// for GitHub issue #197: CMD+V paste of image from clipboard does nothing on
// macOS.
//
// When the system clipboard holds an image (not text), the terminal's paste
// produces an empty tea.PasteMsg. Before the fix, handlePasteMsg would flow
// through the text-paste path and silently do nothing (empty text insert).
// After the fix, empty paste content delegates to pasteImageFromClipboard so
// image data on the clipboard is attached instead.
//
// This test verifies the routing decision: empty paste content must return
// the pasteImageFromClipboard command (identifiable by its function pointer),
// not the normal text-paste command.
func TestHandlePasteMsg_EmptyContentRoutesToClipboardImage(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	u.textarea.SetValue("existing text")

	// Empty paste - simulates clipboard holding an image on macOS.
	cmd := u.handlePasteMsg(tea.PasteMsg{Content: ""})

	// The textarea value must be unchanged.
	require.Equal(t, "existing text", u.textarea.Value(),
		"empty paste must not modify textarea content; it should route to clipboard image fallback")

	// The returned command must be the pasteImageFromClipboard method value,
	// proving the clipboard fallback path was taken (not the text insert path).
	require.NotNil(t, cmd, "empty paste must return a command (the clipboard fallback)")
	expectedFunc := reflect.ValueOf(u.pasteImageFromClipboard).Pointer()
	actualFunc := reflect.ValueOf(cmd).Pointer()
	require.Equal(t, expectedFunc, actualFunc,
		"empty paste must route to pasteImageFromClipboard, not the text insertion path")
}

// TestHandlePasteMsg_WhitespaceOnlyContentRoutesToClipboardImage ensures that
// whitespace-only paste content (which the terminal may produce for a non-text
// clipboard) also routes to the clipboard image fallback.
func TestHandlePasteMsg_WhitespaceOnlyContentRoutesToClipboardImage(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	u.textarea.SetValue("original")

	// Whitespace-only paste - another form of empty clipboard content.
	cmd := u.handlePasteMsg(tea.PasteMsg{Content: "   \n\t  "})

	require.Equal(t, "original", u.textarea.Value(),
		"whitespace-only paste must not modify textarea content")
	require.NotNil(t, cmd, "whitespace-only paste must return the clipboard fallback command")
	expectedFunc := reflect.ValueOf(u.pasteImageFromClipboard).Pointer()
	actualFunc := reflect.ValueOf(cmd).Pointer()
	require.Equal(t, expectedFunc, actualFunc,
		"whitespace-only paste must route to pasteImageFromClipboard")
}

// TestHandlePasteMsg_NonEmptyContentInsertsText is the control test: real
// text paste content must still go through the normal text insertion path
// and must NOT route to the clipboard fallback.
func TestHandlePasteMsg_NonEmptyContentInsertsText(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.dialog = dialog.NewOverlay()
	u.textarea.SetValue("")

	cmd := u.handlePasteMsg(tea.PasteMsg{Content: "hello world"})

	require.Contains(t, u.textarea.Value(), "hello world",
		"non-empty paste must insert text into the textarea")

	// Non-empty text must NOT route to the clipboard image path.
	if cmd != nil {
		clipboardFunc := reflect.ValueOf(u.pasteImageFromClipboard).Pointer()
		actualFunc := reflect.ValueOf(cmd).Pointer()
		require.NotEqual(t, clipboardFunc, actualFunc,
			"non-empty paste must not route to clipboard image fallback")
	}
}
