package shellconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbeddingsSet(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `embeddings set --base-url https://emb.example.com/v1 --api-key sk-test --model nomic-embed --dimension 512`)

	emb, ok := result["embeddings"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "https://emb.example.com/v1", emb["base_url"])
	require.Equal(t, "sk-test", emb["api_key"])
	require.Equal(t, "nomic-embed", emb["model"])
	require.Equal(t, float64(512), emb["dimension"])
}

func TestEmbeddingsSetPartialKeepsExisting(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `embeddings set --model m1 --api-key k1
embeddings set --dimension 256`)

	emb := result["embeddings"].(map[string]any)
	require.Equal(t, "m1", emb["model"])
	require.Equal(t, "k1", emb["api_key"])
	require.Equal(t, float64(256), emb["dimension"])
}

func TestEmbeddingsSetInvalidDimension(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/crushrc"
	_, err := LoadShellConfig(t.Context(), path, []byte(`embeddings set --dimension 0`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "dimension must be positive")
}

func TestEmbeddingsClear(t *testing.T) {
	t.Parallel()

	// Clearing the only section leaves the builder empty, which the
	// loader treats as "no config" and returns no JSON.
	path := t.TempDir() + "/crushrc"
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`embeddings set --model m1
embeddings clear`))
	require.NoError(t, err)
	require.Empty(t, string(jsonBytes))

	result := loadScript(t, `provider add openai --api-key k
embeddings set --model m1
embeddings clear`)
	require.NotContains(t, result, "embeddings")
}

func TestEmbeddingsUnknownSubcommand(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/crushrc"
	_, err := LoadShellConfig(t.Context(), path, []byte(`embeddings bogus`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown subcommand")
}
