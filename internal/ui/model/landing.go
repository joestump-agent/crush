package model

import (
	"image"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/charmbracelet/ultraviolet/layout"
)

// selectedLargeModel returns the currently selected large language model as
// memoized by the off-thread busy/agent probe (see workspace_cache.go), or
// nil when the agent isn't ready. It must never probe the workspace: it is
// called on every frame and AgentIsReady/AgentModel are synchronous HTTP
// round-trips in client/server mode.
func (m *UI) selectedLargeModel() *workspace.AgentModel {
	if m.agentReady {
		model := m.agentModel
		return &model
	}
	return nil
}

// landingView renders the landing page view showing the current working
// directory, model information, and LSP/MCP status in a two-column layout.
func (m *UI) landingView() string {
	t := m.com.Styles
	width := m.layout.main.Dx()
	cwd := common.PrettyPathInline(t, m.com.Workspace.WorkingDir(), m.gitBranch, width)

	parts := []string{
		cwd,
	}

	parts = append(parts, "", m.modelInfo(width))
	infoSection := lipgloss.JoinVertical(lipgloss.Left, parts...)

	var remainingHeightArea image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(infoSection)+1),
		layout.Fill(1),
	).Split(m.layout.main).Assign(new(image.Rectangle), &remainingHeightArea)

	// The always-on columns are joined by the opt-in ones: Agents (the
	// dispatch roster, #434) and Channels render only when they have
	// entries, so the column count, and each column's width, adapts.
	columnCount := 3
	if len(m.agentStatusItems()) > 0 {
		columnCount++
	}
	if len(m.channelStatusItems()) > 0 {
		columnCount++
	}
	columnWidth := min(30, max(1, (width-(columnCount-1))/columnCount))
	columnHeight := max(1, remainingHeightArea.Dy())

	columns := []string{
		m.lspInfo(columnWidth, columnHeight, false),
		m.mcpInfo(columnWidth, columnHeight, false),
		m.skillsInfo(columnWidth, columnHeight, false),
		m.agentsInfo(columnWidth, columnHeight, false),
		m.channelsInfo(columnWidth, columnHeight, false),
	}
	// Join side by side with a one-space gutter. strings.Join would glue
	// multi-line blocks end to end and stack the columns vertically.
	visible := make([]string, 0, len(columns)*2)
	for _, column := range columns {
		if column == "" {
			continue
		}
		if len(visible) > 0 {
			visible = append(visible, " ")
		}
		visible = append(visible, column)
	}
	content := lipgloss.JoinHorizontal(lipgloss.Left, visible...)

	return lipgloss.NewStyle().
		Width(width).
		Height(m.layout.main.Dy() - 1).
		PaddingTop(1).
		Render(
			lipgloss.JoinVertical(lipgloss.Left, infoSection, "", content),
		)
}
