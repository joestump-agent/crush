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
	"github.com/alecthomas/chroma/v2/lexers"
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
	components := semanticSearchSurface("where is auth handled", "", 0, hits)
	validateSurfaceComponents(t, components)
	// Card, column, title, subtitle, then per hit: a block column, a head, a
	// meta line and a snippet — with a divider between consecutive hits.
	require.Len(t, components, 4+4*len(hits)+(len(hits)-1))
}

// The card is where the user reads a hit, so it has to carry the things the
// digest only names: which lines the chunk spans, where to jump, how relevant
// it is, and what the code looks like.
func TestSemanticSearchSurfaceHitDetail(t *testing.T) {
	t.Parallel()
	components := semanticSearchSurface("q", "", 0, []semanticSearchHit{
		{Path: "internal/auth/token.go", Symbol: "Verify", StartLine: 41, EndLine: 88, Score: 0.734, Snippet: "func Verify() error {\n\treturn nil\n}"},
	})
	text := surfaceText(t, components)

	// Rank plus a path:line jump target, and the range and score below it.
	require.Contains(t, text, "1. internal/auth/token.go:42")
	require.Contains(t, text, "Verify · lines 42–89 · ")
	require.Contains(t, text, "0.734")
	// A meter beside the score, so neighboring hits compare at a glance.
	require.Contains(t, text, scoreMeter(0.734))
	// The snippet is a Go-tagged fence with a line-number gutter starting at
	// the chunk's own first line.
	require.Contains(t, text, "```go\n42  func Verify() error {\n43      return nil\n44  }\n```")
}

// Scores are 1 - cosine distance; the meter clamps rather than repeating a
// negative count of blocks or overflowing its width.
func TestScoreMeter(t *testing.T) {
	t.Parallel()
	require.Equal(t, strings.Repeat("░", scoreMeterCells), scoreMeter(0))
	require.Equal(t, strings.Repeat("█", scoreMeterCells), scoreMeter(1))
	require.Equal(t, strings.Repeat("░", scoreMeterCells), scoreMeter(-2))
	require.Equal(t, strings.Repeat("█", scoreMeterCells), scoreMeter(9))
	require.Equal(t, strings.Repeat("█", 5)+strings.Repeat("░", 5), scoreMeter(0.5))
}

// The store holds absolute paths. Showing them verbatim spends most of a
// card's width on the part every hit shares.
func TestSemanticSearchSurfaceRelativizesPaths(t *testing.T) {
	t.Parallel()
	// t.TempDir is absolute on every platform. A hand-built "/src/repo" is
	// not absolute on Windows, so relativizePath would decline it and the
	// test would pass everywhere the separator happens to be "/".
	wd := t.TempDir()
	inside := filepath.Join(wd, "internal", "auth.go")
	outside := filepath.Join(filepath.Dir(wd), "elsewhere.go")

	text := surfaceText(t, semanticSearchSurface("q", wd, 0, []semanticSearchHit{
		{Path: inside, Score: 0.5, Snippet: "x"},
		{Path: outside, Score: 0.4, Snippet: "y"},
	}))
	require.Contains(t, text, "1. "+filepath.Join("internal", "auth.go")+":1")
	// A path outside the working directory stays absolute rather than
	// becoming a pile of "../".
	require.Contains(t, text, "2. "+outside+":1")
}

// A long line wrapped by the host's Markdown renderer restarts in column
// zero, which breaks the gutter the block exists to provide.
func TestNumberedSnippetTruncatesToWidth(t *testing.T) {
	t.Parallel()
	out := numberedSnippet(strings.Repeat("x", 200), 1, 6, 20)
	require.Equal(t, "1  "+strings.Repeat("x", 16)+"…", out)
	for _, line := range strings.Split(out, "\n") {
		require.LessOrEqual(t, len([]rune(line)), 20)
	}
}

// Chunks arrive indented at their nesting depth; spending a card's width on
// leading space is the one thing a preview cannot afford.
func TestNumberedSnippetDedentsAndElides(t *testing.T) {
	t.Parallel()
	snippet := "\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\tmore()"
	require.Equal(t,
		"1  if err != nil {\n2      return err\n3  }\n   …",
		numberedSnippet(snippet, 1, 3, 60))
}

