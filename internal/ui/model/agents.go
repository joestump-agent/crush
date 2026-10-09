package model

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
)

// agentStatusItem holds the display data for a single dispatch agent entry.
type agentStatusItem struct {
	id          string
	icon        string
	title       string
	description string
}

// agentsInfo renders the dispatch agents section: the roster the dispatch
// tool can target, from the builtin worker to runtime a2a agents behind
// their external Agent Cards (#434). An empty roster hides the section
// entirely, unlike the always-on sections: dispatch agents are opt-in
// surface.
func (m *UI) agentsInfo(width, maxItems int, isSection bool) string {
	t := m.com.Styles

	title := t.Resource.Heading.Render("Agents")
	if isSection {
		title = common.Section(t, title, width)
	}

	items := m.agentStatusItems()
	if len(items) == 0 {
		return ""
	}

	list := agentList(t, items, width, maxItems)
	return lipgloss.NewStyle().Width(width).Render(fmt.Sprintf("%s\n\n%s", title, list))
}

// agentStatusItems collects the dispatch agents from the resolved config,
// sorted by id. Main and subagent roles stay out: they are not dispatch
// targets.
func (m *UI) agentStatusItems() []agentStatusItem {
	t := m.com.Styles
	cfg := m.com.Config()
	if cfg == nil {
		return nil
	}

	var items []agentStatusItem
	for id, agent := range cfg.Agents {
		if agent.Role != config.AgentRoleDispatch || agent.Disabled {
			continue
		}
		icon := t.Resource.OnlineIcon.String()
		description := agent.Runtime
		if agent.Runtime == config.AgentRuntimeA2A && agent.Unusable != "" {
			icon = t.Resource.ErrorIcon.String()
			description = fmt.Sprintf("error: %s", agent.Unusable)
		}
		items = append(items, agentStatusItem{
			id:          id,
			icon:        icon,
			title:       t.Resource.Name.Render(agent.Name),
			description: t.Resource.StatusText.Render(description),
		})
	}

	slices.SortStableFunc(items, func(a, b agentStatusItem) int {
		return strings.Compare(a.id, b.id)
	})

	return items
}

func agentList(t *styles.Styles, items []agentStatusItem, width, maxItems int) string {
	if maxItems <= 0 {
		return ""
	}

	if len(items) > maxItems {
		visibleItems := items[:maxItems-1]
		remaining := len(items) - (maxItems - 1)
		items = append(visibleItems, agentStatusItem{
			title: t.Resource.AdditionalText.Render(fmt.Sprintf("…and %d more", remaining)),
		})
	}

	renderedItems := make([]string, 0, len(items))
	for _, item := range items {
		renderedItems = append(renderedItems, common.Status(t, common.StatusOpts{
			Icon:        item.icon,
			Title:       item.title,
			Description: item.description,
		}, width))
	}
	return lipgloss.JoinVertical(lipgloss.Left, renderedItems...)
}
