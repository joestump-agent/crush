package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	a2ui "github.com/tmc/a2ui"
)

// This file renders the semantic tools' results as A2UI surfaces alongside
// their model-facing text. The payloads travel in response metadata (the
// same channel read_mcp_resource uses), so the chat UI draws a live,
// theme-styled surface under the tool call while the model keeps a compact
// text digest it can act on. When no chat UI will render metadata
// (channel-originated turns, disable_a2ui deployments, headless runs) the
// tools fall back to their original plain-text output, so behavior off the
// interactive UI is unchanged.
//
// @joestump-agent 08/25/2026 - Initial version for semantic_search and
// semantic_index.
//
// @joestump 08/25/2026 - Marked the diverted text model-only so the chat
// stops drawing the digest underneath the card, restored the blank line
// between headless results, and pluralized the surface's result count.

// a2uiSurfaceIDPrefix namespaces the semantic tools' surface IDs so they
// cannot collide with MCP-served surfaces sharing an ID.
const a2uiSurfaceIDPrefix = "semantic-"

// semanticDivert reports whether this turn carries an interactive chat UI
// that will render tool-result metadata as a live A2UI surface. Mirrors the
// gating in read_mcp_resource: channel turns need the model to relay the
// data, disable_a2ui opts out of surfaces, and a zero content width means
// no UI tagged the turn.
func semanticDivert(ctx context.Context, cfg *config.ConfigStore) bool {
	return GetChannelFromContext(ctx) == "" &&
		cfg != nil &&
		!cfg.Config().Options.DisableA2UI &&
		GetContentWidthFromContext(ctx) > 0
}

// withSemanticSurface returns resp with the given components attached as a
// UI-only A2UI surface in response metadata. The model never sees the
// payload, so it cannot echo the JSON back and double-render the surface.
func withSemanticSurface(resp fantasy.ToolResponse, surfaceID string, components []a2ui.Component) fantasy.ToolResponse {
	msg := a2ui.ServerMessage{
		Version: a2ui.Version,
		UpdateComponents: &a2ui.UpdateComponents{
			SurfaceID:  surfaceID,
			Components: components,
		},
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return resp
	}
	metadata := ReadMCPResourceResponseMetadata{
		A2UISurfaces: []string{"<a2ui-json>" + string(payload) + "</a2ui-json>"},
		// resp's text is the model's digest of the same results the surface
		// draws. Without this the chat would render the card and then repeat
		// the digest underneath it.
		TextIsModelOnly: true,
	}
	return fantasy.WithResponseMetadata(resp, metadata)
}

// semanticSearchSurface builds the results card for a semantic_search call:
// a header with the query and hit count, then one block per result with the
// location line, relevance score, and a code snippet. Components form a
// flat id-linked tree per the A2UI wire format.
func semanticSearchSurface(query string, results []semanticSearchHit) []a2ui.Component {
	text := func(id, s string) a2ui.Component {
		return a2ui.Component{ID: id, Text: &a2ui.TextComponent{Text: a2ui.StringLiteral(s)}}
	}
	caption := func(id, s string) a2ui.Component {
		return a2ui.Component{ID: id, Text: &a2ui.TextComponent{
			Text:    a2ui.StringLiteral(s),
			Variant: a2ui.TextVariantCaption,
		}}
	}

	ids := newA2UIIDGen()
	titleID := ids.next()
	subID := ids.next()

	var blockIDs []string
	var extra []a2ui.Component
	for _, r := range results {
		loc := r.Path
		if r.Symbol != "" {
			loc += " :: " + r.Symbol
		}
		block := ids.next()
		line := ids.next()
		score := ids.next()
		snippet := ids.next()
		blockIDs = append(blockIDs, block)
		extra = append(extra,
			a2ui.Component{ID: block, Column: &a2ui.ColumnComponent{
				Children: a2ui.ChildList{IDs: []string{line, score, snippet}},
			}},
			text(line, loc),
			caption(score, fmt.Sprintf("lines %d-%d · score %.3f", r.StartLine+1, r.EndLine+1, r.Score)),
			caption(snippet, firstLines(r.Snippet, 4)),
		)
	}

	return append([]a2ui.Component{
		{ID: "root", Card: &a2ui.CardComponent{Child: "col"}},
		{ID: "col", Column: &a2ui.ColumnComponent{
			Children: a2ui.ChildList{IDs: append([]string{titleID, subID}, blockIDs...)},
		}},
		{ID: titleID, Text: &a2ui.TextComponent{
			Text:    a2ui.StringLiteral(fmt.Sprintf("Semantic search: %q", query)),
			Variant: a2ui.TextVariantH3,
		}},
		caption(subID, fmt.Sprintf("%d %s", len(results), pluralize(len(results), "result"))),
	}, extra...)
}

