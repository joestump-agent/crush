package config

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/env"
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

// failingResolver stands in for the real shell resolver on the error path.
// Its error embeds the template it was handed, exactly as resolveError does.
type failingResolver struct{}

func (failingResolver) ResolveValue(v string) (string, error) {
	return "", errors.New("resolving " + v + ": parse error")
}

// TestResolveEmbeddingsNeverLogsAPIKey pins the reason api_key is logged
// without its error: resolveError renders the pre-expansion template, which
// is the credential itself when the user wrote a literal key.
func TestResolveEmbeddingsNeverLogsAPIKey(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := &Config{Embeddings: &EmbeddingsConfig{APIKey: "sk-live-DEADBEEF"}}
	c.resolveEmbeddings(failingResolver{})

	require.NotContains(t, buf.String(), "sk-live-DEADBEEF")
	require.Contains(t, buf.String(), "embeddings API key")
}

// TestResolveEmbeddingsClearsUnresolvableFields pins that a failed
// resolution does not leave the raw template behind, which would otherwise
// be sent verbatim as the bearer token and base URL.
func TestResolveEmbeddingsClearsUnresolvableFields(t *testing.T) {
	t.Parallel()

	c := &Config{Embeddings: &EmbeddingsConfig{
		APIKey:  "$EMB_KEY",
		BaseURL: "$(broken",
	}}
	c.resolveEmbeddings(failingResolver{})

	require.Empty(t, c.Embeddings.APIKey)
	require.Empty(t, c.Embeddings.BaseURL)

	// The cleared base URL falls back to the documented default.
	e, ok := c.ResolvedEmbeddings()
	require.True(t, ok)
	require.Equal(t, "https://api.openai.com/v1", e.BaseURL)
}

// TestResolveEmbeddingsRealResolverHidesLiteralKey is the end-to-end version
// of the above against the real shell resolver: a literal key containing
// shell metacharacters fails to parse, and must not reach the log.
func TestResolveEmbeddingsRealResolverHidesLiteralKey(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := &Config{Embeddings: &EmbeddingsConfig{APIKey: `sk-live-DEADBEEF$(`}}
	c.resolveEmbeddings(NewShellVariableResolver(env.New()))

	require.False(t, strings.Contains(buf.String(), "DEADBEEF"), "api key leaked into logs: %s", buf.String())
	require.Empty(t, c.Embeddings.APIKey)
}
