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

// getDynamicHeightLimits will give us the num of items to show in each section based on the height
// some items are more important than others.
func getDynamicHeightLimits(availableHeight, fileCount, lspCount, mcpCount, skillCount, channelCount int) (maxFiles, maxLSPs, maxMCPs, maxSkills, maxChannels int) {
	const (
		minItemsPerSection = 2
		// Keep these high so dynamic layout uses available sidebar space
		// instead of hitting small hard limits.
		defaultMaxFilesShown    = 1000
		defaultMaxLSPsShown     = 1000
		defaultMaxMCPsShown     = 1000
		defaultMaxSkillsShown   = 1000
		defaultMaxChannelsShown = 1000
		minAvailableHeightLimit = 10
	)

	if availableHeight < minAvailableHeightLimit {
		return minItemsPerSection, minItemsPerSection, minItemsPerSection, minItemsPerSection, minItemsPerSection
	}

	maxFiles = minItemsPerSection
	maxLSPs = minItemsPerSection
	maxMCPs = minItemsPerSection
	maxSkills = minItemsPerSection
	maxChannels = minItemsPerSection

	remainingHeight := max(0, availableHeight-(minItemsPerSection*5))

	sectionValues := []*int{&maxFiles, &maxLSPs, &maxMCPs, &maxSkills, &maxChannels}
	sectionCaps := []int{defaultMaxFilesShown, defaultMaxLSPsShown, defaultMaxMCPsShown, defaultMaxSkillsShown, defaultMaxChannelsShown}
	sectionNeeds := []int{max(0, fileCount-maxFiles), max(0, lspCount-maxLSPs), max(0, mcpCount-maxMCPs), max(0, skillCount-maxSkills), max(0, channelCount-maxChannels)}

	for remainingHeight > 0 {
		allocated := false
		for i, section := range sectionValues {
			if remainingHeight == 0 {
				break
			}
			if sectionNeeds[i] == 0 || *section >= sectionCaps[i] {
				continue
			}
			*section = *section + 1
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
		for i, section := range sectionValues {
			if remainingHeight == 0 {
				break
			}
			if *section >= sectionCaps[i] {
				continue
			}
			*section = *section + 1
			remainingHeight--
			allocated = true
		}
		if !allocated {
			break
		}
	}

	return maxFiles, maxLSPs, maxMCPs, maxSkills, maxChannels
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
	cwd := common.PrettyPath(t, m.com.Workspace.WorkingDir(), contentWidth)
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
	channelsCount := len(m.channelStatusItems())

	// Each section below the header renders a title line plus a blank line
	// before its items, and adjacent sections are joined with one blank
	// separator line (see fullContent below). That overhead must come out
	// of the height before budgeting item lines, or the bottom section is
	// silently clipped by the MaxHeight applied at the end.
	sectionCounts := []int{filesCount, lspsCount, mcpsCount, skillsCount, channelsCount}
	sectionOverhead := len(sectionCounts)*2 + len(sectionCounts) - 1
	remainingHeight := remainingHeightArea.Dy() - sectionOverhead

	maxFiles, maxLSPs, maxMCPs, maxSkills, maxChannels := getDynamicHeightLimits(remainingHeight, filesCount, lspsCount, mcpsCount, skillsCount, channelsCount)

	// When focused, show all items so scroll can reveal truncated content.
	if focused {
		maxFiles = max(maxFiles, filesCount)
		maxLSPs = max(maxLSPs, lspsCount)
		maxMCPs = max(maxMCPs, mcpsCount)
		maxSkills = max(maxSkills, skillsCount)
		maxChannels = max(maxChannels, channelsCount)
	}

	lspSection := m.lspInfo(contentWidth, maxLSPs, true)
	mcpSection := m.mcpInfo(contentWidth, maxMCPs, true)
	skillsSection := m.skillsInfo(contentWidth, maxSkills, true)
	channelsSection := m.channelsInfo(contentWidth, maxChannels, true)
	filesSection := m.filesInfo(m.com.Workspace.WorkingDir(), contentWidth, maxFiles, true)

	fullContent := lipgloss.JoinVertical(
		lipgloss.Left,
		sidebarHeader,
		filesSection,
		"",
		lspSection,
		"",
		mcpSection,
		"",
		skillsSection,
		"",
		channelsSection,
	)

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
