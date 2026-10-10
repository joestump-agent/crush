package model

import (
	"cmp"
	"fmt"
	"image"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/logo"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
)

// modelInfo renders the current model information including reasoning
// settings and context usage/cost for the sidebar.
func (m *UI) modelInfo(width int) string {
	model := m.selectedLargeModel()
	reasoningInfo := ""
	providerName := ""

	if model != nil {
		// Get provider name first
		providerConfig, ok := m.com.Config().Providers.Get(model.ModelCfg.Provider)
		if ok {
			providerName = providerConfig.Name

			// Only check reasoning if model can reason
			if model.CatwalkCfg.CanReason {
				if len(model.CatwalkCfg.ReasoningLevels) == 0 {
					if model.ModelCfg.Think {
						reasoningInfo = "Thinking On"
					} else {
						reasoningInfo = "Thinking Off"
					}
				} else {
					reasoningEffort := cmp.Or(model.ModelCfg.ReasoningEffort, model.CatwalkCfg.DefaultReasoningEffort)
					reasoningInfo = fmt.Sprintf("Reasoning %s", common.FormatReasoningEffort(reasoningEffort))
				}
			}
		}
	}

	var modelContext *common.ModelContextInfo
	if model != nil && m.session != nil {
		modelContext = &common.ModelContextInfo{
			ContextUsed:    m.session.CompletionTokens + m.session.PromptTokens,
			Cost:           m.session.Cost,
			ModelContext:   model.CatwalkCfg.ContextWindow,
			EstimatedUsage: m.session.EstimatedUsage,
		}
	}
	var modelName string
	if model != nil {
		modelName = model.CatwalkCfg.Name
	}
	return common.ModelInfo(m.com.Styles, modelName, providerName, reasoningInfo, modelContext, width, m.hyperCredits)
}

// sidebarSection is one collapsible block of the sidebar body: how many
// items it has and how to render it with an item budget. An optional
// section (Agents, Channels) hides entirely when it has nothing to list;
// the always-on ones render a "None" placeholder instead.
type sidebarSection struct {
	count    int
	optional bool
	render   func(maxItems int) string
}

// getDynamicHeightLimits will give us the num of items to show in each
// section based on the height; some items are more important than others.
// It takes one count per visible section, in draw order, and returns the
// per-section item budget in the same order.
func getDynamicHeightLimits(availableHeight int, sectionCounts ...int) []int {
	const (
		minItemsPerSection = 2
		// Keep these high so dynamic layout uses available sidebar space
		// instead of hitting small hard limits.
		defaultMaxItemsShown    = 1000
		minAvailableHeightLimit = 10
	)

	sectionCount := len(sectionCounts)
	if sectionCount == 0 {
		return nil
	}

	maxes := make([]int, sectionCount)
	if availableHeight < minAvailableHeightLimit {
		for i := range maxes {
			maxes[i] = minItemsPerSection
		}
		return maxes
	}

	for i := range maxes {
		maxes[i] = minItemsPerSection
	}

	remainingHeight := max(0, availableHeight-(minItemsPerSection*sectionCount))

	sectionNeeds := make([]int, sectionCount)
	for i, count := range sectionCounts {
		sectionNeeds[i] = max(0, count-maxes[i])
	}

	for remainingHeight > 0 {
		allocated := false
		for i := range maxes {
			if remainingHeight == 0 {
				break
			}
			if sectionNeeds[i] == 0 || maxes[i] >= defaultMaxItemsShown {
				continue
			}
			maxes[i] = maxes[i] + 1
			sectionNeeds[i]--
			remainingHeight--
			allocated = true
		}
		if !allocated {
			break
		}
	}

	for remainingHeight > 0 {
		allocated := false
		for i := range maxes {
			if remainingHeight == 0 {
				break
			}
			if maxes[i] >= defaultMaxItemsShown {
				continue
			}
			maxes[i] = maxes[i] + 1
			remainingHeight--
			allocated = true
		}
		if !allocated {
			break
		}
	}

	return maxes
}

