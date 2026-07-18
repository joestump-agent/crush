package attachments

import (
	"fmt"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func newTestRenderer() *Renderer {
	sty := styles.CharmtonePantera()
	return NewRenderer(
		sty.Attachments.Normal,
		sty.Attachments.Deleting,
		sty.Attachments.Image,
		sty.Attachments.Text,
		sty.Attachments.Skill,
		sty.Attachments.Remove,
	)
}

func TestRender_IncludesRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "test.txt"},
	}
	out := r.Render(atts, false, true, 80)
	require.Contains(t, out, styles.RemoveIcon)
}

func TestRender_DeletingModeNoRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "test.txt"},
	}
	out := r.Render(atts, true, true, 80)
	require.NotContains(t, out, styles.RemoveIcon)
}

func TestRender_ShowRemoveFalseOmitsRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "no-change.png"},
	}
	out := r.Render(atts, false, false, 80)
	require.NotContains(t, out, styles.RemoveIcon,
		"posted-message attachments must not show a remove button")
	require.Empty(t, r.bounds,
		"no remove bounds should be recorded when the button is hidden")
	require.Equal(t, -1, r.HitTestRemove(atts, 0))
}

func TestRender_ShowRemoveFalseKeepsGapBetweenChips(t *testing.T) {
	t.Parallel()

	// Regression for the #134 + #135 interaction: #134 moved the trailing
	// margin onto the remove button, and #135 hides that button on posted
	// messages. Together, posted messages with multiple attachments lost the
	// margin that separated adjacent chips, so their backgrounds touched. The
	// filename must carry the margin when the remove button is hidden.
	//
	// White-box width check: the visible width of the two chips without any
	// separator is icon+filename per chip. With the fix each posted chip adds
	// a 1-column trailing margin, so the rendered row is exactly two columns
	// wider. Stripping ANSI can't detect this (a margin space and a
	// background-colored padding space are both just spaces), so we measure
	// width instead.
	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "alpha.txt"},
		{FileName: "beta.txt"},
	}
	bare := lipgloss.Width(r.textStyle.String()+r.normalStyle.Render("alpha.txt")) +
		lipgloss.Width(r.textStyle.String()+r.normalStyle.Render("beta.txt"))

	got := lipgloss.Width(r.Render(atts, false, false, 200))
	require.Equal(t, bare+2, got,
		"each posted chip must carry a 1-col trailing margin so adjacent chip backgrounds don't touch")
}

func TestRender_MultipleChipsEachHaveRemoveButton(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "a.txt"},
		{FileName: "b.txt"},
		{FileName: "c.txt"},
	}
	out := r.Render(atts, false, true, 120)
	// Count occurrences of the remove glyph.
	count := 0
	for _, c := range out {
		if string(c) == styles.RemoveIcon {
			count++
		}
	}
	require.Equal(t, 3, count, "each chip should have a remove button")
}

func TestHitTestRemove_ClickOnFirstChipRemove(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}
	_ = r.Render(atts, false, true, 120)

	// The remove button of the first chip should be hit-testable.
	// Click at various X positions to verify we hit the right chip.
	idx := r.HitTestRemove(atts, 0)
	// At x=0 we're on the icon, not the remove button.
	require.Equal(t, -1, idx)
}

func TestHitTestRemove_ReturnsCorrectIndex(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}
	_ = r.Render(atts, false, true, 120)

	// Each chip bounds are stored after render. Verify there are two.
	require.Len(t, r.bounds, 2)

	// Click on the first chip's remove button.
	b0 := r.bounds[0]
	idx := r.HitTestRemove(atts, b0.startX)
	require.Equal(t, 0, idx)

	// Click on the second chip's remove button.
	b1 := r.bounds[1]
	idx = r.HitTestRemove(atts, b1.startX)
	require.Equal(t, 1, idx)
}

