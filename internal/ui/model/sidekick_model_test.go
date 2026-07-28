package model

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/ui/dialog"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// sidekickModelTestConfig builds a config with a configured "zai"
// provider carrying glm-5.2, plus main-agent large/small selections and
// the sidekick agent entry, mirroring a real workspace setup (#54).
func sidekickModelTestConfig() *config.Config {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("zai", config.ProviderConfig{
		ID:     "zai",
		Name:   "Z.ai",
		Type:   "openai-compat",
		Models: []catwalk.Model{{ID: "glm-5.2", Name: "GLM-5.2"}},
	})
	return &config.Config{
		Options:   &config.Options{},
		Providers: providers,
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: "main", Model: "big-model"},
			config.SelectedModelTypeSmall: {Provider: "main", Model: "small-model"},
		},
		Agents: map[string]config.Agent{
			config.AgentSidekick: {
				ID:    config.AgentSidekick,
				Model: config.SelectedModelTypeSmall,
			},
		},
	}
}

// TestSidekickModelSelectIsSessionScoped pins the #54 isolation
// contract: applying a Sidekick model selection routes through the
// workspace's Sidekick-only setter and never mutates the main agent's
// model selections in the config.
func TestSidekickModelSelectIsSessionScoped(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()
	ws.cfg = sidekickModelTestConfig()

	cmd := m.handleSelectSidekickModel(dialog.ActionSelectModel{
		ForSidekick: true,
		Model:       config.SelectedModel{Provider: "zai", Model: "glm-5.2"},
	})
	require.NotNil(t, cmd)

	require.Equal(t, "zai", ws.model.Provider, "selection must reach the Sidekick setter")
	require.Equal(t, "glm-5.2", ws.model.Model)

	cfg := m.com.Config()
	require.Equal(t, "big-model", cfg.Models[config.SelectedModelTypeLarge].Model,
		"the main agent's large model must never change")
	require.Equal(t, "small-model", cfg.Models[config.SelectedModelTypeSmall].Model,
		"the main agent's small model must never change")
}

// TestSidekickModelSelectRejectsUnconfiguredProvider verifies a
// selection for a provider missing from the config warns instead of
// reaching the setter (the Sidekick picker cannot drive auth flows).
func TestSidekickModelSelectRejectsUnconfiguredProvider(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()
	ws.cfg = sidekickModelTestConfig()

	cmd := m.handleSelectSidekickModel(dialog.ActionSelectModel{
		ForSidekick: true,
		Model:       config.SelectedModel{Provider: "nope", Model: "x"},
	})
	require.NotNil(t, cmd)
	require.Empty(t, ws.model.Provider, "an unconfigured provider must never reach the setter")
}

// TestSidekickFooterShowsActiveModel verifies the panel footer renders
// the workspace's live Sidekick model — so a session-scoped switch shows
// up immediately (#53/#54) — falling back to the config-derived model
// when the workspace reports none.
func TestSidekickFooterShowsActiveModel(t *testing.T) {
	t.Parallel()
	m, ws := newSidekickPanelTestUI()
	ws.cfg = sidekickModelTestConfig()

	// No live selection reported: config fallback (the small model).
	out := ansi.Strip(m.renderSidekickFooter(60))
	require.Contains(t, out, "small-model")

	// The workspace reports the active Sidekick selection: the footer
	// follows it, preferring the catwalk display name.
	ws.model = config.SelectedModel{Provider: "zai", Model: "glm-5.2"}
	out = ansi.Strip(m.renderSidekickFooter(60))
	require.Contains(t, out, "GLM-5.2")
	require.NotContains(t, out, "small-model")
}
