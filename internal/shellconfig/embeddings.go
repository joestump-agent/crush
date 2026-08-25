package shellconfig

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// handleEmbeddings implements the `embeddings` builtin.
//
// Usage:
//
//	embeddings set [--base-url URL] [--api-key KEY] [--model NAME]
//	    [--dimension N]
//	embeddings clear
//
// "set" defines or updates the single embedding provider that backs the
// semantic_search tool; unspecified fields keep their current (or default)
// values. "clear" removes the configuration entirely, which unregisters the
// semantic search tools on the next config load.
func handleEmbeddings(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	b := configBuilderFromCtx(ctx)
	if b == nil {
		return nil
	}
	if len(args) < 2 {
		return usage(stderr, "usage: embeddings set [--base-url URL] [--api-key KEY] [--model NAME] [--dimension N] | embeddings clear")
	}

	switch args[1] {
	case "set":
		return embeddingsSet(b, args, stderr)
	case "clear", "remove", "rm":
		return embeddingsClear(b)
	default:
		return usage(stderr, fmt.Sprintf("embeddings: unknown subcommand %q (expected set or clear)", args[1]))
	}
}

// embeddingsSetFlags is the declarative flag surface for `embeddings set`.
var embeddingsSetFlags = []flagSpec{
	{name: "--base-url", jsonKey: "base_url", kind: flagString, op: opSet},
	{name: "--api-key", jsonKey: "api_key", kind: flagString, op: opSet},
	{name: "--model", jsonKey: "model", kind: flagString, op: opSet},
	{name: "--dimension", jsonKey: "dimension", kind: flagInt, op: opSet, validate: positiveDimension},
}

// positiveDimension rejects non-positive dimensions: a zero or negative
// dimension would silently default at load, hiding the typo from the user.
func positiveDimension(v any) error {
	if d, ok := v.(int64); ok && d <= 0 {
		return fmt.Errorf("dimension must be positive, got %d", d)
	}
	return nil
}

func embeddingsSet(b *ConfigBuilder, args []string, stderr io.Writer) error {
	section := b.section("embeddings")
	if err := applyFlags(embeddingsSetFlags, args, 2, section, "embeddings set", stderr); err != nil {
		return err
	}
	slog.Info("Embedding provider defined in shell config")
	return nil
}

func embeddingsClear(b *ConfigBuilder) error {
	delete(b.root, "embeddings")
	slog.Info("Embedding provider removed in shell config")
	return nil
}
