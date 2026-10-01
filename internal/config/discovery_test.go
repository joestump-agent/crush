package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestProviderWantsDiscovery(t *testing.T) {
	t.Parallel()

	discoverTrue := true
	discoverFalse := false

	tests := []struct {
		name       string
		pc         ProviderConfig
		userModels []catwalk.Model
		want       bool
	}{
		{
			name: "empty user models auto-triggers",
			pc:   ProviderConfig{},
			want: true,
		},
		{
			name:       "curated user models do not trigger",
			pc:         ProviderConfig{},
			userModels: []catwalk.Model{{ID: "a"}},
			want:       false,
		},
		{
			name:       "explicit discover_models:true triggers even with models",
			pc:         ProviderConfig{AutoDiscoverModels: &discoverTrue},
			userModels: []catwalk.Model{{ID: "a"}},
			want:       true,
		},
		{
			name: "discover_models:false never triggers",
			pc:   ProviderConfig{AutoDiscoverModels: &discoverFalse},
			want: false,
		},
		{
			name: "reload: discovery-merged models do not widen consent",
			// pc.Models holds load-discovered models, but the recorded
			// user-configured list is empty, so it stays eligible.
			pc:         ProviderConfig{Models: []catwalk.Model{{ID: "found"}}},
			userModels: nil,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, providerWantsDiscovery(tt.pc, tt.userModels))
		})
	}
}

func TestReloadModelDiscovery(t *testing.T) {
	t.Parallel()

	// modelsBody is swapped out to simulate a provider that gains a model
	// between the first and second discovery pass (e.g. an `ollama pull`).
	var modelsBody atomic.Value
	modelsBody.Store(`{"data": [{"id": "existing-model", "object": "model"}]}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsBody.Load().(string)))
	}))
	defer server.Close()

	discoverTrue := true
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				ID:      "custom",
				APIKey:  "test-key",
				BaseURL: server.URL + "/v1",
				Models: []catwalk.Model{
					{ID: "existing-model", Name: "Existing"},
				},
				AutoDiscoverModels: &discoverTrue,
			},
		}),
	}
	cfg.setDefaults(t.TempDir(), "")

	store := testStore(cfg)
	store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))
	store.userConfiguredModels = map[string][]catwalk.Model{
		"custom": {{ID: "existing-model", Name: "Existing"}},
	}

	// First reload: the server only reports the model we already have, so
	// nothing new is discovered.
	added, err := store.ReloadModelDiscovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, added)

	p, ok := store.Config().Providers.Get("custom")
	require.True(t, ok)
	require.Len(t, p.Models, 1)

	// A new model appears on the provider; a reload should pick it up.
	modelsBody.Store(`{"data": [
		{"id": "existing-model", "object": "model"},
		{"id": "fresh-model", "object": "model"}
	]}`)

	added, err = store.ReloadModelDiscovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, added)

	p, ok = store.Config().Providers.Get("custom")
	require.True(t, ok)
	require.Len(t, p.Models, 2)
	require.Equal(t, "existing-model", p.Models[0].ID)
	require.Equal(t, "Existing", p.Models[0].Name, "user-specified model keeps its name")
	require.Equal(t, "fresh-model", p.Models[1].ID)
}

func TestReloadModelDiscovery_RespectsOptOut(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "should-not-appear", "object": "model"}]}`))
	}))
	defer server.Close()

	discoverFalse := false
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				ID:                 "custom",
				APIKey:             "test-key",
				BaseURL:            server.URL + "/v1",
				Models:             []catwalk.Model{{ID: "listed-model"}},
				AutoDiscoverModels: &discoverFalse,
			},
		}),
	}
	cfg.setDefaults(t.TempDir(), "")

	store := testStore(cfg)
	store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))

	added, err := store.ReloadModelDiscovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, added)
	require.False(t, called.Load(), "provider opted out of discovery should not be queried")

	p, ok := store.Config().Providers.Get("custom")
	require.True(t, ok)
	require.Len(t, p.Models, 1)
	require.Equal(t, "listed-model", p.Models[0].ID)
}

