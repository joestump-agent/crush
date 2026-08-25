package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeResolver struct{}

func (fakeResolver) ResolveValue(v string) (string, error) { return "resolved:" + v, nil }

func TestResolvedEmbeddingsDefaults(t *testing.T) {
	t.Parallel()

	c := &Config{Embeddings: &EmbeddingsConfig{APIKey: "sk-test"}}
	e, ok := c.ResolvedEmbeddings()
	require.True(t, ok)
	require.Equal(t, "https://api.openai.com/v1", e.BaseURL)
	require.Equal(t, "text-embedding-3-small", e.Model)
	require.Equal(t, 768, e.Dimension)
	require.Equal(t, "sk-test", e.APIKey)
}

func TestResolvedEmbeddingsUnset(t *testing.T) {
	t.Parallel()

	c := &Config{}
	_, ok := c.ResolvedEmbeddings()
	require.False(t, ok)
}

func TestResolveEmbeddingsExpandsCredentials(t *testing.T) {
	t.Parallel()

	c := &Config{Embeddings: &EmbeddingsConfig{
		BaseURL: "$(printenv EMB_BASE)",
		APIKey:  "$EMB_KEY",
	}}
	c.resolveEmbeddings(fakeResolver{})
	require.Equal(t, "resolved:$(printenv EMB_BASE)", c.Embeddings.BaseURL)
	require.Equal(t, "resolved:$EMB_KEY", c.Embeddings.APIKey)
}

func TestResolveEmbeddingsNilNoop(t *testing.T) {
	t.Parallel()

	c := &Config{}
	c.resolveEmbeddings(fakeResolver{})
	require.Nil(t, c.Embeddings)
}
