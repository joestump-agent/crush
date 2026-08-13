// a2ui_inline.go is the Sidekick panel's display-only A2UI render path.
//
// The main chat renders A2UI through AssistantMessageItem.renderContentWithA2UI,
// which keeps each surface's a2tea model alive on the item so it can receive
// focus and key input (#44). The Sidekick message list is a plain string-based
// scrollback with no per-item state to host live models, so surfaces there are
// rendered display-only in v1: each a2tea model is built, rendered once, and
// discarded (buttons and inputs draw as read-only visuals). This file shares
// the masking/repair/scan/alert helpers with the live path so both agree on
// what counts as a surface, a dropped block, and a truncation.

package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/joestump-agent/a2tea"
	"github.com/joestump-agent/a2tea/render"
)

// RenderA2UIInline renders assistant content that carries A2UI at the given
// width via the shared a2tea.Scan + render pipeline — the same one the main
// chat uses. The Sidekick panel calls it to draw surfaces inline in its
// message list at sidebar (narrow) width; surfaces there are display-only in
// v1 (no event routing), so buttons and inputs render as read-only visuals.
//
// Returns ok=false when the content carries no A2UI (and, for unfinished
// messages, no truncated block), leaving the caller to render the text its
// own way.
func RenderA2UIInline(sty *styles.Styles, content string, width int, finished bool) (out string, ok bool) {
	switch {
	case contentHasA2UI(content):
		return renderA2UIStatic(sty, content, width, finished, plainMarkdownRenderer(sty)), true
	case finished && contentHasUnclosedA2UI(content):
		// Generation was truncated mid-block: an <a2ui-json> tag never got
		// its closing partner. Show the alert instead of raw partial JSON.
		return renderTruncatedA2UIStatic(sty, content, width, plainMarkdownRenderer(sty)), true
	}
	return "", false
}

// plainMarkdownRenderer returns a whole-content markdown fallback for callers
// without a streaming prefix cache (the Sidekick panel). It acquires the
// shared renderer lock itself, so it must not be called with that lock held.
func plainMarkdownRenderer(sty *styles.Styles) func(content string, width int) string {
	return func(content string, width int) string {
		renderer := common.MarkdownRenderer(sty, width)
		mu := common.LockMarkdownRenderer(renderer)
		mu.Lock()
		defer mu.Unlock()
		out, err := renderer.Render(content)
		if err != nil {
			return strings.TrimSpace(content)
		}
		return trimGlamourMargins(out)
	}
}

// renderA2UIStatic is the display-only counterpart of
// AssistantMessageItem.renderContentWithA2UI: it scans, masks, and repairs
// identically, but renders each surface from a freshly built a2tea model that
// is discarded immediately (no live focus/key state). fallback renders the
// whole content as markdown when the scan fails; it is called without the
// shared renderer lock held.
func renderA2UIStatic(sty *styles.Styles, content string, width int, finished bool, fallback func(content string, width int) string) string {
	masked, codeReps := maskMarkdownCode(content)
	// Repair childless-but-labeled buttons on the raw JSON before parsing —
	// the stray label field is dropped by the typed parser (#47). Runs after
	// masking so button examples inside fenced code stay verbatim.
	masked = repairA2UIButtons(masked)

	parts, err := a2tea.Scan(masked)
	if err != nil {
		// Not parseable as A2UI — render everything as markdown so nothing is
		// lost.
		return fallback(content, width)
	}

	renderer := common.MarkdownRenderer(sty, width)
	mu := common.LockMarkdownRenderer(renderer)
	mu.Lock()
	defer mu.Unlock()

	renderMarkdown := func(text string) string {
		if strings.TrimSpace(text) == "" {
			return ""
		}
		// Restore masked code before markdown rendering.
		text = unmaskMarkdownCode(text, codeReps)
		out, err := renderer.Render(text)
		if err != nil {
			return strings.TrimSpace(text)
		}
		return trimGlamourMargins(out)
	}

	var b strings.Builder
	writeChunk := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}

	// a2tea's chrome renders with terminal defaults (monochrome by design),
	// so each surface is wrapped in a themed container to match the rest of
	// the chat. The a2tea model is sized to the container's inner width.
	surface := sty.Messages.A2UISurface
	innerWidth := max(width-surface.GetHorizontalFrameSize(), 1)
	for _, p := range parts {
		writeChunk(renderMarkdown(p.Text))
		if len(p.Messages) == 0 {
			continue
		}
		model, rerr := a2tea.Render(p.Messages)
		if rerr != nil {
			// Valid A2UI messages with nothing to draw (e.g. a data-model
			// update). Not an error worth alarming the user about.
			continue
		}
		rm, ok := model.(render.Model)
		if !ok {
			continue
		}
		rm.SetSize(innerWidth, 0)
		rendered := strings.TrimRight(rm.View().Content, "\n")
		writeChunk(surface.Width(max(width-surface.GetHorizontalBorderSize(), 1)).Render(rendered))
	}

	// Alert when the parser dropped a complete tagged block (#7) or when
	// generation was truncated mid-block (#5); protocol-only blocks get the
	// quiet notice instead (#168). Only for finished messages — while
	// streaming, an "unclosed" tag usually just means the close tag hasn't
	// arrived yet.
	if finished {
		stats := scanA2UIBlocks(masked)
		switch {
		case stats.dropped > 0 || stats.unclosed:
			writeChunk(renderA2UIAlertStatic(sty, width))
		case stats.protocolOnly > 0:
			writeChunk(renderA2UIProtocolNoticeStatic(sty, width))
		}
	}

	return b.String()
}