func TestReloadModelDiscovery_SkipsCuratedProviders(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "should-not-appear", "object": "model"}]}`))
	}))
	defer server.Close()

	// A curated, non-empty models list without discover_models: true is
	// never discovered at load; reload must respect the same consent.
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"curated": {
				ID:      "curated",
				APIKey:  "test-key",
				BaseURL: server.URL + "/v1",
				Models:  []catwalk.Model{{ID: "hand-picked"}},
			},
		}),
	}
	cfg.setDefaults(t.TempDir(), "")

	store := testStore(cfg)
	store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))
	store.userConfiguredModels = map[string][]catwalk.Model{
		"curated": {{ID: "hand-picked"}},
	}

	added, err := store.ReloadModelDiscovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, added)
	require.False(t, called.Load(), "curated provider must not be re-discovered on reload")

	p, ok := store.Config().Providers.Get("curated")
	require.True(t, ok)
	require.Len(t, p.Models, 1)
	require.Equal(t, "hand-picked", p.Models[0].ID)
}

func TestReloadModelDiscovery_ResurrectsFailedProvider(t *testing.T) {
	t.Parallel()

	// The server is "down" (500) while configureProviders runs, then comes
	// up before the reload — the feature's headline case (Crush started
	// before Ollama).
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "not ready", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [
			{"id": "model-a", "object": "model"},
			{"id": "model-b", "object": "model"}
		]}`))
	}))
	defer server.Close()

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"flaky": {
				APIKey:  "test-key",
				BaseURL: server.URL + "/v1",
			},
		}),
	}
	cfg.setDefaults(t.TempDir(), "")

	store := testStore(cfg)
	testEnv := env.NewFromMap(map[string]string{})
	resolver := NewShellVariableResolver(testEnv)
	store.resolver = resolver

	require.NoError(t, cfg.configureProviders(context.Background(), store, testEnv, resolver, nil))

	_, ok := store.Config().Providers.Get("flaky")
	require.False(t, ok, "provider with failed discovery and no models is dropped at load")
	require.Contains(t, store.failedDiscoveryProviders, "flaky")

	// Endpoint comes up; the reload should resurrect the provider.
	healthy.Store(true)

	added, err := store.ReloadModelDiscovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, added, "resurrected provider's models count as added")

	p, ok := store.Config().Providers.Get("flaky")
	require.True(t, ok, "provider is resurrected once its endpoint answers")
	require.Len(t, p.Models, 2)
	require.NotContains(t, store.failedDiscoveryProviders, "flaky")
}

func TestReloadModelDiscovery_PrunesRemovedModels(t *testing.T) {
	t.Parallel()

	t.Run("auto-discovered provider", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [
				{"id": "fresh-a", "object": "model"},
				{"id": "fresh-b", "object": "model"}
			]}`))
		}))
		defer server.Close()

		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					ID:      "custom",
					APIKey:  "test-key",
					BaseURL: server.URL + "/v1",
					// Discovered at load; gone from the endpoint now.
					Models: []catwalk.Model{{ID: "stale-model"}},
				},
			}),
		}
		cfg.setDefaults(t.TempDir(), "")

		store := testStore(cfg)
		store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))
		store.userConfiguredModels = map[string][]catwalk.Model{"custom": nil}

		added, err := store.ReloadModelDiscovery(context.Background())
		require.NoError(t, err)
		require.Equal(t, 2, added, "added is the set difference, not a length delta")

		p, ok := store.Config().Providers.Get("custom")
		require.True(t, ok)
		require.Len(t, p.Models, 2)
		require.Equal(t, "fresh-a", p.Models[0].ID)
		require.Equal(t, "fresh-b", p.Models[1].ID)
	})

	t.Run("user models survive alongside pruning", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [{"id": "new-model", "object": "model"}]}`))
		}))
		defer server.Close()

		discoverTrue := true
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"custom": {
					ID:      "custom",
					APIKey:  "test-key",
					BaseURL: server.URL + "/v1",
					Models: []catwalk.Model{
						{ID: "user-model", Name: "User"},
						{ID: "stale-model"},
					},
					AutoDiscoverModels: &discoverTrue,
				},
			}),
		}
		cfg.setDefaults(t.TempDir(), "")

		store := testStore(cfg)
		store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))
		store.userConfiguredModels = map[string][]catwalk.Model{
			"custom": {{ID: "user-model", Name: "User"}},
		}

		// Simultaneous add (new-model) + remove (stale-model): added must
		// count only genuinely new IDs.
		added, err := store.ReloadModelDiscovery(context.Background())
		require.NoError(t, err)
		require.Equal(t, 1, added)

		p, ok := store.Config().Providers.Get("custom")
		require.True(t, ok)
		require.Len(t, p.Models, 2)
		require.Equal(t, "user-model", p.Models[0].ID)
		require.Equal(t, "User", p.Models[0].Name, "user-specified model is preserved")
		require.Equal(t, "new-model", p.Models[1].ID)
	})
}

