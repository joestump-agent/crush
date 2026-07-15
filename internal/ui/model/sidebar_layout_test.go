package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSidebarLayoutFlushToEdge verifies that in the non-compact chat view the
// sidebar rectangle extends to the terminal's rightmost column and is the full
// sidebarWidth (32). This pins the scrollbar gutter flush to the edge so
// content is never clipped and the rightmost terminal column is not wasted.
//
// See: https://github.com/joestump-agent/crush/issues/112
func TestSidebarLayoutFlushToEdge(t *testing.T) {
	t.Parallel()

	m := newSidebarTestUI()
	// newSidebarTestUI defaults to uiChat + non-compact, which is the layout
	// branch we need to exercise.
	layout := m.generateLayout(m.width, 50)

	const sidebarWidth = 32

	require.Equal(t, sidebarWidth, layout.sidebar.Dx(),
		"sidebar should be the full sidebarWidth (%d), got %d",
		sidebarWidth, layout.sidebar.Dx())
	require.Equal(t, m.width, layout.sidebar.Max.X,
		"sidebar right edge should equal terminal width (%d) so the gutter is flush to the edge, got Max.X=%d",
		m.width, layout.sidebar.Max.X)
}