// removeGlyphCells returns the x cell position of each RemoveIcon glyph in
// the ANSI-stripped render output, so hit zones can be checked against what
// is actually on screen rather than against the recorded bounds.
func removeGlyphCells(t *testing.T, rendered string) []int {
	t.Helper()
	var cells []int
	x := 0
	for _, r := range ansi.Strip(rendered) {
		if string(r) == styles.RemoveIcon {
			cells = append(cells, x)
		}
		x += ansi.StringWidth(string(r))
	}
	return cells
}

func TestHitTestRemove_MatchesRenderedGlyphs(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.png"},
		{FileName: "third.md"},
	}
	out := r.Render(atts, false, true, 200)

	glyphs := removeGlyphCells(t, out)
	require.Len(t, glyphs, len(atts))

	marginW := r.removeStyle.GetMarginRight()
	require.Positive(t, marginW, "test assumes the remove style has a trailing margin")
	// Visible button width: padding + glyph, without the trailing margin.
	btnW := lipgloss.Width(r.removeStyle.String()) - marginW

	for i, glyphX := range glyphs {
		start := glyphX - r.removeStyle.GetPaddingLeft()
		for x := start; x < start+btnW; x++ {
			require.Equalf(t, i, r.HitTestRemove(atts, x), "cell %d inside chip %d's remove button", x, i)
		}
		// The margin cells after the button are just the gap between
		// chips; clicking there must not remove anything.
		for x := start + btnW; x < start+btnW+marginW; x++ {
			require.Equalf(t, -1, r.HitTestRemove(atts, x), "margin cell %d after chip %d's remove button", x, i)
		}
	}
}

func TestHitTestRemove_OverflowAreaHitsNothing(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	var atts []message.Attachment
	for i := range 10 {
		atts = append(atts, message.Attachment{FileName: fmt.Sprintf("file-%d.txt", i)})
	}
	out := r.Render(atts, false, true, 60)

	stripped := ansi.Strip(out)
	require.Contains(t, stripped, "more…", "expected the overflow label at this width")
	require.Less(t, len(r.bounds), len(atts), "not every chip should fit at this width")

	// Every cell from the end of the last remove button to past the end of
	// the row (the overflow label area) must hit nothing.
	last := r.bounds[len(r.bounds)-1]
	for x := last.removeEnd; x < ansi.StringWidth(stripped)+5; x++ {
		require.Equalf(t, -1, r.HitTestRemove(atts, x), "cell %d in the overflow label area", x)
	}
}

func TestHitTestRemove_OutsideAnyRemoveReturnsMinusOne(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	atts := []message.Attachment{
		{FileName: "test.txt"},
	}
	_ = r.Render(atts, false, true, 80)

	// Click far past the remove button.
	idx := r.HitTestRemove(atts, 999)
	require.Equal(t, -1, idx)
}

func TestHandleClick_RemovesAttachment(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)
	m.list = []message.Attachment{
		{FileName: "first.txt"},
		{FileName: "second.txt"},
	}

	// Render so bounds are populated.
	_ = m.Render(120)

	// Click the first chip's remove button.
	b0 := r.bounds[0]
	handled := m.HandleClick(b0.startX)
	require.True(t, handled)
	require.Len(t, m.list, 1)
	require.Equal(t, "second.txt", m.list[0].FileName)
}

func TestHandleClick_ClickOutsideRemoveDoesNothing(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)
	m.list = []message.Attachment{
		{FileName: "test.txt"},
	}

	_ = m.Render(80)

	// Click at x=0 (the icon area, not the remove button).
	handled := m.HandleClick(0)
	require.False(t, handled)
	require.Len(t, m.list, 1)
}

func TestHandleClick_DeletingModeIgnored(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)
	m.list = []message.Attachment{
		{FileName: "test.txt"},
	}
	m.deleting = true

	_ = m.Render(80)

	// bounds are empty in deleting mode since remove buttons aren't rendered.
	require.Empty(t, r.bounds)
	// Click anywhere — should be ignored.
	handled := m.HandleClick(10)
	require.False(t, handled)
}

func TestHandleClick_EmptyListIgnored(t *testing.T) {
	t.Parallel()

	r := newTestRenderer()
	km := Keymap{}
	m := New(r, km)

	handled := m.HandleClick(5)
	require.False(t, handled)
}
