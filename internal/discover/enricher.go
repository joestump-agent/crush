package discover

import (
	"context"
	"sort"

	"charm.land/catwalk/pkg/catwalk"
)

// Enricher fills in model metadata (context window, max tokens, pricing,
// etc.) for discovered models. Providers that expose richer endpoints
// beyond /v1/models can register an Enricher to populate fields that the
// standard listing endpoint omits.
type Enricher interface {
	// EnrichModels takes a slice of bare discovered models and returns
	// them with metadata populated. Implementations should preserve
	// existing non-zero fields (user overrides take precedence).
	EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error)
}

// typeEnrichers maps provider type strings to their enrichment
// implementations. Each enricher self-registers via init() so that
// adding a new provider requires only a new file, with no changes to
// load.go or any existing enricher.
var typeEnrichers = map[string]Enricher{}

// idEnrichers maps provider ID strings to enrichers specific to that
// one provider, for metadata sources that describe a vendor's lineup
// rather than a wire protocol (e.g. Hyper's live provider feed). IDs
// are looked up before types, and never appear as accepted provider
// types in the schema or type validation.
var idEnrichers = map[string]Enricher{}

// knownCustomTypes is the set of provider type strings that Crush treats
// as known custom provider types: the local gateways that speak
// OpenAI-compat under the hood and get the corresponding wire handling
// in the coordinator. Kept separate from typeEnrichers so that a future
// enricher on a standard type (openai, openai-compat) cannot silently
// change how requests are built.
var knownCustomTypes = map[string]bool{}

// RegisterEnricher registers an Enricher for the given provider type
// and marks that type as a known custom provider type. Called from
// init() in each enricher implementation file.
func RegisterEnricher(providerType string, e Enricher) {
	typeEnrichers[providerType] = e
	knownCustomTypes[providerType] = true
}

// RegisterProviderEnricher registers an Enricher for a single provider
// ID. It takes precedence over any type-keyed enricher when both match.
//
// The ID is also marked as a known custom provider type. Crush accepts
// some of these IDs as a `type` value in their own right (hyper is the
// worked example: load.go's type validation, the schema enum, and the
// coordinator's wire handling all accept `type: hyper`), and the wire
// handling behind that acceptance is what marks a provider OpenAI-compat
// under the hood. Registering the ID without the mark would leave a
// `type: hyper` provider with no OpenAI-compat request construction.
func RegisterProviderEnricher(providerID string, e Enricher) {
	idEnrichers[providerID] = e
	knownCustomTypes[providerID] = true
}

// GetEnricher returns the Enricher for the given provider, looking up
// the provider ID first and the provider type second, or nil if no
// enricher is registered for either.
func GetEnricher(providerID, providerType string) Enricher {
	if e, ok := idEnrichers[providerID]; ok {
		return e
	}
	return typeEnrichers[providerType]
}

// IsKnownCustomProvider reports whether the given provider type is one
// of the recognized local gateway types that speak OpenAI-compat
// protocol, without exposing the enrichers themselves to callers that
// only need the compatibility check. ID-keyed enrichers count only for
// the IDs Crush already accepts as a `type` value (hyper); an ID that is
// not also a type never gains the wire handling by being registered.
func IsKnownCustomProvider(providerType string) bool {
	return knownCustomTypes[providerType]
}

// RegisteredProviderTypes returns the known custom provider type
// strings, sorted for stable output. These are the custom,
// locally-discovered providers (e.g. ollama, omlx) that Crush accepts as
// a `type` value even though they are not catwalk provider types. The
// schema generator uses this so the published enum stays in sync with
// the registry instead of drifting from a hand-maintained list.
func RegisteredProviderTypes() []string {
	types := make([]string, 0, len(knownCustomTypes))
	for providerType := range knownCustomTypes {
		types = append(types, providerType)
	}
	sort.Strings(types)
	return types
}