// scrollSidebarOnWheel scrolls the sidebar when a wheel event lands over it,
// returning true if it handled the event. DeltaY>0 is a scroll-down (matching
// list.ScrollBy and the chat wheel handler), and a higher sidebarScroll shows
// lower content, so the delta is added — keeping the wheel consistent with the
// chat panel and the Down key. The upper bound is clamped at draw time.
func (m *UI) scrollSidebarOnWheel(msg common.CoalescedWheelMsg) bool {
	if msg.Mouse.X < m.layout.sidebar.Min.X || msg.Mouse.X >= m.layout.sidebar.Max.X {
		return false
	}
	if lines := int(msg.DeltaY); lines != 0 {
		m.sidebarScroll = max(0, m.sidebarScroll+lines)
	}
	return true
}

// sidebarScrollbarWidth is the fixed 1-column gutter the sidebar reserves for
// its scroll indicator (flush to the terminal's rightmost column).
const sidebarScrollbarWidth = 1

// sidebarRightPadWidth is a fixed 1-column blank spacer between the sidebar
// content and the scrollbar gutter, so the scrollbar never sits flush against
// the content.
const sidebarRightPadWidth = 1

// sidebarContentWidth returns the width available for sidebar content after
// reserving the right pad and the scrollbar gutter. Both are reserved
// unconditionally (not only when content overflows) so the content — including
// the fixed-width logo, which is cached at this same width — is always rendered
// at its final width and never clipped when the scrollbar is drawn. Keeping it
// focus- and overflow-independent also stops the content from shifting on focus
// changes.
func sidebarContentWidth(sidebarWidth int) int {
	return max(sidebarWidth-sidebarScrollbarWidth-sidebarRightPadWidth, 0)
}

// blankSidebarColumn renders an empty column height rows tall, used for the
// right pad and for the scrollbar gutter when there is no scrollbar to draw.
func blankSidebarColumn(height int) string {
	if height <= 0 {
		return ""
	}
	var sb strings.Builder
	for i := 0; i < height; i++ {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(" ")
	}
	return sb.String()
}