func TestReloadModelDiscovery_AllProvidersFail(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				ID:      "custom",
				APIKey:  "test-key",
				BaseURL: server.URL + "/v1",
			},
		}),
	}
	cfg.setDefaults(t.TempDir(), "")

	store := testStore(cfg)
	store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))

	added, err := store.ReloadModelDiscovery(context.Background())
	require.Error(t, err, "total discovery failure must not look like 'no new models'")
	require.Contains(t, err.Error(), "all 1")
	require.Equal(t, 0, added)
}

func TestReloadModelDiscovery_DisableDefaultProviders(t *testing.T) {
	t.Parallel()

	newServer := func(called *atomic.Bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [{"id": "found-model", "object": "model"}]}`))
		}))
	}

	newConfig := func(baseURL string) *Config {
		cfg := &Config{
			Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"myprov": {
					ID:      "myprov",
					APIKey:  "test-key",
					BaseURL: baseURL + "/v1",
				},
			}),
		}
		cfg.setDefaults(t.TempDir(), "")
		return cfg
	}

	knownProviders := []catwalk.Provider{{ID: "myprov"}}

	t.Run("known-ID provider skipped by default", func(t *testing.T) {
		t.Parallel()

		var called atomic.Bool
		server := newServer(&called)
		defer server.Close()

		cfg := newConfig(server.URL)
		store := testStore(cfg)
		store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))
		store.knownProviders = knownProviders

		added, err := store.ReloadModelDiscovery(context.Background())
		require.NoError(t, err)
		require.Equal(t, 0, added)
		require.False(t, called.Load(), "known providers are not custom-discovered")
	})

	t.Run("known-ID provider reloaded when defaults disabled", func(t *testing.T) {
		t.Parallel()

		var called atomic.Bool
		server := newServer(&called)
		defer server.Close()

		cfg := newConfig(server.URL)
		cfg.Options.DisableDefaultProviders = true
		store := testStore(cfg)
		store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))
		store.knownProviders = knownProviders

		added, err := store.ReloadModelDiscovery(context.Background())
		require.NoError(t, err)
		require.Equal(t, 1, added)
		require.True(t, called.Load(), "with disable_default_providers every provider is custom")

		p, ok := store.Config().Providers.Get("myprov")
		require.True(t, ok)
		require.Len(t, p.Models, 1)
		require.Equal(t, "found-model", p.Models[0].ID)
	})
}

// TestDiscoverProviderModels_SlowProviderDoesNotDropPeers pins two things:
// the timeout argument is honoured per provider, and a provider that blows
// it is reported as an error rather than silently vanishing while its peers
// still resolve.
//
// It deliberately does NOT claim to prove the deadline is per provider
// rather than shared. While every probe runs concurrently those two designs
// are behaviourally identical, and this test passes under both — verified by
// running it against a shared-budget implementation. The per-provider split
// is a structural guarantee for the day probing is staged or throttled; the
// behavioural fix for the dropped-provider bug is the budget itself, which
// TestModelDiscoveryTimeouts_CoverRemoteGateways guards.
func TestDiscoverProviderModels_SlowProviderDoesNotDropPeers(t *testing.T) {
	t.Parallel()

	const perProvider = 150 * time.Millisecond

	quick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "quick-model", "object": "model"}]}`))
	}))
	defer quick.Close()

	// Comfortably past its own deadline, so it fails on its own terms.
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(perProvider * 4)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": [{"id": "stalled-model", "object": "model"}]}`))
	}))
	defer stalled.Close()

	discoverTrue := true
	candidates := map[string]ProviderConfig{
		"quick": {
			ID:                 "quick",
			APIKey:             "test-key",
			BaseURL:            quick.URL + "/v1",
			AutoDiscoverModels: &discoverTrue,
		},
		"stalled": {
			ID:                 "stalled",
			APIKey:             "test-key",
			BaseURL:            stalled.URL + "/v1",
			AutoDiscoverModels: &discoverTrue,
		},
	}

	resolver := NewShellVariableResolver(env.NewFromMap(map[string]string{}))
	results, errs := discoverProviderModels(
		context.Background(),
		candidates,
		map[string]bool{},
		resolver,
		perProvider,
	)

	require.Contains(t, results, "quick", "a responsive provider must survive a peer blowing its deadline")
	require.Len(t, results["quick"], 1)
	require.Equal(t, "quick-model", results["quick"][0].ID)

	require.Contains(t, errs, "stalled", "a provider past its own deadline must report an error, not vanish")
	require.NotContains(t, results, "stalled")
}

