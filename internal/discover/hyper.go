package discover

import (
	"context"
	"log/slog"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/hyper"
)

func init() {
	RegisterProviderEnricher(hyper.Name, &hyperEnricher{})
}

// hyperEnricher describes a custom provider pointed at Hyper from
// Hyper's live provider feed, the same feed the built-in Hyper
// provider is generated from, read inside the discovery probe's
// timeout. If the feed cannot be read, the copy bundled with Crush is
// used, and the embedded catwalk catalog that MergeCatalog consults
// later fills anything both leave unset.
//
// It is registered under the provider ID, not a type, because it
// describes the vendor's model lineup rather than a wire protocol: a
// custom Hyper provider is an ordinary openai-compat provider whose ID
// happens to be hyper.
type hyperEnricher struct{}

func (e *hyperEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error) {
	if cfg.APIType != string(catwalk.TypeOpenAICompat) && cfg.APIType != hyper.Name {
		return models, nil
	}

	feed, err := FetchProviderFeed(ctx, cfg, resolver)
	if err != nil {
		slog.Warn("Could not read Hyper's model feed, using the bundled copy", "provider", cfg.ID, "error", err)
		return MergeCatalog(models, hyper.Embedded().Models), nil
	}
	return MergeCatalog(models, feed.Models), nil
}
