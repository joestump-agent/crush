package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	a2tea "github.com/joestump-agent/a2tea"
	a2ui "github.com/tmc/a2ui"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/semantic"
	"github.com/charmbracelet/crush/internal/symbols"
	"github.com/stretchr/testify/require"
)

// uiTurnContext mimics a turn tagged by the interactive chat UI: no channel
// origin and a positive content width, the two signals semanticDivert keys
// on.
func uiTurnContext(t *testing.T) context.Context {
	t.Helper()
	ctx := context.WithValue(t.Context(), ChannelContextKey, "")
	return context.WithValue(ctx, ContentWidthContextKey, 120)
}

// semanticTestConfig returns a config store with A2UI left at its default
// (enabled).
func semanticTestConfig(t *testing.T) *config.ConfigStore {
	t.Helper()
	return config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Options:   &config.Options{},
	})
}

// validateSurfaceComponents asserts the invariants the chat renderer relies
// on: every component has an id, ids are unique, and every child/children
// reference resolves to a component in the same flat list.
func validateSurfaceComponents(t *testing.T, components []a2ui.Component) {
	t.Helper()
	ids := map[string]bool{}
	for _, c := range components {
		require.NotEmpty(t, c.ID, "component must have an id")
		require.False(t, ids[c.ID], "duplicate component id %q", c.ID)
		ids[c.ID] = true
	}
	refs := []string{}
	for _, c := range components {
		if c.Card != nil {
			refs = append(refs, c.Card.Child)
		}
		if c.Column != nil {
			refs = append(refs, c.Column.Children.IDs...)
		}
		if c.Row != nil {
			refs = append(refs, c.Row.Children.IDs...)
		}
		if c.List != nil {
			refs = append(refs, c.List.Children.IDs...)
		}
	}
	for _, ref := range refs {
		require.True(t, ids[ref], "dangling child reference %q", ref)
	}
}

func TestSemanticSearchSurfaceLinks(t *testing.T) {
	hits := []semanticSearchHit{
		{Path: "a.go", Symbol: "Foo", StartLine: 9, EndLine: 20, Score: 0.912, Snippet: "func Foo() {\n\treturn\n}"},
		{Path: "b.go", StartLine: 0, EndLine: 3, Score: 0.734, Snippet: strings.Repeat("line\n", 10)},
	}
	components := semanticSearchSurface("where is auth handled", hits)
	validateSurfaceComponents(t, components)
	require.Len(t, components, 4+4*len(hits))
}

func TestSemanticIndexSurfaceLinks(t *testing.T) {
	components := semanticIndexSurface(3, 5, 1, 9, []string{"x.go: boom"})
	validateSurfaceComponents(t, components)
}

// TestSemanticSurfacesRender guards the surfaces against drifting from what
// the pinned a2tea actually renders: a surface that only draws placeholders
// would fail silently in chat, since the model-facing text still looks fine.
func TestSemanticSurfacesRender(t *testing.T) {
	t.Parallel()
	for name, comps := range map[string][]a2ui.Component{
		"search": semanticSearchSurface("where is auth handled", []semanticSearchHit{
			{Path: "a.go", Symbol: "Foo", StartLine: 9, EndLine: 20, Score: 0.912, Snippet: "func Foo() {\n\treturn\n}"},
		}),
		"index": semanticIndexSurface(3, 5, 0, 9, nil),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, err := a2tea.Render([]a2ui.ServerMessage{{
				Version:          a2ui.Version,
				UpdateComponents: &a2ui.UpdateComponents{SurfaceID: "s", Components: comps},
			}})
			require.NoError(t, err)
			out := m.View().Content
			require.NotContains(t, out, "[a2tea:")
			require.NotEmpty(t, strings.TrimSpace(out))
		})
	}
}

// TestSemanticSearchDivert pins the split: on a UI-tagged turn the model
// gets the snippet-free digest and the pretty card rides in response
// metadata; on a plain turn the text keeps its snippets and no metadata is
// attached.
func TestSemanticSearchDivert(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}]}`))
		require.NoError(t, err)
	}))
	t.Cleanup(srv.Close)

	store := newSemanticTestStore(t, srv.URL)
	client := semantic.NewClient(semantic.EmbeddingConfig{
		BaseURL: srv.URL, Model: "test-model", Dimension: 3,
	})

	indexTool := NewSemanticIndexTool(semanticTestConfig(t), store, symbols.NewExtractor(), dir)
	_, runErr := runTool(t, indexTool, SemanticIndexParams{})
	require.NoError(t, runErr)

	run := func(ctx context.Context, cfg *config.ConfigStore) fantasy.ToolResponse {
		tool := NewSemanticSearchTool(cfg, store, client)
		input, err := json.Marshal(SemanticSearchParams{Query: "hello"})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: SemanticSearchToolName, Input: string(input)})
		require.NoError(t, err)
		return resp
	}

	uiResp := run(uiTurnContext(t), semanticTestConfig(t))
	require.Contains(t, uiResp.Content, "main.go :: Hello")
	require.NotContains(t, uiResp.Content, "return \"hi\"")
	var meta ReadMCPResourceResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(uiResp.Metadata), &meta))
	require.Len(t, meta.A2UISurfaces, 1)
	require.True(t, strings.HasPrefix(meta.A2UISurfaces[0], "<a2ui-json>"))
	var msg a2ui.ServerMessage
	payload := strings.TrimSuffix(strings.TrimPrefix(meta.A2UISurfaces[0], "<a2ui-json>"), "</a2ui-json>")
	require.NoError(t, json.Unmarshal([]byte(payload), &msg))
	require.Equal(t, a2ui.Version, msg.Version)
	require.NotNil(t, msg.UpdateComponents)
	require.Equal(t, "semantic-search", msg.UpdateComponents.SurfaceID)
	validateSurfaceComponents(t, msg.UpdateComponents.Components)

	// Plain (headless) turn: snippets stay in the text, no surface metadata.
	plainResp := run(t.Context(), semanticTestConfig(t))
	require.Contains(t, plainResp.Content, "return")
	require.Empty(t, plainResp.Metadata)
}
