package agent

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/semantic"
	"github.com/charmbracelet/crush/internal/symbols"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

func coordinatorToolNames(t *testing.T, coord *coordinator) []string {
	t.Helper()
	agentCfg := coord.cfg.Config().Agents[config.AgentCoder]
	built, err := coord.buildTools(context.Background(), agentCfg, false)
	require.NoError(t, err)
	names := make([]string, len(built))
	for i, tool := range built {
		names[i] = tool.Info().Name
	}
	return names
}

func semanticTestStore(t *testing.T) *semantic.Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS chunks (
			chunk_id   INTEGER PRIMARY KEY,
			session_id TEXT,
			path       TEXT,
			symbol     TEXT,
			start_line INTEGER,
			end_line   INTEGER,
			content    TEXT NOT NULL,
			file_hash  TEXT,
			model      TEXT NOT NULL,
			dim        INTEGER NOT NULL
		) STRICT`)
	require.NoError(t, err)
	store, err := semantic.NewStore(context.Background(), db, semantic.EmbeddingConfig{
		BaseURL: "http://127.0.0.1:9/v1", Model: "test", Dimension: 3,
	})
	require.NoError(t, err)
	return store
}

// TestSemanticToolsGatedOnStore pins the registration rule: the
// semantic_search and semantic_index tools exist in the coder's palette
// only when an embedding provider is configured and the store initialised.
func TestSemanticToolsGatedOnStore(t *testing.T) {
	t.Run("absent without store", func(t *testing.T) {
		coord := newGateTestCoordinator(t, false)
		names := coordinatorToolNames(t, coord)
		require.NotContains(t, names, "semantic_search")
		require.NotContains(t, names, "semantic_index")
	})

	t.Run("present with store", func(t *testing.T) {
		coord := newGateTestCoordinator(t, false)
		emb := semantic.EmbeddingConfig{
			BaseURL: "http://127.0.0.1:9/v1", Model: "test", Dimension: 3,
		}
		coord.semanticStore = semanticTestStore(t)
		coord.semanticClient = semantic.NewClient(emb)
		coord.semanticSymbols = symbols.NewExtractor()

		names := coordinatorToolNames(t, coord)
		require.Contains(t, names, "semantic_search")
		require.Contains(t, names, "semantic_index")
	})

	t.Run("default palette allows the tools", func(t *testing.T) {
		coord := newGateTestCoordinator(t, false)
		allowed := coord.cfg.Config().Agents[config.AgentCoder].AllowedTools
		require.True(t, slices.Contains(allowed, "semantic_search"))
		require.True(t, slices.Contains(allowed, "semantic_index"))
	})
}
