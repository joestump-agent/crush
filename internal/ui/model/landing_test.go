package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestLandingViewColumnsSideBySide pins the landing layout: the status
// sections render as columns on one row, so every visible section title
// shares the first line of the column block, and the hidden opt-in
// sections leave no gap.
func TestLandingViewColumnsSideBySide(t *testing.T) {
	t.Parallel()

	m := newSidebarHeightTestUI(t)
	ws := m.com.Workspace.(*sidebarHeightTestWorkspace)
	ws.cfg.Agents = map[string]config.Agent{
		config.AgentWorker: {ID: config.AgentWorker, Name: "Worker", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeBuiltin},
	}
	m.layout.main = uv.Rect(0, 0, 160, 30)

	out := ansi.Strip(m.landingView())

	var titleLine string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "LSPs") {
			titleLine = line
			break
		}
	}
	for _, title := range []string{"LSPs", "MCPs", "Skills", "Agents", "Channels"} {
		require.Contains(t, titleLine, title,
			"section title %q must sit on the same row as the others", title)
	}
}
