package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/joestump-agent/a2tea"
)

// a2uiOpenTag and a2uiCloseTag delimit an inline A2UI block in assistant
// content — the wire format a2tea scans for.
const (
	a2uiOpenTag  = "<a2ui-json>"
	a2uiCloseTag = "</a2ui-json>"
)

var (
	fenceRe = regexp.MustCompile("(?s)```[^\n]*\n.*?```")
	// inlineCodeRe matches markdown inline code spans on a single line:
	// double-backtick spans first (their content may hold single backticks),
	// then single-backtick spans.
	inlineCodeRe = regexp.MustCompile("``[^\n]*?``|`[^`\n]+`")
)

// codeReplacement tracks a masked code span for later restoration.
type codeReplacement struct {
	placeholder string
	original    string
}

// maskMarkdownCode replaces markdown code with unique placeholders so a2tea's
// markdown-unaware scanner does not extract A2UI from code that is
// documentation, not live UI. Fenced code blocks are always masked (#6);
// inline code spans are masked only when they mention A2UI markers (#96) —
// masking every span would leak placeholders into legitimate surface JSON
// that happens to contain backticks. Use unmaskMarkdownCode to restore the
// originals after scanning.
func maskMarkdownCode(content string) (string, []codeReplacement) {
	var reps []codeReplacement
	mask := func(match string) string {
		p := fmt.Sprintf("\x00CODE%d\x00", len(reps))
		reps = append(reps, codeReplacement{p, match})
		return p
	}
	if strings.Contains(content, "```") {
		content = fenceRe.ReplaceAllStringFunc(content, mask)
	}
	if strings.Contains(content, "`") {
		content = inlineCodeRe.ReplaceAllStringFunc(content, func(match string) string {
			if !mentionsA2UI(match) {
				return match
			}
			return mask(match)
		})
	}
	return content, reps
}

// a2uiMarkers are the literals a2tea's scanner reacts to: the wire tag and
// the quoted A2UI message-type keys used for bare-JSON detection. Inline code
// naming any of these is documentation the scanner must not consume (#96).
var a2uiMarkers = []string{
	"a2ui-json",
	`"createSurface"`,
	`"updateComponents"`,
	`"updateDataModel"`,
	`"deleteSurface"`,
}