// sidebar renders the chat sidebar containing session title, working
// directory, model info, file list, LSP status, and MCP status.
func (m *UI) drawSidebar(scr uv.Screen, area uv.Rectangle) {
	if m.session == nil {
		return
	}

	const logoHeightBreakpoint = 30

	t := m.com.Styles
	width := area.Dx()
	height := area.Dy()

	// All content renders into the width left after reserving the scrollbar
	// gutter, so the fixed-width logo (cached at this same width) and every
	// section fit exactly and are never clipped when the gutter is drawn.
	contentWidth := sidebarContentWidth(width)

	focused := m.focus == uiFocusSidebar

	title := t.Sidebar.SessionTitle.Width(contentWidth).MaxHeight(2).Render(m.session.Title)
	cwd := common.PrettyPathWithBranch(t, m.com.Workspace.WorkingDir(), m.gitBranch, contentWidth)
	sidebarLogo := m.sidebarLogo
	if height < logoHeightBreakpoint {
		sidebarLogo = logo.SmallRender(m.com.Styles, contentWidth, logo.Opts{
			Hyper: m.com.IsHyper(),
		})
	}
	blocks := []string{
		sidebarLogo,
		title,
		"",
		cwd,
		"",
		m.modelInfo(contentWidth),
		"",
	}

	sidebarHeader := lipgloss.JoinVertical(
		lipgloss.Left,
		blocks...,
	)

	var remainingHeightArea image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(sidebarHeader)),
		layout.Fill(1),
	).Split(m.layout.sidebar).Assign(new(image.Rectangle), &remainingHeightArea)
	filesCount := 0
	for _, f := range m.sessionFiles {
		if f.Additions == 0 && f.Deletions == 0 {
			continue
		}
		filesCount++
	}

	lspsCount := len(m.lspStates)

	mcpsCount := 0
	for _, mcpCfg := range m.com.Config().MCP.Sorted() {
		if _, ok := m.mcpStates[mcpCfg.Name]; ok {
			mcpsCount++
		}
	}

	skillsCount := len(m.skillStatusItems())
	agentsCount := len(m.agentStatusItems())
	channelsCount := len(m.channelStatusItems())

	// The always-on sections render even when empty (a "None"
	// placeholder); the opt-in ones (Agents, the dispatch roster per
	// #434, and Channels) hide entirely when they have nothing to list.
	sections := []sidebarSection{
		{count: filesCount, render: func(maxItems int) string {
			return m.filesInfo(m.com.Workspace.WorkingDir(), contentWidth, maxItems, true)
		}},
		{count: lspsCount, render: func(maxItems int) string { return m.lspInfo(contentWidth, maxItems, true) }},
		{count: mcpsCount, render: func(maxItems int) string { return m.mcpInfo(contentWidth, maxItems, true) }},
		{count: skillsCount, render: func(maxItems int) string { return m.skillsInfo(contentWidth, maxItems, true) }},
		{count: agentsCount, optional: true, render: func(maxItems int) string { return m.agentsInfo(contentWidth, maxItems, true) }},
		{count: channelsCount, optional: true, render: func(maxItems int) string { return m.channelsInfo(contentWidth, maxItems, true) }},
	}
	visible := make([]sidebarSection, 0, len(sections))
	for _, section := range sections {
		if section.optional && section.count == 0 {
			continue
		}
		visible = append(visible, section)
	}

	// Each section below the header renders a title line plus a blank line
	// before its items, and adjacent sections are joined with one blank
	// separator line (see fullContent below). That overhead must come out
	// of the height before budgeting item lines, or the bottom section is
	// silently clipped by the MaxHeight applied at the end.
	sectionOverhead := len(visible)*2 + len(visible) - 1
	remainingHeight := remainingHeightArea.Dy() - sectionOverhead

	sectionCounts := make([]int, len(visible))
	for i, section := range visible {
		sectionCounts[i] = section.count
	}
	maxes := getDynamicHeightLimits(remainingHeight, sectionCounts...)

	// When focused, show all items so scroll can reveal truncated content.
	if focused {
		for i, section := range visible {
			maxes[i] = max(maxes[i], section.count)
		}
	}

	contentBlocks := make([]string, 0, len(visible)*2+1)
	contentBlocks = append(contentBlocks, sidebarHeader)
	for i, section := range visible {
		if i > 0 {
			contentBlocks = append(contentBlocks, "")
		}
		contentBlocks = append(contentBlocks, section.render(maxes[i]))
	}
	fullContent := lipgloss.JoinVertical(lipgloss.Left, contentBlocks...)

	// Apply scroll offset. Clamp against real content height.
	contentLines := strings.Split(fullContent, "\n")
	contentHeight := len(contentLines)
	maxScroll := max(0, contentHeight-height)
	m.sidebarScroll = min(m.sidebarScroll, maxScroll)
	scroll := min(m.sidebarScroll, maxScroll)
	if scroll > 0 && scroll < len(contentLines) {
		contentLines = contentLines[scroll:]
	}
	scrolledContent := strings.Join(contentLines, "\n")

	contentStyle := lipgloss.NewStyle().
		MaxWidth(contentWidth).
		MaxHeight(height)
	rendered := contentStyle.Render(scrolledContent)

	// The right pad and gutter columns are always reserved (see
	// sidebarContentWidth). Draw a real scrollbar in the gutter when the sidebar
	// is focused and its content overflows; otherwise fill it with a blank
	// spacer so nothing shifts and the scrollbar never overlaps content.
	// Scrollbar returns "" when the content fits, so an unfocused or
	// non-overflowing sidebar gets the blank spacer. The pad is always blank,
	// keeping the scrollbar off the content.
	var gutter string
	if focused {
		gutter = common.Scrollbar(t, height, contentHeight, height, scroll)
	}
	if gutter == "" {
		gutter = blankSidebarColumn(height)
	}
	pad := blankSidebarColumn(height)
	rendered = lipgloss.JoinHorizontal(lipgloss.Top, rendered, pad, gutter)

	uv.NewStyledString(rendered).Draw(scr, area)
}