// An empty or whitespace-only chunk gets no code block at all — an empty
// fence renders as a stray box.
func TestNumberedSnippetSkipsEmpty(t *testing.T) {
	t.Parallel()
	require.Empty(t, numberedSnippet("", 1, 6, 80))
	require.Empty(t, numberedSnippet("  \n\t\n", 1, 6, 80))
	require.Len(t, semanticSearchSurface("q", "", 0, []semanticSearchHit{
		{Path: "a.go", Score: 0.5},
	}), 4+3)
}

// The fence language is what buys the surface real syntax highlighting from
// the host's Markdown renderer, so every extension the indexer accepts needs
// one — and it has to name a lexer that actually exists.
func TestFenceLanguageCoversIndexableExtensions(t *testing.T) {
	t.Parallel()
	for ext := range indexableExtensions {
		lang := fenceLanguage("file" + ext)
		require.NotEmpty(t, lang, "no fence language for %s", ext)
		lexer := lexers.Get(lang)
		require.NotNil(t, lexer, "fence language %q for %s is not a chroma lexer", lang, ext)
		require.NotEqual(t, lexers.Fallback.Config().Name, lexer.Config().Name,
			"fence language %q for %s resolves to the fallback lexer", lang, ext)
	}
	// An extension the indexer never produces still fences, just untagged.
	require.Empty(t, fenceLanguage("a.bin"))
	require.True(t, strings.HasPrefix(fenceCode("x", fenceLanguage("a.bin")), "```\n"))
}

// Markdown is indexable, so a chunk can carry fences of its own. A
// three-backtick wrapper would end at the chunk's first fence and dump the
// rest of the snippet into the card as prose.
func TestFenceCodeEscapesNestedFences(t *testing.T) {
	t.Parallel()
	code := "Example:\n```go\nx := 1\n```"
	out := fenceCode(code, "markdown")
	require.True(t, strings.HasPrefix(out, "````markdown\n"), out)
	require.True(t, strings.HasSuffix(out, "\n````"), out)
	require.Contains(t, out, code)
}

func TestSemanticIndexSurfaceLinks(t *testing.T) {
	components := semanticIndexSurface("", 3, 5, 1, 9, []string{"x.go: boom"})
	validateSurfaceComponents(t, components)
}

// The index tool stops collecting errors after a handful, so a list that
// reads as the complete story is a lie. It also formats them as
// "<path>: <err>" with an absolute path and several layers of escaped
// provider JSON, which wraps into an unreadable wall inside a card.
func TestSemanticIndexSurfaceErrors(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	errs := []string{
		filepath.Join(wd, "a.go") + ": generate embeddings: embedding API returned 422:\n" + strings.Repeat("{\"error\": \"deep\"}", 40),
	}
	components := semanticIndexSurface(wd, 3, 5, 12, 9, errs)
	validateSurfaceComponents(t, components)
	text := surfaceText(t, components)

	require.Contains(t, text, "12 files failed to index — first 1 shown")
	// The path is split off and relativized; the message is flattened onto
	// one line and capped.
	require.Contains(t, text, "\na.go\n")
	require.Contains(t, text, "generate embeddings: embedding API returned 422: {\"error\"")
	require.NotContains(t, text, "422:\n")
	for _, line := range strings.Split(text, "\n") {
		require.LessOrEqual(t, len([]rune(line)), errorMessageLimit+1)
	}

	// No "— first N shown" when the list is complete.
	require.NotContains(t, surfaceText(t, semanticIndexSurface(wd, 3, 5, 1, 9, errs)), "first")

	// A root that could not be walked produces an error charged to no file;
	// "0 files failed to index" over a populated list would read as a bug.
	require.Contains(t,
		surfaceText(t, semanticIndexSurface(wd, 0, 0, 0, 0, []string{"docs: lstat docs: no such file or directory"})),
		"1 error")
}

