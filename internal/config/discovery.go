package config

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/discover"
)

// loadModelDiscoveryTimeout bounds each provider's model-discovery probe
// during config load. It is per provider, not a budget shared across the
// pass. Because the probes run concurrently, both designs start the same
// clock and behave identically today: a shared deadline drops every
// endpoint slower than the budget, not only the slowest one, so raising
// the budget is what recovers a slow remote gateway. The per-provider
// split is the structural guarantee for the day probing is staged or
// throttled, when a shared budget would start to starve later probes.
// Providers whose endpoints miss it are recorded on the store and can be
// retried later via ReloadModelDiscovery.
//
// It is sized for a remote gateway on a poor link (LiteLLM and Hyper have
// been measured at 10.5s and 6.7s respectively over hotel/boat wifi), not
// for a healthy LAN. Because the probes are concurrent, the worst case it
// adds to startup is one provider's timeout, not the sum.
const loadModelDiscoveryTimeout = 15 * time.Second

// modelDiscoveryTimeout bounds each provider's probe on an interactive
// model-discovery reload. Like loadModelDiscoveryTimeout it is per
// provider.
const modelDiscoveryTimeout = 15 * time.Second

// providerWantsDiscovery reports whether a custom provider consents to
// model discovery. userModels is the provider's user-configured model
// list: at load time that is pc.Models itself; on reload it is the list
// the store recorded before load-time discovery merged endpoint models
// into pc.Models.
//
// The consent model: a provider with a curated, non-empty models list is
// never discovered unless it explicitly sets discover_models: true, and
// an empty list implies consent unless discover_models: false opts out.
// An interactive reload re-probes eligible providers but never widens
// this consent.
func providerWantsDiscovery(pc ProviderConfig, userModels []catwalk.Model) bool {
	if pc.AutoDiscoverModels != nil {
		return *pc.AutoDiscoverModels
	}
	return len(userModels) == 0
}

// knownProviderNameSet returns the set of provider IDs treated as known
// (non-custom) for discovery purposes. When disableDefaults is set every
// provider is custom — matching configureProviders, which skips all
// default providers under that option — so the set is empty.
func knownProviderNameSet(known []catwalk.Provider, disableDefaults bool) map[string]bool {
	names := make(map[string]bool, len(known))
	if disableDefaults {
		return names
	}
	for _, p := range known {
		names[string(p.ID)] = true
	}
	return names
}

// discoverProviderModels runs model discovery concurrently for the custom
// (non-known) candidates that are eligible for it (see
// providerWantsDiscovery). Each candidate's Models field must hold only
// its user-configured models; those are passed to discovery as the
// existing set, so they always survive and take precedence over endpoint
// models. It returns the discovered model lists and the per-provider
// discovery errors, both keyed by provider ID. A provider that succeeds
// but reports no models gets a results entry with an empty list.
//
// perProviderTimeout bounds each provider's probe independently, so one
// slow endpoint cannot consume another's budget. The caller's ctx still
// cancels the whole pass.
func discoverProviderModels(
	ctx context.Context,
	candidates map[string]ProviderConfig,
	knownProviderNames map[string]bool,
	resolver VariableResolver,
	perProviderTimeout time.Duration,
) (map[string][]catwalk.Model, map[string]error) {
	results := make(map[string][]catwalk.Model)
	errs := make(map[string]error)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for id, pc := range candidates {
		if knownProviderNames[id] {
			continue
		}
		if pc.Disable || pc.BaseURL == "" {
			continue
		}
		if !providerWantsDiscovery(pc, pc.Models) {
			continue
		}

		providerID := cmp.Or(pc.ID, id)
		cfg := discover.Config{
			ID:             providerID,
			BaseURL:        pc.BaseURL,
			APIKey:         pc.APIKey,
			ExtraHeaders:   pc.ExtraHeaders,
			ExistingModels: pc.Models,
		}
		providerType := cmp.Or(pc.Type, catwalk.TypeOpenAICompat)
		wg.Go(func() {
			// Each provider gets its own deadline. Enrichment shares it
			// with the probe: together they are this provider's budget.
			pctx, pcancel := context.WithTimeout(ctx, perProviderTimeout)
			defer pcancel()

			models, err := discover.DiscoverModels(pctx, cfg, resolver)
			if err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", err)
				mu.Lock()
				errs[id] = err
				mu.Unlock()
				return
			}
			if len(models) > 0 {
				if enricher := discover.GetEnricher(string(providerType)); enricher != nil {
					models, _ = enricher.EnrichModels(pctx, cfg, resolver, models)
				}
			}
			mu.Lock()
			results[id] = models
			mu.Unlock()
		})
	}
	wg.Wait()
	return results, errs
}

