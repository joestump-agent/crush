package model

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
)

// newAgentsTestUI builds a UI whose Config exposes the given resolved
// agents map.
func newAgentsTestUI(t *testing.T, agents map[string]config.Agent) *UI {
	t.Helper()
	com := &common.Common{
		Workspace: &channelsTestWorkspace{cfg: &config.Config{Agents: agents}},
		Styles:    common.DefaultCommon(nil).Styles,
	}
	return &UI{com: com}
}

// TestAgentStatusItems_FiltersSortsAndMapsRuntime covers the core logic:
// only enabled dispatch agents are listed (main and subagent roles are not
// dispatch targets), they are sorted by id, and the description carries
// the two labels a bare runtime token collapsed — origin (built-in vs
// user-defined) and transport (in-process vs external a2a) — while an
// unusable a2a agent surfaces its reason.
func TestAgentStatusItems_FiltersSortsAndMapsRuntime(t *testing.T) {
	t.Parallel()

	m := newAgentsTestUI(t, map[string]config.Agent{
		"worker":   {ID: "worker", Name: "Worker", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeBuiltin},
		"go-coder": {ID: "go-coder", Name: "Go Coder", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeBuiltin},
		"reviewer": {ID: "reviewer", Name: "Reviewer", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeA2A},
		"broken":   {ID: "broken", Name: "Broken", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeA2A, Unusable: "agents.broken.card: must use https"},
		"coder":    {ID: "coder", Name: "Coder", Role: config.AgentRoleMain, Runtime: config.AgentRuntimeBuiltin},
		"task":     {ID: "task", Name: "Task", Role: config.AgentRoleSubagent, Runtime: config.AgentRuntimeBuiltin},
		"ghost":    {ID: "ghost", Name: "Ghost", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeBuiltin, Disabled: true},
	})

	items := m.agentStatusItems()

	require.Len(t, items, 4, "only enabled dispatch agents are listed")
	require.Equal(t, "broken", items[0].id)
	require.Equal(t, "go-coder", items[1].id)
	require.Equal(t, "reviewer", items[2].id)
	require.Equal(t, "worker", items[3].id)

	require.Contains(t, ansi.Strip(items[0].description), "error: agents.broken.card: must use https")
	require.Equal(t, "user · in-process", ansi.Strip(items[1].description), "a user-defined builtin-runtime agent is custom but in-process")
	require.Equal(t, "user · external a2a", ansi.Strip(items[2].description), "an external card agent runs over the wire")
	require.Equal(t, "built-in · in-process", ansi.Strip(items[3].description), "worker is the one dispatch agent Crush ships")
}

// TestAgentsInfo_EmptyHidden verifies the empty state hides the section:
// with no dispatch agents there is no "Agents" title and no placeholder.
func TestAgentsInfo_EmptyHidden(t *testing.T) {
	t.Parallel()

	m := newAgentsTestUI(t, map[string]config.Agent{
		"coder": {ID: "coder", Name: "Coder", Role: config.AgentRoleMain, Runtime: config.AgentRuntimeBuiltin},
	})

	out := ansi.Strip(m.agentsInfo(40, 10, false))
	require.Empty(t, out)
}

// TestAgentsInfo_RendersSection verifies the populated state renders the
// section title and the agent names.
func TestAgentsInfo_RendersSection(t *testing.T) {
	t.Parallel()

	m := newAgentsTestUI(t, map[string]config.Agent{
		"worker":   {ID: "worker", Name: "Worker", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeBuiltin},
		"reviewer": {ID: "reviewer", Name: "Reviewer", Role: config.AgentRoleDispatch, Runtime: config.AgentRuntimeA2A},
	})

	out := ansi.Strip(m.agentsInfo(40, 10, false))
	require.Contains(t, out, "Agents")
	require.Contains(t, out, "Worker")
	require.Contains(t, out, "Reviewer")
}

// TestAgentList_Truncation covers the "…and N more" overflow behavior and
// the maxItems<=0 guard.
func TestAgentList_Truncation(t *testing.T) {
	t.Parallel()

	styles := common.DefaultCommon(nil).Styles
	items := []agentStatusItem{
		{id: "a", title: "a"},
		{id: "b", title: "b"},
		{id: "c", title: "c"},
	}

	out := ansi.Strip(agentList(styles, items, 80, 2))
	require.Contains(t, out, "and 2 more")

	require.Empty(t, agentList(styles, items, 80, 0))
}
