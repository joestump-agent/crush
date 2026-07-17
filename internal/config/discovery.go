package config

import (
	"cmp"
	"context"
	"log/slog"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/discover"
)

// modelDiscoveryTimeout bounds an interactive model-discovery reload. It is
// more generous than the load-time budget because the user explicitly asked
// for it and local model servers (Ollama, LM Studio, …) can be slow to answer.
const modelDiscoveryTimeout = 10 * time.Second

// providerWantsDiscovery reports whether the given custom provider should have
// its models discovered. When force is true (an explicit reload) discovery
// runs for every provider that has not opted out via discover_models: false.
// Otherwise it uses the load-time trigger: an explicit discover_models: true,
// or an empty models list that hasn't opted out.
func providerWantsDiscovery(pc ProviderConfig, force bool) bool {
	optedOut := pc.AutoDiscoverModels != nil && !*pc.AutoDiscoverModels
	if force {
		return !optedOut
	}
	wantsDiscovery := pc.AutoDiscoverModels != nil && *pc.AutoDiscoverModels
	autoTrigger := len(pc.Models) == 0 && !optedOut
	return wantsDiscovery || autoTrigger
}

// discoverProviderModels runs model discovery concurrently for the custom
// (non-known) providers in the map that need it, returning the freshly
// discovered and enriched model list keyed by provider ID. Providers whose
// discovery fails or yields no models are omitted from the result. Discovered
// models always include the provider's existing (user-specified) models, since
// those take precedence during discovery.
//
// The caller owns the context deadline; both load and reload set one so a
// slow or unreachable provider endpoint cannot block indefinitely.
func discoverProviderModels(
	ctx context.Context,
	providers *csync.Map[string, ProviderConfig],
	knownProviderNames map[string]bool,
	resolver VariableResolver,
	force bool,
) map[string][]catwalk.Model {
	results := make(map[string][]catwalk.Model)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for id, pc := range providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}
		if pc.Disable || pc.BaseURL == "" {
			continue
		}
		if !providerWantsDiscovery(pc, force) {
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
			models, err := discover.DiscoverModels(ctx, cfg, resolver)
			if err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", err)
				return
			}
			if len(models) == 0 {
				return
			}
			if enricher := discover.GetEnricher(string(providerType)); enricher != nil {
				models, _ = enricher.EnrichModels(ctx, cfg, resolver, models)
			}
			mu.Lock()
			results[id] = models
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}

// ReloadModelDiscovery re-runs model discovery for every custom provider that
// supports it and merges any newly found models into the in-memory config. It
// returns the number of models added across all providers.
//
// Discovered models are intentionally not persisted to disk: like the
// discovery that runs during load, they are recomputed from the live provider
// endpoints each time. This lets a running Crush pick up models that appeared
// after startup (e.g. an `ollama pull`) without a restart.
func (s *ConfigStore) ReloadModelDiscovery(ctx context.Context) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cfg := s.Config()

	knownProviderNames := make(map[string]bool, len(s.knownProviders))
	for _, p := range s.knownProviders {
		knownProviderNames[string(p.ID)] = true
	}

	discoverCtx, cancel := context.WithTimeout(ctx, modelDiscoveryTimeout)
	defer cancel()

	results := discoverProviderModels(discoverCtx, cfg.Providers, knownProviderNames, s.resolver, true)

	added := 0
	for id, models := range results {
		pc, ok := cfg.Providers.Get(id)
		if !ok {
			continue
		}
		if len(models) > len(pc.Models) {
			added += len(models) - len(pc.Models)
		}
		pc.Models = models
		cfg.Providers.Set(id, pc)
		slog.Info("Reloaded models for provider", "provider", id, "count", len(models))
	}
	return added, nil
}
