package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"

	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// a2uiSurface is an <a2ui-json> block: a card wrapping a single text component.
const a2uiSurface = `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
	`{"component":"Card","id":"root","child":"t"},` +
	`{"component":"Text","id":"t","text":"Hello from A2UI"}` +
	`]}}</a2ui-json>`

func TestContentHasA2UI(t *testing.T) {
	t.Parallel()
	require.True(t, contentHasA2UI("here you go\n"+a2uiSurface))
	require.False(t, contentHasA2UI("just a normal ```json\n{}\n``` block"))
	require.False(t, contentHasA2UI("plain prose"))
}

// --- Issue #6: fenced code blocks should not render as live surfaces ---

func TestContentHasA2UIIgnoresFencedCode(t *testing.T) {
	t.Parallel()
	// A tagged block inside a fenced code block is an example, not live UI.
	fenced := "Here is an example:\n\n```json\n" + a2uiSurface + "\n```\n\nDone."
	require.False(t, contentHasA2UI(fenced))
}

func TestRenderContentWithA2UIFencedCodeNotLiveSurface(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := "Example:\n\n```json\n" + a2uiSurface + "\n```\n\nAnd some text."
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	// The fenced example is rendered as code — the raw tags and JSON are
	// visible as text, NOT extracted as a live surface.
	require.Contains(t, plain, "a2ui-json")
	// No alert — the fenced block is just an example, not a dropped surface.
	require.NotContains(t, plain, "couldn't render")
	// The surrounding text is preserved.
	require.Contains(t, plain, "Example")
	require.Contains(t, plain, "And some text")
}

func TestRenderContentWithA2UIRealSurfaceNextToFencedExample(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	// A real surface followed by a fenced example: both should render
	// correctly — the real surface as live, the fenced example as code.
	content := "Real: " + a2uiSurface + "\n\nExample:\n\n```json\n" + a2uiSurface + "\n```"
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	require.Contains(t, plain, "Hello from A2UI") // real surface rendered
}

// --- Issue #96: tag mentions in inline code must not be scanned ---

func TestContentHasA2UIIgnoresInlineCode(t *testing.T) {
	t.Parallel()
	// A complete-looking tag pair in inline code is documentation, not live UI.
	require.False(t, contentHasA2UI("Wrap the payload in `<a2ui-json>` and `</a2ui-json>` tags."))
	// Double-backtick spans too.
	require.False(t, contentHasA2UI("Wrap it in ``<a2ui-json>`` and ``</a2ui-json>``."))
	// A whole example block quoted in one inline span.
	require.False(t, contentHasA2UI("Like this: `"+a2uiSurface+"`"))
}

func TestContentHasUnclosedA2UIIgnoresInlineCode(t *testing.T) {
	t.Parallel()
	// A lone open tag in inline code is not a truncated block — without this,
	// renderTruncatedA2UI chops the message at the mention.
	require.False(t, contentHasUnclosedA2UI("Use the `<a2ui-json>` tag to open a block. More prose."))
}

func TestRenderContentWithA2UIInlineCodeMentionPreserved(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	// The exact repro from #96: prose explaining the format, with the tag pair
	// in inline code. The scanner used to consume the pair, swallowing the
	// prose between the mentions and raising a false alert.
	content := "Wrap the payload in `<a2ui-json>` and `</a2ui-json>` tags, then send it."
	plain := ansi.Strip(item.renderContentWithA2UI(content, 80, true))

	require.Contains(t, plain, "<a2ui-json>", "the mention must render verbatim")
	require.Contains(t, plain, "</a2ui-json>")
	require.Contains(t, plain, "then send it", "prose after the mention must survive")
	require.NotContains(t, plain, "couldn't render", "a documentation mention is not a dropped block")
	require.NotContains(t, plain, "\x00", "mask placeholders must not leak")
}

func TestRenderContentWithA2UIRealSurfaceNextToInlineMention(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	// A live surface and an inline-code mention side by side: the surface
	// renders, the mention stays prose, and no alert fires.
	content := "I used the `<a2ui-json>` tag:\n\n" + a2uiSurface + "\n\nDone."
	plain := ansi.Strip(item.renderContentWithA2UI(content, 80, true))

	require.Contains(t, plain, "Hello from A2UI", "the real surface must render")
	require.Contains(t, plain, "<a2ui-json>", "the inline mention must stay visible prose")
	require.Contains(t, plain, "Done")
	require.NotContains(t, plain, "couldn't render")
}

func TestMaskMarkdownCodeLeavesPlainInlineCodeAlone(t *testing.T) {
	t.Parallel()

	// Inline code without A2UI markers is not masked — a live surface whose
	// JSON strings contain backticks must reach the parser byte-for-byte.
	content := "Run `go test` first. " + a2uiSurface
	masked, reps := maskMarkdownCode(content)
	require.Equal(t, content, masked)
	require.Empty(t, reps)
}

// --- Issue #5: truncated mid-block should show alert ---

func TestContentHasUnclosedA2UI(t *testing.T) {
	t.Parallel()
	require.True(t, contentHasUnclosedA2UI("text <a2ui-json>{\"version\":\"v0"))
	require.False(t, contentHasUnclosedA2UI("text "+a2uiSurface))
	require.False(t, contentHasUnclosedA2UI("plain prose"))
	// Unclosed tag inside a fenced block is not a truncation.
	require.False(t, contentHasUnclosedA2UI("```json\n<a2ui-json>{bad\n```"))
	// Complete surface followed by a second truncated block.
	require.True(t, contentHasUnclosedA2UI(a2uiSurface+"\n\n<a2ui-json>{\"version\":\"v0"))
}

func TestRenderTruncatedA2UI(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := "Here is a card:\n\n<a2ui-json>{\"version\":\"v0.9\",\"updateComponents"
	out := item.renderTruncatedA2UI(content, 80)
	plain := ansi.Strip(out)

	// The prose before the truncated tag is preserved.
	require.Contains(t, plain, "Here is a card")
	// The alert is shown.
	require.Contains(t, plain, "couldn't render")
	// The raw partial JSON is NOT shown.
	require.NotContains(t, plain, "updateComponents")
}

// --- Issue #7: bare JSON should not mask dropped tagged block alert ---

func TestRenderContentWithA2UIMalformedShowsAlert(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	// Malformed JSON inside the tags: a2uistream drops it (no messages), so
	// crush must alert rather than silently losing the block.
	content := "Look: <a2ui-json>{not valid json}</a2ui-json>"
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	require.Contains(t, plain, "A2UI")
	require.Contains(t, plain, "couldn't render")
	// The surrounding prose is still there.
	require.Contains(t, plain, "Look")
}

func TestRenderContentWithA2UIMalformedNotMaskedByBareJSON(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	// One malformed tagged block + one bare JSON surface. The bare JSON must
	// not offset the count and suppress the alert for the dropped tagged
	// block.
	bareJSON := `{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Text","id":"t","text":"bare"}]}}`
	content := "<a2ui-json>{not valid json}</a2ui-json>\n\n" + bareJSON
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	require.Contains(t, plain, "couldn't render") // the malformed tagged block alerted
}

// --- Issue #47: childless-but-labeled buttons are repaired on the raw JSON ---

// TestRenderContentWithA2UIRepairsTextOnButton pins the DoD for #47: a model
// reply carrying the {"component":"Button","text":"Send"} anti-pattern (label
// in a text field, no child) renders the intended labels — not IDs, not
// "missing component" placeholders.
func TestRenderContentWithA2UIRepairsTextOnButton(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := `Form: <a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Column","id":"root","children":["btn1","btn2"]},` +
		`{"component":"Button","id":"btn1","text":"Send"},` +
		`{"component":"Button","id":"btn2","text":"Cancel"}` +
		`]}}</a2ui-json>`
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	require.Contains(t, plain, "Send", "the button's intended label must render")
	require.Contains(t, plain, "Cancel", "the second button's label must render")
	require.NotContains(t, plain, "btn1", "the button must not fall back to its ID")
	require.NotContains(t, plain, "missing component")
	require.NotContains(t, plain, "couldn't render")
}

func TestRepairA2UIButtonsSynthesizesChildText(t *testing.T) {
	t.Parallel()

	content := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Button","id":"btn1","text":"Send"}]}}</a2ui-json>`
	repaired := repairA2UIButtons(content)

	body := strings.TrimSuffix(strings.TrimPrefix(repaired, "<a2ui-json>"), "</a2ui-json>")
	var msg struct {
		UpdateComponents struct {
			Components []map[string]any `json:"components"`
		} `json:"updateComponents"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &msg))

	comps := msg.UpdateComponents.Components
	require.Len(t, comps, 2, "a Text component must be added for the label")

	btn := comps[0]
	require.Equal(t, "Button", btn["component"])
	require.Equal(t, "btn1-label", btn["child"], "the button must point at the synthesized Text")
	require.NotContains(t, btn, "text", "the stray text field must be removed")

	label := comps[1]
	require.Equal(t, "Text", label["component"])
	require.Equal(t, "btn1-label", label["id"])
	require.Equal(t, "Send", label["text"])
}

func TestRepairA2UIButtonsLabelFieldAndIDCollision(t *testing.T) {
	t.Parallel()

	// The same anti-pattern with a `label` field instead of `text`, plus an
	// existing component already occupying the derived label id.
	content := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Text","id":"btn1-label","text":"taken"},` +
		`{"component":"Button","id":"btn1","label":"Send"}]}}</a2ui-json>`
	repaired := repairA2UIButtons(content)

	require.Contains(t, repaired, `"child":"btn1-label-2"`, "the synthesized id must not collide")
	require.Contains(t, repaired, `"id":"btn1-label-2"`)
	require.NotContains(t, repaired, `"label":"Send"`, "the stray label field must be removed")
}

// TestRepairA2UIButtonsLeavesValidUntouched pins the surgical guarantee:
// content whose buttons already use a child Text id passes through repair
// byte-for-byte unchanged, as does anything else the repair has no business
// rewriting.
func TestRepairA2UIButtonsLeavesValidUntouched(t *testing.T) {
	t.Parallel()

	valid := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Button","id":"btn","child":"lbl"},` +
		`{"component":"Text","id":"lbl","text":"OK"}]}}</a2ui-json>`
	require.Equal(t, valid, repairA2UIButtons(valid))

	// A button with BOTH child and text keeps its child (text is the parser's
	// problem to drop, not ours to rewire).
	both := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Button","id":"btn","child":"lbl","text":"stray"},` +
		`{"component":"Text","id":"lbl","text":"OK"}]}}</a2ui-json>`
	require.Equal(t, both, repairA2UIButtons(both))

	// Inline-nested child (an object, another anti-pattern) is not rewritten;
	// it stays on the existing error path.
	nested := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Button","id":"btn","child":{"component":"Text","text":"X"}}]}}</a2ui-json>`
	require.Equal(t, nested, repairA2UIButtons(nested))

	// The canonical card surface is untouched too.
	require.Equal(t, "prose "+a2uiSurface, repairA2UIButtons("prose "+a2uiSurface))

	// Plain prose without any block is returned as-is.
	require.Equal(t, "no blocks here", repairA2UIButtons("no blocks here"))
}

// TestRepairA2UIButtonsMalformedUnchanged: undecodable JSON passes through
// verbatim so the existing malformed-block alert still fires downstream.
func TestRepairA2UIButtonsMalformedUnchanged(t *testing.T) {
	t.Parallel()

	malformed := `before <a2ui-json>{"component":"Button","text":"Send"</a2ui-json> after`
	require.Equal(t, malformed, repairA2UIButtons(malformed))

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}
	plain := ansi.Strip(item.renderContentWithA2UI(malformed, 80, true))
	require.Contains(t, plain, "couldn't render", "malformed A2UI must still alert")
}

// TestRenderContentWithA2UIValidButtonStillRenders: a correctly authored
// child-based button renders its label with the repair in the path.
func TestRenderContentWithA2UIValidButtonStillRenders(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[` +
		`{"component":"Button","id":"btn","child":"lbl"},` +
		`{"component":"Text","id":"lbl","text":"Acknowledge"}]}}</a2ui-json>`
	plain := ansi.Strip(item.renderContentWithA2UI(content, 80, true))

	require.Contains(t, plain, "Acknowledge")
	require.NotContains(t, plain, "couldn't render")
}

// --- Existing tests ---

func TestRenderContentWithA2UI(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := "Here is a card:\n\n" + a2uiSurface + "\n\nAnything else?"
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	// The A2UI surface renders (text pulled from the card's Text component)...
	require.Contains(t, plain, "Hello from A2UI")
	// ...and the surrounding prose is preserved on both sides.
	require.Contains(t, plain, "Here is a card")
	require.Contains(t, plain, "Anything else")
	// The raw A2UI JSON / tags are NOT shown verbatim.
	require.NotContains(t, plain, "a2ui-json")
	require.NotContains(t, plain, "updateComponents")
	// No alert when the surface rendered fine.
	require.NotContains(t, plain, "couldn't render")
}

// TestRenderContentWithA2UIThemedContainer asserts the rendered surface is
// wrapped in the themed A2UISurface container (#46): the surface line sits
// between the container's vertical border runes, under a top border and above
// a bottom border. Prose outside the surface stays unboxed.
func TestRenderContentWithA2UIThemedContainer(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := "Here is a card:\n\n" + a2uiSurface + "\n\nAnything else?"
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	// Container corners from the rounded border are present.
	require.Contains(t, plain, "╭")
	require.Contains(t, plain, "╰")

	// The surface content line is enclosed by the container's side borders.
	var surfaceLine string
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "Hello from A2UI") {
			surfaceLine = strings.TrimRight(line, " ")
			break
		}
	}
	require.NotEmpty(t, surfaceLine, "surface content line not found")
	require.True(t, strings.HasPrefix(surfaceLine, "│"), "surface line should start with container border: %q", surfaceLine)
	require.True(t, strings.HasSuffix(surfaceLine, "│"), "surface line should end with container border: %q", surfaceLine)

	// Prose outside the surface is not boxed.
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "Here is a card") || strings.Contains(line, "Anything else") {
			require.False(t, strings.Contains(line, "│"), "prose should not be inside the container: %q", line)
		}
	}
}

func TestRenderContentWithA2UIMixedGoodAndBadAlerts(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	// One valid surface plus one malformed block: the good one renders AND the
	// dropped one is still surfaced via an alert (not silently lost).
	content := "ok: " + a2uiSurface + " bad: <a2ui-json>{nope}</a2ui-json>"
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	require.Contains(t, plain, "Hello from A2UI") // the good surface rendered
	require.Contains(t, plain, "couldn't render") // the bad block alerted
}

// TestDroppedBlockAlert_UnrecognizedValidJSON pins the parser-agreement fix:
// {"foo":1} is valid JSON — the old single-object unmarshal check passed it —
// but the A2UI parser silently drops it (no recognized message), so the user
// lost the block with no alert. The dropped-block check now runs each block
// through the same parser rendering uses.
func TestDroppedBlockAlert_UnrecognizedValidJSON(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := `Look: <a2ui-json>{"foo": 1}</a2ui-json>`
	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)

	require.Contains(t, plain, "couldn't render",
		"valid-but-unrecognized JSON is dropped by the parser and must alert")
}

// TestNoDroppedBlockAlert_MultiMessageBody pins the other direction: a tagged
// body carrying multiple newline-delimited A2UI messages renders fine, so no
// alert may appear beside the working surface (the old single-object
// unmarshal false-positived here).
func TestNoDroppedBlockAlert_MultiMessageBody(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	body := `{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[{"component":"Text","id":"root","text":"First"}]}}
{"version":"v0.9","updateDataModel":{"surfaceId":"s","path":"/x","value":"y"}}`
	content := "Here: <a2ui-json>" + body + "</a2ui-json>"

	// Sanity: the parser really does yield messages for this body.
	require.Zero(t, countDroppedTaggedBlocks(content),
		"multi-message body must not count as dropped")

	out := item.renderContentWithA2UI(content, 80, true)
	plain := ansi.Strip(out)
	require.Contains(t, plain, "First", "the surface must render")
	require.NotContains(t, plain, "couldn't render",
		"no alert may appear next to a successfully rendered surface")
}

// TestNoAlertWhileStreaming pins the streaming gate: with one complete
// surface rendered and a second block still open (its close tag not yet
// arrived), an unfinished message must NOT flash the alert; the same content
// on a finished message must show it.
func TestNoAlertWhileStreaming(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[{"component":"Text","id":"root","text":"Done"}]}}</a2ui-json>
And a second one: <a2ui-json>{"version":"v0.9","updateComp`

	streaming := ansi.Strip(item.renderContentWithA2UI(content, 80, false))
	require.Contains(t, streaming, "Done", "the complete surface renders mid-stream")
	require.NotContains(t, streaming, "couldn't render",
		"no alert while the close tag simply hasn't arrived yet")

	finished := ansi.Strip(item.renderContentWithA2UI(content, 80, true))
	require.Contains(t, finished, "couldn't render",
		"a finished message with a dangling open tag must alert")
}

