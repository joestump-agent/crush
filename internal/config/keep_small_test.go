package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeepConfiguredSmall(t *testing.T) {
	small := SelectedModel{Provider: "litellm", Model: "Qwen3.8-27B"}
	require.True(t, KeepConfiguredSmall(small, "litellm"))
	require.False(t, KeepConfiguredSmall(small, "openai"), "another provider")
	require.False(t, KeepConfiguredSmall(SelectedModel{}, "litellm"), "nothing configured")
	require.False(t, KeepConfiguredSmall(small, ""), "no large override")
}