// renderTruncatedA2UIStatic is the display-only counterpart of
// AssistantMessageItem.renderTruncatedA2UI: the prose before the unclosed tag
// is rendered as markdown (via renderProse), and the truncated block is
// surfaced through the standard A2UI alert instead of leaving a wall of raw
// JSON (#5).
func renderTruncatedA2UIStatic(sty *styles.Styles, content string, width int, renderProse func(content string, width int) string) string {
	// Mask (not strip) markdown code: a fence or inline span containing an
	// <a2ui-json> example must not be mistaken for the truncation point, but
	// the code must stay in the rendered prose — stripping deleted it from
	// the message.
	masked, codeReps := maskMarkdownCode(content)
	idx := strings.Index(masked, "<a2ui-json>")

	var b strings.Builder
	if idx > 0 {
		prose := unmaskMarkdownCode(masked[:idx], codeReps)
		if strings.TrimSpace(prose) != "" {
			b.WriteString(renderProse(prose, width))
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(renderA2UIAlertStatic(sty, width))
	return b.String()
}

// renderA2UIAlertStatic is the display-only counterpart of
// AssistantMessageItem.renderA2UIAlert, styled identically.
func renderA2UIAlertStatic(sty *styles.Styles, width int) string {
	inner := max(width-2, 1)
	tag := sty.Messages.ErrorTag.Render("A2UI")
	title := sty.Messages.ErrorTitle.Render("couldn't render a UI block in this message")
	reason := sty.Messages.ErrorDetails.Width(inner).Render(
		"The A2UI content was malformed or used unsupported components.")
	return tag + " " + title + "\n\n" + reason
}

// renderA2UIProtocolNoticeStatic is the display-only counterpart of
// AssistantMessageItem.renderA2UIProtocolNotice (#168).
func renderA2UIProtocolNoticeStatic(sty *styles.Styles, width int) string {
	inner := max(width-2, 1)
	return sty.Messages.ErrorDetails.Width(inner).Render(
		"This message includes A2UI protocol data with nothing to display.")
}

// --- Interactive dashboard surface -----------------------------------------
//
// The Sidekick panel's pinned dashboard slot is a LIVE a2tea surface (unlike
// the display-only chat scrollback above): the agent pushes a surface via the
// sidekick_update tool (#57), the slot holds the model, and the user can
// focus it, edit its fields, and submit its buttons (#56). These helpers back
// that slot.

// NewA2UIDashboardSurface builds a live a2tea surface model from agent-pushed
// dashboard content (the sidekick_update tool payload, #57). The content goes
// through the same masking, button repair, and scan as the main chat; the
// first part carrying renderable messages becomes the surface. Returns the
// live model, its A2UI surface ID, and ok=false when nothing renderable was
// found.
func NewA2UIDashboardSurface(content string) (render.Model, string, bool) {
	masked, _ := maskMarkdownCode(content)
	masked = repairA2UIButtons(masked)
	parts, err := a2tea.Scan(masked)
	if err != nil {
		return nil, "", false
	}
	for _, p := range parts {
		if len(p.Messages) == 0 {
			continue
		}
		model, err := a2tea.Render(p.Messages)
		if err != nil {
			continue
		}
		if rm, ok := model.(render.Model); ok {
			return rm, a2uiPartSurfaceID(p.Messages), true
		}
	}
	return nil, "", false
}

// RenderA2UISurfaceModel renders a live surface model inside the themed
// container at the given width — the same chrome the main chat wraps its
// surfaces in. The view reflects the model's current focus ring and edited
// values, so callers holding a live surface (the Sidekick dashboard slot)
// re-render through this on every frame.
func RenderA2UISurfaceModel(sty *styles.Styles, model render.Model, width int) string {
	surface := sty.Messages.A2UISurface
	innerWidth := max(width-surface.GetHorizontalFrameSize(), 1)
	model.SetSize(innerWidth, 0)
	rendered := strings.TrimRight(model.View().Content, "\n")
	return surface.Width(max(width-surface.GetHorizontalBorderSize(), 1)).Render(rendered)
}

// A2UISurfaceWantsKey reports whether a focused live surface should consume
// the key — the exported form of a2uiSurfaceWantsKey for hosts outside this
// package (the Sidekick dashboard slot).
func A2UISurfaceWantsKey(s render.Model, key tea.KeyMsg) bool {
	return a2uiSurfaceWantsKey(s, key)
}

// A2UISurfaceFieldValues reads a live surface's current field values (by
// component ID), or nil when the model cannot report them.
func A2UISurfaceFieldValues(s render.Model) map[string]any {
	if fv, ok := s.(interface{ FieldValues() map[string]any }); ok {
		return fv.FieldValues()
	}
	return nil
}

