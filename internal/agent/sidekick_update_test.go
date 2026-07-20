package agent

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
	a2ui "github.com/tmc/a2ui"
)

func toolNames(list []fantasy.AgentTool) []string {
	names := make([]string, 0, len(list))
	for _, tool := range list {
		names = append(names, tool.Info().Name)
	}
	return names
}

// TestSidekickUpdateToolWiring pins the sidekick_update routing rules
// (#57): the tool reaches the main coder agent only — never sub-agents,
// never the Sidekick agent itself — and disappears entirely when A2UI is
// disabled.
func TestSidekickUpdateToolWiring(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	agentCfg := config.Agent{
		ID:           config.AgentCoder,
		AllowedTools: []string{tools.SidekickUpdateToolName},
	}

	// Main coder agent gets the tool.
	built, err := coord.buildTools(t.Context(), agentCfg, false, "")
	require.NoError(t, err)
	require.Contains(t, toolNames(built), tools.SidekickUpdateToolName)

	// Sub-agents never do, even with the tool allowed.
	built, err = coord.buildTools(t.Context(), agentCfg, true, "")
	require.NoError(t, err)
	require.NotContains(t, toolNames(built), tools.SidekickUpdateToolName)

	// The Sidekick agent's own tool list never includes it.
	skCfg, ok := coord.cfg.Config().Agents[config.AgentSidekick]
	require.True(t, ok)
	require.NotContains(t, toolNames(coord.buildSidekickTools(skCfg)), tools.SidekickUpdateToolName)
	require.NotContains(t, skCfg.AllowedTools, tools.SidekickUpdateToolName,
		"the sidekick agent config must never allow sidekick_update")

	// Disabling A2UI removes the push channel: the payload is A2UI.
	coord.cfg.Config().Options.DisableA2UI = true
	built, err = coord.buildTools(t.Context(), agentCfg, false, "")
	require.NoError(t, err)
	require.NotContains(t, toolNames(built), tools.SidekickUpdateToolName)
}

// TestSidekickUpdateInDefaultCoderTools verifies SetupAgents allows the
// tool on the coder agent and keeps it off the read-only task agent.
func TestSidekickUpdateInDefaultCoderTools(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	// newSidekickTestCoordinator blanks the coder's AllowedTools to keep
	// construction light; re-derive the defaults to inspect them.
	cfg := coord.cfg.Config()
	cfg.SetupAgents()
	require.Contains(t, cfg.Agents[config.AgentCoder].AllowedTools, tools.SidekickUpdateToolName)
	require.NotContains(t, cfg.Agents[config.AgentTask].AllowedTools, tools.SidekickUpdateToolName)
	require.NotContains(t, cfg.Agents[config.AgentSidekick].AllowedTools, tools.SidekickUpdateToolName)
}

// TestSidekickDashboardSubscribeDeliversToolPushes wires the tool to the
// coordinator broker and asserts a push reaches a dashboard subscriber —
// the deterministic #57 → #56 route.
func TestSidekickDashboardSubscribeDeliversToolPushes(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	sub := coord.SidekickDashboardSubscribe(t.Context())
	require.NotNil(t, sub)

	agentCfg := config.Agent{ID: config.AgentCoder, AllowedTools: []string{tools.SidekickUpdateToolName}}
	built, err := coord.buildTools(t.Context(), agentCfg, false, "")
	require.NoError(t, err)
	require.Len(t, built, 1)

	const payload = `{"version":"v0.9","updateComponents":{"surfaceId":"s1","components":[{"component":"Text","id":"t","text":"Step 1/3"}]}}`
	input, err := json.Marshal(map[string]string{"surface": payload})
	require.NoError(t, err)
	resp, err := built[0].Run(t.Context(), fantasy.ToolCall{
		ID:    "call1",
		Name:  tools.SidekickUpdateToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "rendered", resp.Content)

	ev := <-sub
	require.Contains(t, ev.Payload.Content, "Step 1/3")
	require.Contains(t, ev.Payload.Content, "<a2ui-json>")
}

// TestCoderPromptSidekickUpdateGate pins the system-prompt guidance
// (#57): present only when both A2UI and the Sidekick dashboard are on.
func TestCoderPromptSidekickUpdateGate(t *testing.T) {
	t.Parallel()

	off := renderCoderTemplate(t, prompt.PromptDat{A2UI: true, A2UIVersion: a2ui.Version})
	require.NotContains(t, off, "sidekick_update")

	on := renderCoderTemplate(t, prompt.PromptDat{A2UI: true, A2UIVersion: a2ui.Version, SidekickUpdate: true})
	require.Contains(t, on, "sidekick_update")
	require.Contains(t, on, "replaces the previous dashboard")

	// The guidance lives inside the A2UI section: without A2UI there is
	// no surface format to speak of, so it must not render.
	bare := renderCoderTemplate(t, prompt.PromptDat{SidekickUpdate: true})
	require.NotContains(t, bare, "sidekick_update")
}
