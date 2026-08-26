package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/semantic"
	"github.com/charmbracelet/crush/internal/symbols"
)

const SemanticIndexToolName = "semantic_index"

// SemanticIndexParams are the parameters for the semantic_index tool.
type SemanticIndexParams struct {
	Paths []string `json:"paths,omitempty" jsonschema:"description=Optional files or directories to index, relative to the working directory. Defaults to the whole working directory."`
}

// indexableExtensions is the allowlist of file extensions worth embedding.
// The extractor falls back to whole-file chunking for unknown languages,
// which is fine for source-adjacent text but worthless (and costly) for
// binaries, images, and lockfiles.
var indexableExtensions = map[string]bool{
	".bash": true, ".c": true, ".cc": true, ".cljc": true, ".clj": true,
	".cpp": true, ".cs": true, ".css": true, ".cxx": true, ".dart": true,
	".elixir": true, ".elm": true, ".ex": true, ".exs": true, ".go": true,
	".groovy": true, ".h": true, ".hpp": true, ".hs": true, ".htm": true,
	".html": true, ".java": true, ".js": true, ".json": true, ".jsx": true,
	".kt": true, ".kts": true, ".lua": true, ".md": true, ".mjs": true,
	".ml": true, ".mts": true, ".php": true, ".py": true, ".rb": true,
	".rs": true, ".scala": true, ".scm": true, ".sh": true, ".sql": true,
	".svelte": true, ".swift": true, ".toml": true, ".ts": true, ".tsx": true,
	".vue": true, ".xml": true, ".yaml": true, ".yml": true, ".zig": true,
}

// NewSemanticIndexTool creates a tool that builds or refreshes the
// semantic search index. Files whose contents and embedding model have
// not changed since the last index are skipped, so repeated runs are
// incremental.
func NewSemanticIndexTool(cfg *config.ConfigStore, store *semantic.Store, extractor *symbols.Extractor, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		SemanticIndexToolName,
		semanticIndexDescription(),
		func(ctx context.Context, params SemanticIndexParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if store == nil {
				return fantasy.NewTextErrorResponse("Semantic search index is not configured. Set up an embedding provider in crush.json to enable semantic search."), nil
			}

			roots := params.Paths
			if len(roots) == 0 {
				roots = []string{"."}
			}

			var (
				indexed int
				skipped int
				failed  int
				errs    []string
			)
			for _, root := range roots {
				absRoot := root
				if !filepath.IsAbs(absRoot) {
					absRoot = filepath.Join(workingDir, root)
				}

				// The glob walker is gitignore-aware and skips hidden
				// directories, so vendor trees and .git never reach the
				// embedding endpoint. The limit keeps a pathological
				// repository from running away; 10k files is already an
				// expensive index.
				files, _, err := fsext.GlobGitignoreAwareCtx(ctx, "**/*", absRoot, 10000)
				if err != nil {
					errs = append(errs, fmt.Sprintf("%s: %v", root, err))
					continue
				}

				for _, path := range files {
					if ctx.Err() != nil {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("Indexing cancelled: %v", ctx.Err())), nil
					}
					if indexableExtensions[strings.ToLower(filepath.Ext(path))] {
						n, skip, err := store.IndexFile(ctx, extractor, path)
						if err != nil {
							failed++
							if len(errs) < 10 {
								errs = append(errs, fmt.Sprintf("%s: %v", path, err))
							}
							continue
						}
						if skip {
							skipped++
						}
						indexed += n
					}
				}
			}

			total, _ := store.ChunkCount(ctx)

			var b strings.Builder
			fmt.Fprintf(&b, "Indexed %d chunks from newly indexed or changed files. %d files unchanged and skipped.", indexed, skipped)
			if failed > 0 {
				fmt.Fprintf(&b, " %d files failed.", failed)
			}
			fmt.Fprintf(&b, " Index now holds %d chunks.\n", total)
			for _, e := range errs {
				fmt.Fprintf(&b, "error: %s\n", e)
			}
			resp := fantasy.NewTextResponse(b.String())
			// The summary card renders as a live A2UI surface when a chat UI
			// is attached; the text above stays model-facing either way.
			if semanticDivert(ctx, cfg) {
				resp = withSemanticSurface(resp, a2uiSurfaceIDPrefix+"index", semanticIndexSurface(workingDir, indexed, skipped, failed, int(total), errs))
			}
			return resp, nil
		},
	)
}

func semanticIndexDescription() string {
	return `Build or refresh the semantic search index for this repository. Files are chunked by symbol, embedded through the configured embedding provider, and stored in the project database. Unchanged files are skipped, so this is incremental and safe to re-run.

Run this before semantic_search when the index is empty or stale (for example after pulling changes or editing many files). With no arguments it indexes the whole working directory (gitignore-aware); pass specific files or directories to narrow the run.`
}
