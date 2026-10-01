package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
)

// MergeCatalog fills in metadata on discovered models from the catalog
// entry with the same ID. A provider's /models endpoint usually returns
// bare IDs, so without this a model discovered from a provider Crush
// already has a catalog for (e.g. a custom "zai" or "hyper" provider under
// disable_default_providers) runs with no context window, no pricing and
// no reasoning settings, and is named by its raw ID.
//
// Existing values win: a field is filled only while it is still zero, and
// a name only while it is empty or the bare ID discovery assigned. As in
// the LM Studio enricher, a false bool is treated as unset. Models absent
// from the catalog are left untouched, and catalog models absent from the
// discovered list are not added: discovery decides which models exist,
// the catalog only describes them.
func MergeCatalog(models, catalog []catwalk.Model) []catwalk.Model {
	if len(catalog) == 0 {
		return models
	}
	byID := make(map[string]catwalk.Model, len(catalog))
	for _, m := range catalog {
		byID[m.ID] = m
	}

	for i := range models {
		c, ok := byID[models[i].ID]
		if !ok {
			continue
		}
		m := &models[i]
		if (m.Name == "" || m.Name == m.ID) && c.Name != "" {
			m.Name = c.Name
		}
		if m.CostPer1MIn == 0 {
			m.CostPer1MIn = c.CostPer1MIn
		}
		if m.CostPer1MOut == 0 {
			m.CostPer1MOut = c.CostPer1MOut
		}
		if m.CostPer1MInCached == 0 {
			m.CostPer1MInCached = c.CostPer1MInCached
		}
		if m.CostPer1MOutCached == 0 {
			m.CostPer1MOutCached = c.CostPer1MOutCached
		}
		if m.ContextWindow == 0 {
			m.ContextWindow = c.ContextWindow
		}
		if m.DefaultMaxTokens == 0 {
			m.DefaultMaxTokens = c.DefaultMaxTokens
		}
		if !m.CanReason {
			m.CanReason = c.CanReason
		}
		if len(m.ReasoningLevels) == 0 {
			m.ReasoningLevels = slices.Clone(c.ReasoningLevels)
		}
		if m.DefaultReasoningEffort == "" {
			m.DefaultReasoningEffort = c.DefaultReasoningEffort
		}
		if !m.SupportsImages {
			m.SupportsImages = c.SupportsImages
		}
	}
	return models
}

// FetchProviderFeed reads the catwalk-format provider document served at
// <base>/provider. Hyper publishes its model catalog there (the same feed
// its built-in provider is generated from), so a custom provider pointed
// at Hyper can describe its discovered models from the live list rather
// than a copy bundled at build time.
func FetchProviderFeed(ctx context.Context, cfg Config, resolver Resolver) (catwalk.Provider, error) {
	var provider catwalk.Provider
	resp, err := doRequest(ctx, http.MethodGet, cfg.BaseURL, "/provider", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return provider, fmt.Errorf("fetch provider feed for %s: %w", cfg.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return provider, fmt.Errorf("fetch provider feed for %s: %s", cfg.ID, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&provider); err != nil {
		return provider, fmt.Errorf("fetch provider feed for %s: %w", cfg.ID, err)
	}
	return provider, nil
}