// TestModelDiscoveryTimeouts_CoverRemoteGateways guards the budget itself.
// The load-time deadline was 3s, which is under the round-trip of a remote
// gateway on a poor link — LiteLLM and Hyper were measured at 10.5s and
// 6.7s — so whichever was slowest that run was dropped from the model list
// without an error the user could see. Keep enough headroom that a working
// remote provider is not mistaken for a broken one.
func TestModelDiscoveryTimeouts_CoverRemoteGateways(t *testing.T) {
	t.Parallel()

	require.GreaterOrEqual(t, loadModelDiscoveryTimeout, 10*time.Second,
		"load-time discovery must tolerate a slow remote gateway")
	require.GreaterOrEqual(t, modelDiscoveryTimeout, loadModelDiscoveryTimeout,
		"an explicitly requested reload must be at least as patient as startup")

	// The discover package's HTTP client carries its own timeout, and the
	// shorter of the two wins. At 10s it silently capped the 15s budget,
	// so a gateway answering in 10.5s was still dropped.
	require.Greater(t, discover.RequestTimeout, modelDiscoveryTimeout,
		"the HTTP client backstop must not undercut the discovery budget")
}

// TestDiscoverProviderModels_FillsCatalogMetadata pins the catalog lookup
// that describes discovered models. /models returns bare IDs, so a custom
// provider named after a catalog provider (a "hyper" or "zai" entry under
// disable_default_providers) ran every model with no context window and no
// reasoning settings. On Hyper that sent glm-5.3 out with thinking off and
// no reasoning_effort, which Hyper rejects as invalid input.
func TestDiscoverProviderModels_FillsCatalogMetadata(t *testing.T) {
	t.Parallel()

	resolver := NewShellVariableResolver(env.NewFromMap(map[string]string{}))

	// newServer lists ids at /v1/models and serves feed at /v1/provider,
	// or a 404 there when feed is empty.
	newServer := func(t *testing.T, ids []string, feed string) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/models":
				var data []string
				for _, id := range ids {
					data = append(data, fmt.Sprintf(`{"id": %q, "object": "model"}`, id))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data": [%s]}`, strings.Join(data, ","))
			case "/v1/provider":
				if feed == "" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(feed))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	discoverOne := func(t *testing.T, id string, typ catwalk.Type, srv *httptest.Server) []catwalk.Model {
		t.Helper()
		discoverTrue := true
		results, errs := discoverProviderModels(
			context.Background(),
			map[string]ProviderConfig{id: {
				ID:                 id,
				Type:               typ,
				APIKey:             "test-key",
				BaseURL:            srv.URL + "/v1",
				AutoDiscoverModels: &discoverTrue,
			}},
			map[string]bool{},
			resolver,
			5*time.Second,
		)
		require.Empty(t, errs)
		return results[id]
	}

	t.Run("hyper reads the live feed", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, []string{"live-model"}, `{
			"id": "hyper",
			"models": [{
				"id": "live-model", "name": "Live Model", "context_window": 123456,
				"default_max_tokens": 4096, "can_reason": true,
				"reasoning_levels": ["low", "high", "max"], "default_reasoning_effort": "high"
			}]
		}`)

		models := discoverOne(t, "hyper", catwalk.TypeOpenAICompat, srv)
		require.Len(t, models, 1)
		require.Equal(t, "Live Model", models[0].Name)
		require.Equal(t, int64(123456), models[0].ContextWindow)
		require.Equal(t, int64(4096), models[0].DefaultMaxTokens)
		require.True(t, models[0].CanReason)
		require.Equal(t, []string{"low", "high", "max"}, models[0].ReasoningLevels)
		require.Equal(t, "high", models[0].DefaultReasoningEffort)
	})

	t.Run("hyper falls back to the bundled copy", func(t *testing.T) {
		t.Parallel()
		bundled := hyper.Embedded().Models
		require.NotEmpty(t, bundled)
		want := bundled[0]
		srv := newServer(t, []string{want.ID}, "")

		models := discoverOne(t, "hyper", catwalk.TypeOpenAICompat, srv)
		require.Len(t, models, 1)
		require.Equal(t, want.Name, models[0].Name)
		require.Equal(t, want.ContextWindow, models[0].ContextWindow)
	})

	t.Run("hyper under another API type is not described", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, []string{"live-model"}, `{"id": "hyper", "models": [{"id": "live-model", "name": "Live Model"}]}`)

		models := discoverOne(t, "hyper", catwalk.TypeAnthropic, srv)
		require.Len(t, models, 1)
		require.Equal(t, "live-model", models[0].Name)
	})

	t.Run("zai is described by the embedded catwalk catalog", func(t *testing.T) {
		t.Parallel()
		zai, ok := embeddedCatalog()["zai"]
		require.True(t, ok)
		require.NotEmpty(t, zai.Models)
		want := zai.Models[0]
		srv := newServer(t, []string{want.ID}, "")

		models := discoverOne(t, "zai", catwalk.TypeOpenAICompat, srv)
		require.Len(t, models, 1)
		require.Equal(t, want.Name, models[0].Name)
		require.Equal(t, want.ContextWindow, models[0].ContextWindow)
		require.Equal(t, want.CanReason, models[0].CanReason)
		require.Equal(t, want.ReasoningLevels, models[0].ReasoningLevels)
	})

	t.Run("a catalog for another API type is not used", func(t *testing.T) {
		t.Parallel()
		gemini, ok := embeddedCatalog()["gemini"]
		require.True(t, ok)
		require.Equal(t, catwalk.TypeGoogle, gemini.Type)
		require.NotEmpty(t, gemini.Models)
		id := gemini.Models[0].ID
		srv := newServer(t, []string{id}, "")

		models := discoverOne(t, "gemini", catwalk.TypeOpenAICompat, srv)
		require.Len(t, models, 1)
		require.Equal(t, id, models[0].Name)
		require.Zero(t, models[0].ContextWindow)
	})

	t.Run("a provider with no catalog is untouched", func(t *testing.T) {
		t.Parallel()
		// glm-5.3 is in the zai catalog: the provider ID, not the model
		// ID, decides whether a catalog applies.
		srv := newServer(t, []string{"glm-5.3"}, "")

		models := discoverOne(t, "myprov", catwalk.TypeOpenAICompat, srv)
		require.Len(t, models, 1)
		require.Equal(t, "glm-5.3", models[0].Name)
		require.Zero(t, models[0].ContextWindow)
		require.False(t, models[0].CanReason)
	})
}