// An error with no "<path>: " prefix keeps its whole text rather than losing
// its first clause to a path column.
func TestSplitIndexError(t *testing.T) {
	t.Parallel()
	path, msg := splitIndexError("", "x.go: boom")
	require.Equal(t, "x.go", path)
	require.Equal(t, "boom", msg)

	path, msg = splitIndexError("", "indexing cancelled: context deadline exceeded")
	require.Empty(t, path)
	require.Equal(t, "indexing cancelled: context deadline exceeded", msg)
}

// TestSemanticSurfacesRender guards the surfaces against drifting from what
// the pinned a2tea actually renders: a surface that only draws placeholders
// would fail silently in chat, since the model-facing text still looks fine.
func TestSemanticSurfacesRender(t *testing.T) {
	t.Parallel()
	for name, comps := range map[string][]a2ui.Component{
		"search": semanticSearchSurface("where is auth handled", "", 0, []semanticSearchHit{
			{Path: "a.go", Symbol: "Foo", StartLine: 9, EndLine: 20, Score: 0.912, Snippet: "func Foo() {\n\treturn\n}"},
		}),
		"index": semanticIndexSurface("", 3, 5, 0, 9, nil),
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

// The diverted text is a digest of the same hits the surface draws, so it
// must be flagged model-only — otherwise the chat renders the card and then
// repeats the digest as flat text underneath it.
func TestSemanticSurfaceMarksTextModelOnly(t *testing.T) {
	t.Parallel()
	resp := withSemanticSurface(fantasy.NewTextResponse("digest"), a2uiSurfaceIDPrefix+"search",
		semanticSearchSurface("q", "", 0, []semanticSearchHit{{Path: "a.go", Score: 0.5, Snippet: "x"}}))
	var meta ReadMCPResourceResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.True(t, meta.TextIsModelOnly)
}

// The headless path must reproduce the pre-A2UI text byte for byte: the
// blank line between results was part of it, and dropping it silently
// changed every non-interactive semantic_search result.
func TestSemanticSearchDigestHeadlessFormat(t *testing.T) {
	t.Parallel()
	hits := []semanticSearchHit{
		{Path: "a.go", Symbol: "Foo", StartLine: 0, EndLine: 1, Score: 0.5, Snippet: "one\ntwo"},
		{Path: "b.go", StartLine: 4, EndLine: 4, Score: 0.25, Snippet: "solo"},
	}
	require.Equal(t, "Found 2 results:\n\n"+
		"1. a.go :: Foo (lines 1-2, score 0.500)\n   one\n   two\n\n"+
		"2. b.go (lines 5-5, score 0.250)\n   solo\n\n",
		semanticSearchDigest(hits, true))

	require.Equal(t, "Found 2 results:\n\n"+
		"1. a.go :: Foo (lines 1-2, score 0.500)\n"+
		"2. b.go (lines 5-5, score 0.250)\n",
		semanticSearchDigest(hits, false))
}

// The surface is user-facing copy, so a single hit reads "1 result".
func TestSemanticSearchSurfacePluralizesCount(t *testing.T) {
	t.Parallel()
	one := semanticSearchSurface("q", "", 0, []semanticSearchHit{{Path: "a.go", Score: 0.5, Snippet: "x"}})
	require.Contains(t, surfaceText(t, one), "1 result")
	require.NotContains(t, surfaceText(t, one), "1 results")
	two := semanticSearchSurface("q", "", 0, []semanticSearchHit{
		{Path: "a.go", Score: 0.5, Snippet: "x"},
		{Path: "b.go", Score: 0.4, Snippet: "y"},
	})
	require.Contains(t, surfaceText(t, two), "2 results")
}

// surfaceText joins every Text component's literal in a surface.
func surfaceText(t *testing.T, components []a2ui.Component) string {
	t.Helper()
	var parts []string
	for _, c := range components {
		if c.Text != nil && c.Text.Text.Literal != nil {
			parts = append(parts, *c.Text.Text.Literal)
		}
	}
	return strings.Join(parts, "\n")
}
