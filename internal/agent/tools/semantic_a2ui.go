package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/x/ansi"
	a2ui "github.com/tmc/a2ui"
)

// Semantic Tool Result Surfaces
//
// This file renders the semantic tools' results as A2UI surfaces alongside
// their model-facing text. The payloads travel in response metadata (the
// same channel read_mcp_resource uses), so the chat UI draws a live,
// theme-styled surface under the tool call while the model keeps a compact
// text digest it can act on. When no chat UI will render metadata
// (channel-originated turns, disable_a2ui deployments, headless runs) the
// tools fall back to their original plain-text output, so behavior off the
// interactive UI is unchanged.
//
// Snippets ride in a body-variant Text as a fenced code block. a2tea routes
// body Text that looks like Markdown through the host's MarkdownRenderer,
// which in Crush is glamour + chroma — so the fence language is what buys
// the surface real syntax highlighting. Nothing here reaches into the UI
// packages to do it: any A2UI host with a Markdown renderer gets the same
// result from the same payload.
//
// @joestump-agent 08/25/2026 - Initial version for semantic_search and
// semantic_index.
//
// @joestump 08/25/2026 - Marked the diverted text model-only so the chat
// stops drawing the digest underneath the card, restored the blank line
// between headless results, and pluralized the surface's result count.
//
// @joestump-agent 08/25/2026 - Reworked both cards: search hits are now
// ranked blocks with a workdir-relative path, a jump target, a line range, a
// score meter and a line-numbered, syntax-highlighted snippet; the index card
// splits its per-file errors into path + condensed message and stops
// silently dropping the ones past the tool's cap.

// a2uiSurfaceIDPrefix namespaces the semantic tools' surface IDs so they
// cannot collide with MCP-served surfaces sharing an ID.
const a2uiSurfaceIDPrefix = "semantic-"

const (
	// snippetLines is how many lines of a hit's chunk the card shows before
	// eliding. The card is a scanning aid, not a viewer — the model is told
	// to open the file for authoritative content.
	snippetLines = 6
	// scoreMeterCells is the width of the relevance meter drawn beside each
	// score, so neighboring hits can be compared at a glance.
	scoreMeterCells = 10
	// gutterTabWidth expands tabs in snippets. Terminal tab stops interact
	// badly with the code block's own indentation, and a fixed expansion
	// keeps the line-number gutter aligned.
	gutterTabWidth = 4
	// errorMessageLimit caps a per-file index error. Provider errors nest
	// escaped JSON several layers deep; the leading clause is the part that
	// says what actually went wrong.
	errorMessageLimit = 160
	// defaultSnippetWidth sizes snippet lines when the turn carries no width
	// hint, and minSnippetWidth floors the computed one so a very narrow pane
	// cannot truncate every line down to its gutter.
	defaultSnippetWidth = 100
	minSnippetWidth     = 40
)

// Snippet line widths
//
// A code block whose lines overflow the pane is word-wrapped by the host's
// Markdown renderer, and the continuation lands in column zero — which breaks
// the line-number gutter the block exists to provide. Truncating instead
// keeps the grid readable, and a search hit is a scanning aid: the model is
// told to open the file for the authoritative content.
//
// The chat pane's width reaches the tool through the same content-width hint
// MCP surfaces get, so the payload can be sized to it. The arithmetic mirrors
// how the chat nests a surface — message padding, then tool-body padding,
// then the card's border and padding — and being a cell or two off only costs
// an occasional wrap, so it is a hint, not a contract.
//
// @joestump-agent 08/25/2026 - Added with the reworked search card.
func snippetWidth(contentWidth int) int {
	if contentWidth <= 0 {
		return defaultSnippetWidth
	}
	w := min(contentWidth-messageLeftPadding, maxMessageWidth) - toolBodyLeftPadding - cardChrome - codeBlockMargin
	return max(w, minSnippetWidth)
}

// The chat layout constants the width hint has to be walked through. They are
// duplicated rather than imported because internal/ui must not be a
// dependency of the tool layer; a drift here degrades to a wrapped line.
const (
	messageLeftPadding  = 2
	maxMessageWidth     = 120
	toolBodyLeftPadding = 2
	cardChrome          = 4
	// codeBlockMargin is the Markdown renderer's own indent on a fenced
	// block, on both sides.
	codeBlockMargin = 4
)

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