// semanticIndexSurface builds the summary card for a semantic_index run:
// chunk totals plus any per-file errors.
func semanticIndexSurface(indexed, skipped, failed, total int, errs []string) []a2ui.Component {
	caption := func(id, s string) a2ui.Component {
		return a2ui.Component{ID: id, Text: &a2ui.TextComponent{
			Text:    a2ui.StringLiteral(s),
			Variant: a2ui.TextVariantCaption,
		}}
	}

	ids := newA2UIIDGen()
	titleID := ids.next()
	statsID := ids.next()
	totalID := ids.next()

	childIDs := []string{titleID, statsID, totalID}
	var extra []a2ui.Component
	for _, e := range errs {
		id := ids.next()
		childIDs = append(childIDs, id)
		extra = append(extra, caption(id, "error: "+e))
	}

	failedPart := ""
	if failed > 0 {
		failedPart = fmt.Sprintf(" · %d failed", failed)
	}

	return append([]a2ui.Component{
		{ID: "root", Card: &a2ui.CardComponent{Child: "col"}},
		{ID: "col", Column: &a2ui.ColumnComponent{
			Children: a2ui.ChildList{IDs: childIDs},
		}},
		{ID: titleID, Text: &a2ui.TextComponent{
			Text:    a2ui.StringLiteral("Semantic index updated"),
			Variant: a2ui.TextVariantH3,
		}},
		caption(statsID, fmt.Sprintf("%d chunks indexed · %d files unchanged%s", indexed, skipped, failedPart)),
		caption(totalID, fmt.Sprintf("Index holds %d chunks", total)),
	}, extra...)
}

// semanticSearchHit is the subset of semantic.SearchResult the surface and
// the model-facing digest are built from.
type semanticSearchHit struct {
	Path      string
	Symbol    string
	StartLine int
	EndLine   int
	Score     float64
	Snippet   string
}

// semanticSearchDigest renders the compact model-facing result list. It
// deliberately omits snippets when a surface shows them to the user — the
// tool description already steers the model to view for authoritative
// content, and dropping the snippets saves the corresponding tokens.
func semanticSearchDigest(results []semanticSearchHit, withSnippets bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d results:\n\n", len(results))
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s", i+1, r.Path)
		if r.Symbol != "" {
			fmt.Fprintf(&b, " :: %s", r.Symbol)
		}
		fmt.Fprintf(&b, " (lines %d-%d, score %.3f)\n", r.StartLine+1, r.EndLine+1, r.Score)
		if withSnippets {
			snippet := firstLines(r.Snippet, 5)
			// The trailing blank line is part of the pre-A2UI format this
			// path must reproduce byte for byte on headless runs.
			fmt.Fprintf(&b, "   %s\n\n", strings.ReplaceAll(snippet, "\n", "\n   "))
		}
	}
	return b.String()
}

// pluralize appends an s to word unless n is exactly 1. The surface is the
// user-facing copy, so "1 results" would read as a bug there; the
// model-facing digest keeps its original wording.
func pluralize(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// firstLines truncates s to at most n lines, marking the cut.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + "\n..."
}

// a2uiIDGen mints unique component ids for one surface. Ids only need to be
// unique within the surface, but a stable scheme keeps output deterministic
// for tests.
type a2uiIDGen struct {
	n int
}

func newA2UIIDGen() *a2uiIDGen {
	return &a2uiIDGen{}
}

func (g *a2uiIDGen) next() string {
	g.n++
	return fmt.Sprintf("c%d", g.n)
}
