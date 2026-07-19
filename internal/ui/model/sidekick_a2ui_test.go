package model

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// sidekickA2UISurface is an <a2ui-json> block: a card wrapping a single text
// component, as the Sidekick's prompt biases it to emit for visual answers.
const sidekickA2UISurface = `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
	`{"component":"Card","id":"root","child":"t"},` +
	`{"component":"Text","id":"t","text":"Sidekick card"}` +
	`]}}</a2ui-json>`

// sidekickSidebarWidth is a representative narrow sidebar width for the
// inline-surface tests (#55).
const sidekickSidebarWidth = 40

func finishedAssistantMessage(text string) *message.Message {
	return &message.Message{
		ID:   "a1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	}
}

func TestRenderSidekickMessageA2UISurfaceInline(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	msg := finishedAssistantMessage("Here you go:\n\n" + sidekickA2UISurface)
	out := ansi.Strip(renderSidekickMessage(&sty, msg, sidekickSidebarWidth))

	// The surface renders inline (text pulled from the card's Text
	// component), with the prose preserved and the raw wire format hidden.
	require.Contains(t, out, "Sidekick card")
	require.Contains(t, out, "Here you go")
	require.NotContains(t, out, "a2ui-json")
	require.NotContains(t, out, "updateComponents")
}

func TestRenderSidekickMessageA2UIFitsSidebarWidth(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	msg := finishedAssistantMessage("A card with a fairly long lead-in sentence " +
		"that must wrap to the panel:\n\n" + sidekickA2UISurface)
	out := renderSidekickMessage(&sty, msg, sidekickSidebarWidth)

	for _, line := range strings.Split(out, "\n") {
		require.LessOrEqual(t, lipgloss.Width(line), sidekickSidebarWidth,
			"every rendered line must fit the sidebar width")
	}
}

func TestRenderSidekickMessagePlainTextKeepsAssistantStyle(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	msg := finishedAssistantMessage("Just a plain answer.")
	out := ansi.Strip(renderSidekickMessage(&sty, msg, sidekickSidebarWidth))
	require.Contains(t, out, "Just a plain answer.")
}

func TestRenderSidekickMessageTruncatedA2UIAlerts(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	// Finished message whose block never closed: alert instead of raw JSON.
	msg := finishedAssistantMessage("Card:\n\n<a2ui-json>{\"version\":\"v0.9\",\"updateComponents")
	out := ansi.Strip(renderSidekickMessage(&sty, msg, sidekickSidebarWidth))
	require.Contains(t, out, "couldn't render")
	require.NotContains(t, out, "updateComponents")

	// The same content mid-stream (no Finish part) is not an error yet —
	// it renders as plain text until the close tag arrives.
	streaming := &message.Message{
		ID:   "a2",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Card:\n\n<a2ui-json>{\"version\":\"v0.9\",\"updateComponents"},
		},
	}
	out = ansi.Strip(renderSidekickMessage(&sty, streaming, sidekickSidebarWidth))
	require.NotContains(t, out, "couldn't render")
}
