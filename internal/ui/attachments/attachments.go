package attachments

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/x/ansi"
)

const maxFilename = 15

type Keymap struct {
	DeleteMode,
	DeleteAll,
	Escape key.Binding
}

func New(renderer *Renderer, keyMap Keymap) *Attachments {
	return &Attachments{
		keyMap:   keyMap,
		renderer: renderer,
	}
}

type Attachments struct {
	renderer *Renderer
	keyMap   Keymap
	list     []message.Attachment
	deleting bool
}

func (m *Attachments) List() []message.Attachment { return m.list }
func (m *Attachments) Reset()                     { m.list = nil }

// RemoveByFilePath removes the first attachment whose FilePath matches path.
// Used when an atomic backspace deletes a @file mention from the prompt so
// the corresponding attachment chip is removed alongside it.
func (m *Attachments) RemoveByFilePath(path string) {
	for i, att := range m.list {
		if att.FilePath == path {
			m.list = slices.Delete(m.list, i, i+1)
			return
		}
	}
}

func (m *Attachments) Update(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case message.Attachment:
		m.list = append(m.list, msg)
		return true
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, m.keyMap.DeleteMode):
			if len(m.list) > 0 {
				m.deleting = true
			}
			return true
		case m.deleting && key.Matches(msg, m.keyMap.Escape):
			m.deleting = false
			return true
		case m.deleting && key.Matches(msg, m.keyMap.DeleteAll):
			m.deleting = false
			m.list = nil
			return true
		case m.deleting:
			// Handle digit keys for individual attachment deletion.
			r := msg.Code
			if r >= '0' && r <= '9' {
				num := int(r - '0')
				if num < len(m.list) {
					m.list = slices.Delete(m.list, num, num+1)
				}
				m.deleting = false
			}
			return true
		}
	}
	return false
}

// HandleClick processes a mouse click at the given x offset within the
// attachment row. If the click lands on a remove button, the
// corresponding attachment is removed. It returns true if the click was
// handled.
func (m *Attachments) HandleClick(x int) bool {
	if m.deleting || len(m.list) == 0 {
		return false
	}
	idx := m.renderer.HitTestRemove(m.list, x)
	if idx >= 0 && idx < len(m.list) {
		m.list = slices.Delete(m.list, idx, idx+1)
		return true
	}
	return false
}

func (m *Attachments) Render(width int) string {
	// The editor is interactive, so the remove button is shown.
	return m.renderer.Render(m.list, m.deleting, true, width)
}

// Renderer returns the attachment renderer so callers can update its
// styles in place.
func (m *Attachments) Renderer() *Renderer { return m.renderer }

func NewRenderer(normalStyle, deletingStyle, imageStyle, textStyle, skillStyle, promptStyle, removeStyle lipgloss.Style) *Renderer {
	return &Renderer{
		normalStyle:   normalStyle,
		textStyle:     textStyle,
		imageStyle:    imageStyle,
		skillStyle:    skillStyle,
		promptStyle:   promptStyle,
		removeStyle:   removeStyle,
		deletingStyle: deletingStyle,
	}
}

// SetStyles updates the renderer styles in place.
func (r *Renderer) SetStyles(normalStyle, deletingStyle, imageStyle, textStyle, skillStyle, promptStyle, removeStyle lipgloss.Style) {
	r.normalStyle = normalStyle
	r.textStyle = textStyle
	r.imageStyle = imageStyle
	r.skillStyle = skillStyle
	r.promptStyle = promptStyle
	r.removeStyle = removeStyle
	r.deletingStyle = deletingStyle
}

type Renderer struct {
	normalStyle, textStyle, imageStyle, skillStyle, promptStyle, removeStyle, deletingStyle lipgloss.Style
	// bounds stores the X-coordinate ranges of each chip's remove
	// button from the most recent Render call, for mouse hit-testing.
	bounds []chipBounds
}

// chipBounds holds the rendered strings and the X-coordinate range of
// each chip's remove button for hit-testing.
type chipBounds struct {
	startX    int
	removeEnd int // exclusive end X of the remove button (0 if none)
}