// ReloadModelDiscovery re-runs model discovery for every custom provider
// eligible for it and merges the results into the in-memory config. It
// returns the number of models added across all providers.
//
// Eligibility matches load exactly (see providerWantsDiscovery): a reload
// never widens consent, it only re-probes providers load would have
// probed — including providers load dropped because discovery failed and
// they had no user-specified models (e.g. Crush started before Ollama),
// which are resurrected here when their endpoint comes back.
//
// Discovered models are intentionally not persisted to disk: like the
// discovery that runs during load, they are recomputed from the live
// provider endpoints each time. This lets a running Crush pick up models
// that appeared after startup (e.g. an `ollama pull`) without a restart.
//
// The network I/O runs without holding writeMu so config mutators (e.g.
// model selection) are never blocked behind slow provider endpoints. The
// merge goes through mutateInMemory and re-reads each provider fresh, so
// concurrent changes to other provider fields (e.g. an OAuth token
// refresh) are preserved.
//
// It returns an error when every eligible provider failed discovery so
// total failure is distinguishable from "no new models".
func (s *ConfigStore) ReloadModelDiscovery(ctx context.Context) (int, error) {
	cfg := s.Config()

	// Snapshot the discovery bookkeeping under a brief lock.
	s.writeMu.Lock()
	userModels := maps.Clone(s.userConfiguredModels)
	failed := maps.Clone(s.failedDiscoveryProviders)
	s.writeMu.Unlock()

	var disableDefaults bool
	if cfg.Options != nil {
		disableDefaults = cfg.Options.DisableDefaultProviders
	}
	known := knownProviderNameSet(s.knownProviders, disableDefaults)

	// Build the candidate set from the live providers plus the ones load
	// dropped after failed discovery. Models is seeded with the recorded
	// user-configured list (not the current, possibly discovery-merged
	// one) so re-discovery prunes endpoint-removed models while keeping
	// user-specified ones.
	candidates := make(map[string]ProviderConfig)
	for id, pc := range cfg.Providers.Seq2() {
		if um, ok := userModels[id]; ok {
			pc.Models = um
		}
		candidates[id] = pc
	}
	for id, pc := range failed {
		if _, ok := candidates[id]; !ok {
			candidates[id] = pc
		}
	}

	results, errs := discoverProviderModels(ctx, candidates, known, s.resolver, modelDiscoveryTimeout)
	if len(errs) > 0 && len(results) == 0 {
		return 0, fmt.Errorf("model discovery failed for all %d eligible providers", len(errs))
	}

	added := 0
	s.mutateInMemory(func(c *Config) {
		for id, models := range results {
			if len(models) == 0 {
				continue
			}
			// Re-read the provider fresh under writeMu and write only
			// the merged Models onto it, so concurrent changes to other
			// fields (OAuth tokens, header edits, …) are not clobbered.
			pc, ok := c.Providers.Get(id)
			if !ok {
				// Resurrect a provider dropped at load. writeMu is
				// held by mutateInMemory, so touching the failed map
				// here is safe.
				fp, wasFailed := s.failedDiscoveryProviders[id]
				if !wasFailed {
					continue
				}
				fp.Models = models
				c.Providers.Set(id, fp)
				delete(s.failedDiscoveryProviders, id)
				added += len(models)
				slog.Info("Recovered provider via model discovery",
					"provider", id, "count", len(models))
				continue
			}
			newCount := 0
			have := make(map[string]bool, len(pc.Models))
			for _, m := range pc.Models {
				have[m.ID] = true
			}
			for _, m := range models {
				if !have[m.ID] {
					newCount++
				}
			}
			pc.Models = models
			c.Providers.Set(id, pc)
			if newCount > 0 {
				added += newCount
				slog.Info("Reloaded models for provider",
					"provider", id, "added", newCount, "count", len(models))
			}
		}
	})
	return added, nil
}