// mentionsA2UI reports whether s contains any marker the a2tea scanner
// could mistake for live A2UI content.
func mentionsA2UI(s string) bool {
	for _, m := range a2uiMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// unmaskMarkdownCode restores code spans replaced by maskMarkdownCode.
func unmaskMarkdownCode(text string, reps []codeReplacement) string {
	for _, r := range reps {
		text = strings.ReplaceAll(text, r.placeholder, r.original)
	}
	return text
}

// contentHasA2UI reports whether the assistant content carries any A2UI
// outside markdown code (fences and inline spans), so the renderer only takes
// the a2tea path when there is live UI to draw.
func contentHasA2UI(content string) bool {
	masked, _ := maskMarkdownCode(content)
	return a2tea.Contains(masked)
}

// hasUnclosedA2UITag reports whether content has more opening
// <a2ui-json> tags than closing </a2ui-json> tags — a truncated block
// (#5). This catches both a single unclosed block and the rarer case of
// a complete surface followed by a second truncated block. Only
// meaningful for finished messages; while streaming the close tag
// simply hasn't arrived yet.
func hasUnclosedA2UITag(content string) bool {
	return strings.Count(content, "<a2ui-json>") > strings.Count(content, "</a2ui-json>")
}

// contentHasUnclosedA2UI is the gate-level check (markdown code masked) for
// truncated A2UI blocks in finished messages.
func contentHasUnclosedA2UI(content string) bool {
	masked, _ := maskMarkdownCode(content)
	return hasUnclosedA2UITag(masked)
}

// countDroppedTaggedBlocks scans content for complete
// <a2ui-json>...</a2ui-json> pairs that yield no A2UI messages — blocks the
// parser drops. This is independent of bare-JSON parts, which must not mask
// the alert (#7).
//
// Each block is judged by running it through the SAME parser Scan uses, so
// this check agrees with rendering by construction. The previous
// implementation unmarshalled the body as a single JSON object, which
// disagreed with the parser in both directions: valid-but-unrecognized JSON
// ({"foo":1}) is silently dropped by the parser but unmarshals fine (missed
// alert), while array-wrapped or multiple newline-delimited messages render
// fine but fail a single-object unmarshal (false alert beside a working
// surface).
func countDroppedTaggedBlocks(content string) int {
	var dropped int
	s := content
	for {
		i := strings.Index(s, a2uiOpenTag)
		if i < 0 {
			break
		}
		s = s[i+len(a2uiOpenTag):]
		j := strings.Index(s, a2uiCloseTag)
		if j < 0 {
			break
		}
		body := s[:j]
		s = s[j+len(a2uiCloseTag):]

		messages := 0
		if parts, err := a2tea.Scan(a2uiOpenTag + body + a2uiCloseTag); err == nil {
			for _, p := range parts {
				messages += len(p.Messages)
			}
		}
		if messages == 0 {
			dropped++
		}
	}
	return dropped
}

// repairA2UIButtons rewrites the raw JSON inside <a2ui-json> blocks to fix a
// frequent model mistake (#47): a Button carrying its label in a `text` (or
// `label`) field instead of a child Text component. A2UI's Button has no such
// field in any schema version, so the label is silently dropped when the block
// is parsed into typed components — which is why this repair must run on the
// raw JSON, before a2tea.Scan.
//
// The repair is deliberately surgical: only a Button that has a non-empty
// text/label AND no child is rewritten — a Text component is synthesized from
// the stray label and the button's child pointed at it. Valid child-based
// buttons and every other component are left untouched, and a block whose JSON
// doesn't decode is returned verbatim so the existing error-alert path still
// applies.
func repairA2UIButtons(content string) string {
	if !strings.Contains(content, a2uiOpenTag) {
		return content
	}
	var b strings.Builder
	s := content
	for {
		i := strings.Index(s, a2uiOpenTag)
		if i < 0 {
			break
		}
		j := strings.Index(s[i+len(a2uiOpenTag):], a2uiCloseTag)
		if j < 0 {
			break
		}
		body := s[i+len(a2uiOpenTag) : i+len(a2uiOpenTag)+j]
		b.WriteString(s[:i+len(a2uiOpenTag)])
		b.WriteString(repairA2UIBody(body))
		b.WriteString(a2uiCloseTag)
		s = s[i+len(a2uiOpenTag)+j+len(a2uiCloseTag):]
	}
	b.WriteString(s)
	return b.String()
}

// repairA2UIBody repairs the JSON body of a single <a2ui-json> block. The body
// may hold one message object, an array of messages, or several
// newline-delimited messages — the same forms the a2tea parser accepts. If the
// body doesn't decode, or no button needs fixing, it is returned unchanged
// (preserving the author's formatting and the malformed-block alert path).
func repairA2UIBody(body string) string {
	dec := json.NewDecoder(strings.NewReader(body))
	var vals []any
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return body
		}
		vals = append(vals, v)
	}

	changed := false
	for _, v := range vals {
		switch m := v.(type) {
		case map[string]any:
			changed = repairA2UIMessage(m) || changed
		case []any:
			for _, e := range m {
				if mm, ok := e.(map[string]any); ok {
					changed = repairA2UIMessage(mm) || changed
				}
			}
		}
	}
	if !changed {
		return body
	}

	out := make([]string, 0, len(vals))
	for _, v := range vals {
		enc, err := json.Marshal(v)
		if err != nil {
			return body
		}
		out = append(out, string(enc))
	}
	return strings.Join(out, "\n")
}

// repairA2UIMessage fixes childless-but-labeled buttons inside one raw A2UI
// message map, reporting whether it changed anything. For each such button it
// moves the stray text/label into a new Text component (with a unique id
// derived from the button's) and points the button's child at it.
func repairA2UIMessage(msg map[string]any) bool {
	uc, ok := msg["updateComponents"].(map[string]any)
	if !ok {
		return false
	}
	comps, ok := uc["components"].([]any)
	if !ok {
		return false
	}

	ids := make(map[string]bool, len(comps))
	for _, c := range comps {
		if m, ok := c.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				ids[id] = true
			}
		}
	}

	var added []any
	for _, c := range comps {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := m["component"].(string); typ != "Button" {
			continue
		}
		switch child := m["child"].(type) {
		case nil: // absent (or JSON null): childless, repairable
		case string:
			if child != "" {
				continue // valid child-based button: leave untouched
			}
		default:
			continue // e.g. inline-nested object: not ours to rewrite
		}
		label, _ := m["text"].(string)
		if label == "" {
			label, _ = m["label"].(string)
		}
		if label == "" {
			continue
		}

		base := "label"
		if id, _ := m["id"].(string); id != "" {
			base = id + "-label"
		}
		labelID := base
		for n := 2; ids[labelID]; n++ {
			labelID = fmt.Sprintf("%s-%d", base, n)
		}
		ids[labelID] = true

		m["child"] = labelID
		delete(m, "text")
		delete(m, "label")
		added = append(added, map[string]any{
			"component": "Text",
			"id":        labelID,
			"text":      label,
		})
	}
	if len(added) == 0 {
		return false
	}
	uc["components"] = append(comps, added...)
	return true
}