// TestCloneForWrite_IsolatesProviderModels pins the clone's Providers
// isolation, which the model-discovery merge depends on.
//
// cloneForWrite used to hand back the same *csync.Map, and csync.Map.Get
// returns a struct whose Models slice aliases the map's own backing array.
// ReloadModelDiscovery reads a provider out of the clone and fills model
// metadata in place, so the write landed in the LIVE, published map while
// readers were reading it: a data race, plus torn metadata in the running
// config. Copying Providers in the clone is what keeps the merge private.
func TestCloneForWrite_IsolatesProviderModels(t *testing.T) {
	t.Parallel()

	live := csync.NewMapFrom(map[string]ProviderConfig{
		"custom": {
			ID:     "custom",
			Models: []catwalk.Model{{ID: "glm-5.3", Name: "glm-5.3"}},
		},
	})
	cfg := &Config{Providers: live}
	cfg.setDefaults(t.TempDir(), "")

	clone := cfg.cloneForWrite()
	require.NotSame(t, cfg.Providers, clone.Providers,
		"the clone must not share the live provider map")

	// Mutate through the clone the way the discovery merge does.
	pc, ok := clone.Providers.Get("custom")
	require.True(t, ok)
	pc.Models[0].ContextWindow = 1_000_000
	pc.Models = append(pc.Models, catwalk.Model{ID: "added"})
	clone.Providers.Set("custom", pc)

	published, ok := cfg.Providers.Get("custom")
	require.True(t, ok)
	require.Len(t, published.Models, 1, "the live map must not gain the clone's models")
	require.Zero(t, published.Models[0].ContextWindow,
		"the live map's model must not be written through the clone")
}
