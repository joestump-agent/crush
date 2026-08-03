// Package symbols extracts code symbols (functions, methods, types) from
// source files using tree-sitter grammars via gotreesitter. It provides
// symbol-aware chunking boundaries for semantic search indexing.
package symbols

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// Symbol represents a named code entity extracted from a source file.
type Symbol struct {
	Name      string // qualified name (e.g. "FuncName")
	Kind      string // tag kind (e.g. "definition.function")
	StartLine int    // 0-based start line
	EndLine   int    // 0-based end line (inclusive)
}

// ExtractResult holds the symbols extracted from a single file, or an
// error/fallback indicator if extraction could not proceed.
type ExtractResult struct {
	Path     string
	Symbols  []Symbol
	Fallback bool   // true if fixed-size chunking should be used instead
	Reason   string // why fallback was triggered (empty if Symbols populated)
}

// Extractor extracts symbols from source files using gotreesitter.
type Extractor struct {
	mu sync.Mutex
}

// NewExtractor creates a symbol extractor. It configures the grammar
// cache limit to avoid retaining every grammar touched during large
// indexing runs.
func NewExtractor() *Extractor {
	grammars.SetEmbeddedLanguageCacheLimit(32)
	return &Extractor{}
}

// skipDirs are directory names that should never be descended into
// during source walks. This covers common vendor, generated, and
// build artifact directories across ecosystems.
var skipDirs = map[string]bool{
	".git":          true,
	".hg":           true,
	".svn":          true,
	"node_modules":  true,
	"vendor":        true,
	"__pycache__":   true,
	".tox":          true,
	".eggs":         true,
	".mypy_cache":   true,
	".pytest_cache": true,
	"dist":          true,
	"build":         true,
	".next":         true,
	".nuxt":         true,
	"target":        true,
	".crush":        true,
	".claude":       true,
}

// ExtractFile extracts symbols from a single source file. If the
// language is unsupported, the grammar quality is insufficient, or
// parsing produces errors, it returns a result with Fallback=true.
func (e *Extractor) ExtractFile(path string) ExtractResult {
	result := ExtractResult{Path: path}

	src, err := os.ReadFile(path)
	if err != nil {
		result.Fallback = true
		result.Reason = fmt.Sprintf("read error: %v", err)
		return result
	}

	entry := grammars.DetectLanguage(filepath.Base(path))
	if entry == nil {
		result.Fallback = true
		result.Reason = "unsupported language"
		return result
	}

	// Quality is only populated by AuditParseSupport(); an empty value
	// means the grammar loaded without audit and should be treated as
	// usable. Only reject explicitly degraded grammars.
	if entry.Quality == grammars.ParseQualityPartial || entry.Quality == grammars.ParseQualityNone {
		result.Fallback = true
		result.Reason = fmt.Sprintf("grammar quality %q", entry.Quality)
		return result
	}

	tagsQuery := grammars.ResolveTagsQuery(*entry)
	if tagsQuery == "" {
		result.Fallback = true
		result.Reason = "no tags query available"
		return result
	}

	lang := entry.Language()
	if lang == nil {
		result.Fallback = true
		result.Reason = "failed to load grammar"
		return result
	}

	// Parse first to check for errors before tagging.
	parser := gotreesitter.NewParser(lang)
	tree, err := parser.Parse(src)
	if err != nil || tree == nil || tree.RootNode() == nil {
		result.Fallback = true
		result.Reason = "parse returned nil tree"
		return result
	}
	hasErr := tree.RootNode().HasError()
	tree.Release()

	if hasErr {
		result.Fallback = true
		result.Reason = "parse tree contains errors"
		return result
	}

	tagger, err := gotreesitter.NewTagger(lang, tagsQuery)
	if err != nil {
		result.Fallback = true
		result.Reason = fmt.Sprintf("tagger init: %v", err)
		return result
	}

	tags := tagger.Tag(src)
	for _, tag := range tags {
		if !isDefinition(tag.Kind) {
			continue
		}
		result.Symbols = append(result.Symbols, Symbol{
			Name:      tag.Name,
			Kind:      tag.Kind,
			StartLine: int(tag.NameRange.StartPoint.Row),
			EndLine:   int(tag.NameRange.EndPoint.Row),
		})
	}

	return result
}

// WalkAndExtract walks a directory tree, extracting symbols from every
// recognized source file. It skips vendor/build directories and hidden
// directories. Results are sent on the returned channel; the channel is
// closed when the walk completes or ctx is cancelled.
func (e *Extractor) WalkAndExtract(ctx context.Context, root string) <-chan ExtractResult {
	ch := make(chan ExtractResult, 64)
	go func() {
		defer close(ch)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}

			if ctx.Err() != nil {
				return ctx.Err()
			}

			name := d.Name()

			if d.IsDir() {
				if skipDirs[name] {
					return filepath.SkipDir
				}
				if strings.HasPrefix(name, ".") && name != "." {
					return filepath.SkipDir
				}
				return nil
			}

			entry := grammars.DetectLanguage(name)
			if entry == nil {
				return nil
			}

			select {
			case ch <- e.ExtractFile(path):
			case <-ctx.Done():
				return ctx.Err()
			}

			return nil
		})
		if err != nil {
			slog.Error("Symbol walk failed", "root", root, "error", err)
		}
	}()
	return ch
}

// isDefinition reports whether a tag kind represents a definition
// rather than a reference.
func isDefinition(kind string) bool {
	return strings.HasPrefix(kind, "definition.")
}