// a2uiText builds a body-variant Text component. Body text is the only
// variant a2tea routes through the host's Markdown renderer, so this is what
// carries fenced code.
func a2uiText(id, s string) a2ui.Component {
	return a2ui.Component{ID: id, Text: &a2ui.TextComponent{Text: a2ui.StringLiteral(s)}}
}

// a2uiCaption builds a caption-variant Text component — the muted secondary
// line under a heading.
func a2uiCaption(id, s string) a2ui.Component {
	return a2ui.Component{ID: id, Text: &a2ui.TextComponent{
		Text:    a2ui.StringLiteral(s),
		Variant: a2ui.TextVariantCaption,
	}}
}

// a2uiHeading builds a Text component at the given heading variant.
func a2uiHeading(id, s string, v a2ui.TextVariant) a2ui.Component {
	return a2ui.Component{ID: id, Text: &a2ui.TextComponent{
		Text:    a2ui.StringLiteral(s),
		Variant: v,
	}}
}

// semanticSearchSurface builds the results card for a semantic_search call.
// The header names the query and the hit count; each hit below it is a block
// of three lines — a ranked jump target (path:line), a metadata line
// carrying the symbol, the line range and a relevance meter, and the chunk's
// opening lines as a line-numbered, syntax-highlighted code block. Blocks are
// separated by dividers. Components form a flat id-linked tree per the A2UI
// wire format.
func semanticSearchSurface(query, workingDir string, contentWidth int, results []semanticSearchHit) []a2ui.Component {
	ids := newA2UIIDGen()
	titleID := ids.next()
	subID := ids.next()
	lineWidth := snippetWidth(contentWidth)

	childIDs := []string{titleID, subID}
	var extra []a2ui.Component
	for i, r := range results {
		if i > 0 {
			divID := ids.next()
			childIDs = append(childIDs, divID)
			extra = append(extra, a2ui.Component{ID: divID, Divider: &a2ui.DividerComponent{}})
		}

		path := relativizePath(workingDir, r.Path)
		blockID := ids.next()
		headID := ids.next()
		metaID := ids.next()
		blockChildren := []string{headID, metaID}

		block := []a2ui.Component{
			a2uiHeading(headID, fmt.Sprintf("%d. %s:%d", i+1, path, r.StartLine+1), a2ui.TextVariantH5),
			a2uiCaption(metaID, searchHitMeta(r)),
		}
		if snippet := numberedSnippet(r.Snippet, r.StartLine+1, snippetLines, lineWidth); snippet != "" {
			codeID := ids.next()
			blockChildren = append(blockChildren, codeID)
			block = append(block, a2uiText(codeID, fenceCode(snippet, fenceLanguage(path))))
		}

		childIDs = append(childIDs, blockID)
		extra = append(extra, a2ui.Component{ID: blockID, Column: &a2ui.ColumnComponent{
			Children: a2ui.ChildList{IDs: blockChildren},
		}})
		extra = append(extra, block...)
	}

	return append([]a2ui.Component{
		{ID: "root", Card: &a2ui.CardComponent{Child: "col"}},
		{ID: "col", Column: &a2ui.ColumnComponent{
			Children: a2ui.ChildList{IDs: childIDs},
		}},
		a2uiHeading(titleID, "Semantic search", a2ui.TextVariantH3),
		a2uiCaption(subID, fmt.Sprintf("%q · %d %s", query, len(results), pluralize(len(results), "result"))),
	}, extra...)
}

// searchHitMeta renders a hit's secondary line: the symbol when the chunk has
// one, the line range, and the relevance score behind a meter so neighboring
// hits compare at a glance.
func searchHitMeta(r semanticSearchHit) string {
	parts := make([]string, 0, 3)
	if r.Symbol != "" {
		parts = append(parts, r.Symbol)
	}
	parts = append(parts,
		fmt.Sprintf("lines %d–%d", r.StartLine+1, r.EndLine+1),
		fmt.Sprintf("%s %.3f", scoreMeter(r.Score), r.Score),
	)
	return strings.Join(parts, " · ")
}

