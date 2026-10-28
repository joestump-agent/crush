package app

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/stretchr/testify/require"
)

// noThinking is what a Qwen small model carries so session titles come back
// as text rather than a chain of thought cut off at the 40-token limit.
var noThinking = map[string]any{
	"extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
}

func overrideTestApp(small config.SelectedModel) *App {
	cfg := &config.Config{
		Options: &config.Options{DisableDefaultProviders: true},
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: "litellm", Model: "Qwen3.8-27B"},
			config.SelectedModelTypeSmall: small,
		},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"litellm": {
				ID:      "litellm",
				Name:    "LiteLLM",
				BaseURL: "http://localhost:0/v1",
				Models: []catwalk.Model{
					{ID: "Qwen3.8-27B"},
					{ID: "Qwen3.8-27B-r9700"},
					{ID: "Qwen3.8-Flash-Next"},
				},
			},
			"other": {
				ID:      "other",
				Name:    "Other",
				BaseURL: "http://localhost:0/v1",
				Models:  []catwalk.Model{{ID: "other-large"}},
			},
		}),
	}
	return &App{config: config.NewTestStore(cfg)}
}

func TestModelOverrideKeepsConfiguredSmallOnTheSameProvider(t *testing.T) {
	// `crush run --model litellm/Qwen3.8-Flash-Next`, as every scheduled
	// sweep and one-shot runs: the small slot, and its provider_options,
	// must survive. Before, it became a bare Qwen3.8-Flash-Next and every
	// title request finished on length.
	small := config.SelectedModel{Provider: "litellm", Model: "Qwen3.8-27B", ProviderOptions: noThinking}
	app := overrideTestApp(small)

	require.NoError(t, app.applyModelOverrides("litellm/Qwen3.8-Flash-Next", ""))

	models := app.config.Config().Models
	require.Equal(t, "Qwen3.8-Flash-Next", models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, small, models[config.SelectedModelTypeSmall])
}

func TestModelOverrideOnAnotherProviderStillReplacesSmall(t *testing.T) {
	small := config.SelectedModel{Provider: "litellm", Model: "Qwen3.8-27B", ProviderOptions: noThinking}
	app := overrideTestApp(small)

	require.NoError(t, app.applyModelOverrides("other/other-large", ""))

	got := app.config.Config().Models[config.SelectedModelTypeSmall]
	require.Equal(t, "other", got.Provider, "a small model on another provider must not survive a provider switch")
}

func TestModelOverrideExplicitSmallStillWins(t *testing.T) {
	small := config.SelectedModel{Provider: "litellm", Model: "Qwen3.8-27B", ProviderOptions: noThinking}
	app := overrideTestApp(small)

	require.NoError(t, app.applyModelOverrides("litellm/Qwen3.8-Flash-Next", "litellm/Qwen3.8-27B-r9700"))

	require.Equal(t, "Qwen3.8-27B-r9700", app.config.Config().Models[config.SelectedModelTypeSmall].Model)
}