// Render renders the attachment chips. Each chip shows an icon and a
// filename; when showRemove is true a remove button (✕) follows on the
// right, and in deleting mode that slot shows the numeral to press
// instead, so toggling delete-mode doesn't shift the chips. showRemove
// should be false for attachments on already-posted messages, where
// removal is not possible.
func (r *Renderer) Render(attachments []message.Attachment, deleting, showRemove bool, width int) string {
	var chips []string
	r.bounds = r.bounds[:0]

	removeStr := r.removeStyle.String()
	// Only reserve width for the remove button when it will be drawn.
	removeReserve := ""
	if showRemove {
		removeReserve = removeStr
	}
	// A nominal chip width, used only to size the trailing "N more…" marker
	// and to reserve room for it. Individual chips are measured as they are
	// built: chip labels are no longer a uniform width (a prompt chip shows
	// its full name plus an argument count), so a single width cannot decide
	// how many fit.
	maxItemWidth := lipgloss.Width(r.imageStyle.String() + r.normalStyle.Render(strings.Repeat("x", maxFilename)) + removeReserve)

	var offset int
	for i, att := range attachments {
		spec := r.chipSpecFor(att)
		filename := spec.label

		iconStr := spec.icon.String()
		nameStyle := r.normalStyle
		if !showRemove {
			// Without a remove button there is nothing to carry the
			// trailing margin that separates adjacent chips (the ✕'s
			// MarginRight does this on the editor path), so put it on the
			// filename instead. Otherwise posted messages with multiple
			// attachments render with their chip backgrounds touching.
			nameStyle = nameStyle.MarginRight(1)
		}
		// Bound the label to the room actually left. Prompt labels are
		// deliberately not capped at maxFilename, so without this a single
		// long one overruns the row and the trailing ✕ — and the overflow
		// marker — get eaten by the row-level truncation below, leaving
		// click regions pointing at cells that no longer show a button.
		budget := width - offset - lipgloss.Width(iconStr) - trailWidth(r, deleting, showRemove, removeStr, i)
		if budget > 0 && ansi.StringWidth(filename) > budget {
			filename = ansi.Truncate(filename, budget, "…")
		}
		nameStr := nameStyle.Render(filename)

		chipW := lipgloss.Width(iconStr) + lipgloss.Width(nameStr)

		// Measure the whole chip, trailing element included, before
		// committing to it. Chip widths vary by kind now, so the only
		// reliable cutoff is a running total. Reserve room for the
		// "N more…" marker whenever anything would be left over, and always
		// emit at least one chip however narrow the editor is.
		trailW := trailWidth(r, deleting, showRemove, removeStr, i)
		reserve := 0
		if i < len(attachments)-1 {
			reserve = maxItemWidth
		}
		if offset+chipW+trailW+reserve > width {
			// Nothing more fits. Report the remainder if there is room for
			// the marker; otherwise stop silently rather than overflow.
			if offset+maxItemWidth <= width {
				chips = append(chips, lipgloss.NewStyle().Width(maxItemWidth).
					Render(fmt.Sprintf("%d more…", len(attachments)-i)))
			}
			break
		}

		chips = append(chips, iconStr, nameStr)

		switch {
		case deleting:
			numStr := r.deletingStyle.Render(fmt.Sprintf("%d", i))
			chips = append(chips, numStr)
			offset += chipW + lipgloss.Width(numStr)
		case showRemove:
			chips = append(chips, removeStr)
			removeStart := offset + chipW
			removeW := lipgloss.Width(removeStr)
			// If the button carries a trailing margin it is the gap between
			// chips, not part of the button, so exclude it from the hit
			// region. (Currently the button uses padding rather than a
			// margin, so this subtracts zero, but stays correct if that
			// changes.)
			r.bounds = append(r.bounds, chipBounds{
				startX:    removeStart,
				removeEnd: removeStart + removeW - r.removeStyle.GetHorizontalMargins(),
			})
			offset = removeStart + removeW
		default:
			offset += chipW
		}
	}

	out := lipgloss.JoinHorizontal(lipgloss.Left, chips...)
	// Final guarantee. At least one chip is always drawn, however narrow the
	// editor, and a prompt chip's label is deliberately not truncated to the
	// filename budget — so a single wide chip in a narrow editor can still
	// exceed the space, and the caller lays this row out expecting it not to.
	// Truncating here bounds that case without imposing a budget on labels
	// in the ordinary case, where the running total above already fits them.
	if lipgloss.Width(out) > width {
		out = ansi.Truncate(out, width, "…")
	}
	return out
}