// renderContentWithA2UI renders assistant content that contains A2UI. a2tea
// scans the content into ordered parts of prose text and typed A2UI messages;
// crush renders the prose as markdown and hands each part's messages to
// a2tea.Render, stitching the rendered surface in place.
//
// Markdown code (fences, and inline spans naming A2UI markers) is masked
// before scanning so that A2UI examples in documentation prose are not
// extracted as live surfaces (#6, #96). If any complete tag
// pair contains malformed JSON — a block a2tea drops silently — an alert
// element is appended (#7). An unclosed <a2ui-json> tag (truncated
// generation) also triggers the alert (#5).
//
// This deliberately bypasses the streaming-markdown prefix cache (which assumes
// a single glamour render per item) and renders each segment directly. The
// renderer is shared, so the whole multi-render sequence holds its lock.
func (a *AssistantMessageItem) renderContentWithA2UI(content string, width int, finished bool) string {
	masked, codeReps := maskMarkdownCode(content)
	// Repair childless-but-labeled buttons on the raw JSON before parsing —
	// the stray label field is dropped by the typed parser (#47). Runs after
	// masking so button examples inside fenced code stay verbatim.
	masked = repairA2UIButtons(masked)

	parts, err := a2tea.Scan(masked)
	if err != nil {
		// Not parseable as A2UI — render everything as markdown so nothing is
		// lost.
		return a.renderMarkdown(content, width)
	}

	renderer := common.MarkdownRenderer(a.sty, width)
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
	surface := a.sty.Messages.A2UISurface
	innerWidth := max(width-surface.GetHorizontalFrameSize(), 1)
	for _, p := range parts {
		writeChunk(renderMarkdown(p.Text))
		if len(p.Messages) == 0 {
			continue
		}
		model, err := a2tea.Render(p.Messages)
		if err != nil {
			// Valid A2UI messages with nothing to draw (e.g. a data-model
			// update). Not an error worth alarming the user about.
			continue
		}
		if sz, ok := model.(interface{ SetSize(width, height int) }); ok {
			sz.SetSize(innerWidth, 0)
		}
		rendered := strings.TrimRight(model.View().Content, "\n")
		writeChunk(surface.Width(max(width-surface.GetHorizontalBorderSize(), 1)).Render(rendered))
	}

	// Alert when a complete tag pair was dropped by the parser (#7) —
	// checked directly so bare-JSON parts cannot mask the count — or when
	// generation was truncated mid-block (#5). Only for finished messages:
	// while streaming, an "unclosed" tag usually just means the close tag
	// hasn't arrived yet, and flashing a red alert between flushes that then
	// vanishes reads as a glitch.
	if finished && (countDroppedTaggedBlocks(masked) > 0 || hasUnclosedA2UITag(masked)) {
		writeChunk(a.renderA2UIAlert(width))
	}

	return b.String()
}

// renderTruncatedA2UI handles a finished message whose <a2ui-json> block was
// never closed — generation was truncated mid-block (#5). The prose before
// the unclosed tag is rendered as markdown, and the truncated block is
// surfaced through the standard A2UI alert instead of leaving a wall of raw
// JSON.
func (a *AssistantMessageItem) renderTruncatedA2UI(content string, width int) string {
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
			b.WriteString(a.renderMarkdown(prose, width))
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(a.renderA2UIAlert(width))
	return b.String()
}

// renderA2UIAlert builds an alert element shown when content advertised A2UI but
// a2tea could not turn it into a surface. Styled in crush's existing
// error-message language.
func (a *AssistantMessageItem) renderA2UIAlert(width int) string {
	inner := max(width-2, 1)
	tag := a.sty.Messages.ErrorTag.Render("A2UI")
	title := a.sty.Messages.ErrorTitle.Render("couldn't render a UI block in this message")
	reason := a.sty.Messages.ErrorDetails.Width(inner).Render(
		"The A2UI content was malformed or used unsupported components.")
	return tag + " " + title + "\n\n" + reason
}