// semanticIndexSurface builds the summary card for a semantic_index run:
// the chunk totals, then any per-file errors as a list of path + condensed
// message. errs is capped by the caller, so failed is what says how many
// files actually broke.
func semanticIndexSurface(workingDir string, indexed, skipped, failed, total int, errs []string) []a2ui.Component {
	ids := newA2UIIDGen()
	titleID := ids.next()
	statsID := ids.next()
	totalID := ids.next()

	childIDs := []string{titleID, statsID, totalID}
	var extra []a2ui.Component
	if len(errs) > 0 {
		divID := ids.next()
		errHeadID := ids.next()
		listID := ids.next()
		childIDs = append(childIDs, divID, errHeadID, listID)

		var errIDs []string
		for _, e := range errs {
			path, msg := splitIndexError(workingDir, e)
			entryID := ids.next()
			msgID := ids.next()
			entryChildren := []string{}
			var entry []a2ui.Component
			if path != "" {
				pathID := ids.next()
				entryChildren = append(entryChildren, pathID)
				entry = append(entry, a2uiText(pathID, path))
			}
			entryChildren = append(entryChildren, msgID)
			entry = append(entry, a2uiCaption(msgID, msg))

			errIDs = append(errIDs, entryID)
			extra = append(extra, a2ui.Component{ID: entryID, Column: &a2ui.ColumnComponent{
				Children: a2ui.ChildList{IDs: entryChildren},
			}})
			extra = append(extra, entry...)
		}

		// A root that could not be walked produces an error without a failed
		// file, so the heading counts errors when nothing was charged to a
		// file.
		head := fmt.Sprintf("%d %s", len(errs), pluralize(len(errs), "error"))
		if failed > 0 {
			head = fmt.Sprintf("%d %s failed to index", failed, pluralize(failed, "file"))
			if failed > len(errs) {
				// The tool stops collecting after a handful of errors. Saying
				// so beats a list that silently reads as the complete story.
				head += fmt.Sprintf(" — first %d shown", len(errs))
			}
		}
		extra = append(extra,
			a2ui.Component{ID: divID, Divider: &a2ui.DividerComponent{}},
			a2uiHeading(errHeadID, head, a2ui.TextVariantH5),
			a2ui.Component{ID: listID, List: &a2ui.ListComponent{
				Children: a2ui.ChildList{IDs: errIDs},
			}},
		)
	}

	stats := fmt.Sprintf("%d %s indexed · %d %s unchanged",
		indexed, pluralize(indexed, "chunk"), skipped, pluralize(skipped, "file"))
	if failed > 0 {
		stats += fmt.Sprintf(" · %d failed", failed)
	}

	return append([]a2ui.Component{
		{ID: "root", Card: &a2ui.CardComponent{Child: "col"}},
		{ID: "col", Column: &a2ui.ColumnComponent{
			Children: a2ui.ChildList{IDs: childIDs},
		}},
		a2uiHeading(titleID, "Semantic index updated", a2ui.TextVariantH3),
		a2uiCaption(statsID, stats),
		a2uiCaption(totalID, fmt.Sprintf("Index holds %d %s", total, pluralize(total, "chunk"))),
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

// relativizePath renders an indexed path relative to the working directory
// so the card shows repository-shaped paths instead of the absolute ones the
// store holds. Paths outside the working directory (and any path when the
// directory is unknown) are left alone rather than rendered as a pile of
// "../".
func relativizePath(workingDir, path string) string {
	if workingDir == "" || path == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(workingDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// scoreMeter draws a fixed-width bar for a similarity score. Scores are
// 1 - cosine distance, so they nominally land in [0,1]; the bar clamps
// rather than overflowing on an out-of-range value.
func scoreMeter(score float64) string {
	filled := int(math.Round(math.Max(0, math.Min(1, score)) * scoreMeterCells))
	return strings.Repeat("█", filled) + strings.Repeat("░", scoreMeterCells-filled)
}

// numberedSnippet renders at most maxLines lines of a chunk with a right-aligned
// line-number gutter starting at startLine (1-based), marking an elision with
// a gutter-aligned ellipsis. Tabs are expanded and the block's common
// indentation is stripped so a deeply nested chunk spends its width on code
// rather than leading space, and each line is truncated to width so the
// gutter survives (see snippetWidth).
func numberedSnippet(snippet string, startLine, maxLines, width int) string {
	snippet = strings.TrimRight(snippet, "\n")
	if strings.TrimSpace(snippet) == "" {
		return ""
	}
	lines := strings.Split(snippet, "\n")
	elided := false
	if len(lines) > maxLines {
		lines, elided = lines[:maxLines], true
	}
	for i, ln := range lines {
		lines[i] = strings.TrimRight(strings.ReplaceAll(ln, "\t", strings.Repeat(" ", gutterTabWidth)), " ")
	}
	lines = dedent(lines)

	gutter := len(strconv.Itoa(startLine + len(lines) - 1))
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%*d  %s\n", gutter, startLine+i, ansi.Truncate(ln, max(width-gutter-2, 1), "…"))
	}
	if elided {
		fmt.Fprintf(&b, "%*s  …", gutter, "")
	}
	return strings.TrimRight(b.String(), "\n")
}

// dedent strips the indentation common to every non-blank line.
func dedent(lines []string) []string {
	common := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " "))
		if common < 0 || n < common {
			common = n
		}
	}
	if common <= 0 {
		return lines
	}
	for i, ln := range lines {
		if len(ln) >= common {
			lines[i] = ln[common:]
		} else {
			lines[i] = strings.TrimLeft(ln, " ")
		}
	}
	return lines
}

// fenceCode wraps code in a Markdown fence tagged with lang. The fence is
// grown past the longest backtick run in the code so an indexed Markdown file
// — which can hold fences of its own — cannot break out of the block.
func fenceCode(code, lang string) string {
	longest, run := 0, 0
	for _, r := range code {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + lang + "\n" + code + "\n" + fence
}

// fenceLanguage maps a path to the Markdown fence tag the host's Markdown
// renderer highlights it with. Every extension the indexer accepts (see
// indexableExtensions) has an entry, so a hit can always be tagged; unknown
// extensions fall back to an untagged fence, which still renders as code.
var fenceLanguage = func() func(path string) string {
	byExt := map[string]string{
		".bash": "bash", ".c": "c", ".cc": "cpp", ".cljc": "clojure",
		".clj": "clojure", ".cpp": "cpp", ".cs": "csharp", ".css": "css",
		".cxx": "cpp", ".dart": "dart", ".elixir": "elixir", ".elm": "elm",
		".ex": "elixir", ".exs": "elixir", ".go": "go", ".groovy": "groovy",
		".h": "c", ".hpp": "cpp", ".hs": "haskell", ".htm": "html",
		".html": "html", ".java": "java", ".js": "javascript", ".json": "json",
		".jsx": "jsx", ".kt": "kotlin", ".kts": "kotlin", ".lua": "lua",
		".md": "markdown", ".mjs": "javascript", ".ml": "ocaml",
		".mts": "typescript", ".php": "php", ".py": "python", ".rb": "ruby",
		".rs": "rust", ".scala": "scala", ".scm": "scheme", ".sh": "bash",
		".sql": "sql", ".svelte": "svelte", ".swift": "swift", ".toml": "toml",
		".ts": "typescript", ".tsx": "tsx", ".vue": "vue", ".xml": "xml",
		".yaml": "yaml", ".yml": "yaml", ".zig": "zig",
	}
	return func(path string) string {
		return byExt[strings.ToLower(filepath.Ext(path))]
	}
}()

// splitIndexError pulls the path off a per-file index error, which the index
// tool formats as "<path>: <err>", and condenses the message. An error with
// no recognizable path prefix returns an empty path and the whole condensed
// string.
func splitIndexError(workingDir, e string) (path, msg string) {
	if i := strings.Index(e, ": "); i > 0 {
		if p := relativizePath(workingDir, e[:i]); !strings.Contains(p, " ") {
			return p, condenseError(e[i+2:])
		}
	}
	return "", condenseError(e)
}

// condenseError flattens an error onto one line and truncates it. Provider
// errors arrive as several layers of escaped JSON, which wraps into an
// unreadable wall inside a card; the leading clause carries the cause.
func condenseError(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if r := []rune(s); len(r) > errorMessageLimit {
		return strings.TrimRight(string(r[:errorMessageLimit]), " ") + "…"
	}
	return s
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