// trailWidth is the width of whatever follows a chip's label: the delete-mode
// numeral, the remove button, or nothing.
func trailWidth(r *Renderer, deleting, showRemove bool, removeStr string, i int) int {
	switch {
	case deleting:
		return lipgloss.Width(r.deletingStyle.Render(fmt.Sprintf("%d", i)))
	case showRemove:
		return lipgloss.Width(removeStr)
	default:
		return 0
	}
}

// HitTestRemove returns the index of the attachment whose remove button
// contains the given x coordinate, or -1 if none.
func (r *Renderer) HitTestRemove(_ []message.Attachment, x int) int {
	for i, b := range r.bounds {
		if x >= b.startX && x < b.removeEnd {
			return i
		}
	}
	return -1
}

// chipSpec is what one chip shows, decoupled from how it is laid out.
// Building a spec is the only thing a new attachment kind has to define: the
// layout below measures whatever label it produces, so a kind is free to be
// wider or narrower than a filename without the fit math or the remove-button
// hit regions going wrong.
type chipSpec struct {
	icon  lipgloss.Style
	label string
}

// chipRenderer turns one attachment into the chip that represents it.
type chipRenderer func(*Renderer, message.Attachment) chipSpec

// chipRenderers is the registry of chip presentations, keyed by attachment
// kind. Adding a chip type means adding an entry here and the function it
// points at — nothing in the layout, the fit math, or the hit-testing needs
// to know the kind exists.
//
// A kind with no entry falls back to fileChip, so an attachment produced by
// an older client, or by code that never set Kind, still renders sensibly
// rather than blank.
var chipRenderers = map[message.AttachmentKind]chipRenderer{
	message.AttachmentKindFile:      fileChip,
	message.AttachmentKindMCPPrompt: mcpPromptChip,
}

func (r *Renderer) chipSpecFor(a message.Attachment) chipSpec {
	render, ok := chipRenderers[a.Kind]
	if !ok {
		render = fileChip
	}
	return render(r, a)
}

// fileChip presents anything file-shaped: the basename only, truncated to a
// uniform budget so a row of them stays tidy, with the icon chosen by MIME
// type. MIME sniffing lives here rather than in the dispatch above, because
// it can only distinguish file-ish things from each other.
func fileChip(r *Renderer, a message.Attachment) chipSpec {
	label := filepath.Base(a.FileName)
	if ansi.StringWidth(label) > maxFilename {
		label = ansi.Truncate(label, maxFilename, "…")
	}
	return chipSpec{icon: r.icon(a), label: label}
}

// mcpPromptChip presents a resolved MCP prompt. The full prompt name is the
// point — "/gitea:foo…" would leave two prompts from the same server
// indistinguishable — so it is never truncated. Argument values live in the
// token in the editor, where there is room; the chip carries only how many
// there are.
func mcpPromptChip(r *Renderer, a message.Attachment) chipSpec {
	label := promptChipName(a.FileName)
	if a.PromptArgCount > 0 {
		label = fmt.Sprintf("%s (%d %s)", label, a.PromptArgCount,
			plural(a.PromptArgCount, "arg", "args"))
	}
	return chipSpec{icon: r.promptStyle, label: label}
}

// promptChipName normalizes what the chip shows for a prompt.
//
// In the composer the label is already "/server:prompt". A posted message
// rebuilds its attachments from the stored part, which carries only the
// removal key — "server:prompt#3", made unique per insertion — so the
// trailing counter comes off and the leading slash goes back on. Doing it
// here keeps every prompt-specific presentation rule in one place.
func promptChipName(name string) string {
	if i := strings.LastIndexByte(name, '#'); i > 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			name = name[:i]
		}
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return name
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (r *Renderer) icon(a message.Attachment) lipgloss.Style {
	if a.IsImage() {
		return r.imageStyle
	}
	if a.IsMarkdown() {
		return r.skillStyle
	}
	return r.textStyle
}