// TestRenderTruncatedA2UIPreservesFencedCode pins the fence fix: prose before
// the truncated block may contain fenced code, which must survive rendering —
// the old implementation stripped fences from the whole message, deleting the
// user-visible code block.
func TestRenderTruncatedA2UIPreservesFencedCode(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := "Example:\n\n```go\nfunc keepMe() {}\n```\n\nForm: <a2ui-json>{\"version\":\"v0.9\",\"updateComp"
	out := ansi.Strip(item.renderTruncatedA2UI(content, 80))

	require.Contains(t, out, "keepMe", "fenced code before the truncated block must be preserved")
	require.Contains(t, out, "couldn't render")
	require.NotContains(t, out, "updateComp", "raw truncated JSON must not leak")
}

// TestContentCache_FinishedRenderNotServedFromStreamingEntry pins the cache
// key folding in the finish state: the last streaming delta and the Finish
// part often carry byte-identical text, so without finish state in the key
// the no-alert render cached mid-stream would be served forever and the
// dropped-block alert the finished gate promises could never appear.
func TestContentCache_FinishedRenderNotServedFromStreamingEntry(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := &AssistantMessageItem{sty: &sty}

	content := `<a2ui-json>{"version":"v0.9","updateComponents":{"surfaceId":"s","components":[{"component":"Text","id":"root","text":"Done"}]}}</a2ui-json>
And a second one: <a2ui-json>{"version":"v0.9","updateComp`

	// Final content delta arrives while the message is still streaming;
	// the no-alert render lands in the section cache.
	item.message = &message.Message{
		ID:    "m-finish-key",
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: content}},
	}
	streaming := ansi.Strip(item.cachedContent(80))
	require.NotContains(t, streaming, "couldn't render", "no alert while streaming")

	// The Finish part lands with the text unchanged. The finished render
	// must re-render and alert, not serve the streaming cache entry.
	item.message = &message.Message{
		ID:   "m-finish-key",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: content},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	}
	finished := ansi.Strip(item.cachedContent(80))
	require.Contains(t, finished, "couldn't render",
		"finished message with a dangling block must alert even when its text matches the cached streaming render")
}
