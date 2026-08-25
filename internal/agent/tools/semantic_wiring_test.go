package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/semantic"
	"github.com/charmbracelet/crush/internal/symbols"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// newSemanticTestStore opens an in-memory store with the chunks table the
// migrations create, so NewStore only has to add the vec0 table.
func newSemanticTestStore(t *testing.T, baseURL string) *semantic.Store {
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
		BaseURL: baseURL, Model: "test-model", Dimension: 3,
	})
	require.NoError(t, err)
	return store
}

// embeddingServer serves an /embeddings endpoint returning fixed
// 3-dimensional vectors, one per input.
func embeddingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		out := "{\"data\":["
		for i := range req.Input {
			if i > 0 {
				out += ","
			}
			out += "{\"embedding\":[0.1,0.2,0.3],\"index\":" + strconv.Itoa(i) + "}"
		}
		out += "]}"
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(out))
		require.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runTool[T any](t *testing.T, tool fantasy.AgentTool, params T) (string, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	call := fantasy.ToolCall{ID: "test-call", Name: tool.Info().Name, Input: string(input)}
	resp, err := tool.Run(t.Context(), call)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func TestSemanticIndexToolIndexesFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644))
	// A binary extension must be skipped by the extension allowlist.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "img.png"), []byte("binary"), 0o644))

	srv := embeddingServer(t)
	store := newSemanticTestStore(t, srv.URL)

	tool := NewSemanticIndexTool(store, symbols.NewExtractor(), dir)
	resp, err := runTool(t, tool, SemanticIndexParams{})
	require.NoError(t, err)
	require.Contains(t, resp, "Index now holds 1 chunks")

	count, err := store.ChunkCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestSemanticIndexToolIsIncremental pins the claim in the tool's own
// description: a second run over unchanged files re-embeds nothing and
// reports them as skipped, so re-running is cheap rather than a full
// re-index at the embedding provider's expense.
func TestSemanticIndexToolIsIncremental(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644))

	srv := embeddingServer(t)
	store := newSemanticTestStore(t, srv.URL)
	tool := NewSemanticIndexTool(store, symbols.NewExtractor(), dir)

	first, err := runTool(t, tool, SemanticIndexParams{})
	require.NoError(t, err)
	require.Contains(t, first, "0 files unchanged and skipped")

	second, err := runTool(t, tool, SemanticIndexParams{})
	require.NoError(t, err)
	require.Contains(t, second, "Indexed 0 chunks")
	require.Contains(t, second, "1 files unchanged and skipped")

	// Idempotent: the second run must not duplicate the chunk.
	count, err := store.ChunkCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	// A changed file is re-indexed rather than skipped.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc Goodbye() string { return \"bye\" }\n"), 0o644))
	third, err := runTool(t, tool, SemanticIndexParams{})
	require.NoError(t, err)
	require.Contains(t, third, "0 files unchanged and skipped")

	// Re-indexing replaces the file's chunks rather than accumulating
	// stale ones alongside them.
	count, err = store.ChunkCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

func TestSemanticIndexToolUnconfigured(t *testing.T) {
	tool := NewSemanticIndexTool(nil, nil, t.TempDir())
	resp, err := runTool(t, tool, SemanticIndexParams{})
	require.NoError(t, err)
	require.Contains(t, resp, "not configured")
}

func TestSemanticSearchToolUnconfigured(t *testing.T) {
	tool := NewSemanticSearchTool(nil, nil)
	resp, err := runTool(t, tool, SemanticSearchParams{Query: "anything"})
	require.NoError(t, err)
	require.Contains(t, resp, "not configured")
}

func TestCrushInfoSemanticSection(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{
		Providers:  csync.NewMap[string, config.ProviderConfig](),
		Embeddings: &config.EmbeddingsConfig{Model: "test-model", Dimension: 512, APIKey: "sk-secret"},
	})
	store := newSemanticTestStore(t, "")
	out := buildCrushInfo(cfg, nil, nil, nil, nil, store)
	require.Contains(t, out, "[semantic_index]")
	require.Contains(t, out, "model = test-model (dim 512)")
	require.Contains(t, out, "chunks = 0")
	// The API key must never be echoed back.
	require.NotContains(t, out, "sk-secret")

	out = buildCrushInfo(cfg, nil, nil, nil, nil, nil)
	require.Contains(t, out, "store = unavailable")

	cfgNone := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	out = buildCrushInfo(cfgNone, nil, nil, nil, nil, nil)
	require.NotContains(t, out, "[semantic_index]")
}
