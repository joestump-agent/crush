package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
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

	p, ok := cfg.Providers.Get("custom")
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

	p, ok = cfg.Providers.Get("custom")
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

	p, ok := cfg.Providers.Get("custom")
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

	p, ok := cfg.Providers.Get("curated")
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

	_, ok := cfg.Providers.Get("flaky")
	require.False(t, ok, "provider with failed discovery and no models is dropped at load")
	require.Contains(t, store.failedDiscoveryProviders, "flaky")

	// Endpoint comes up; the reload should resurrect the provider.
	healthy.Store(true)

	added, err := store.ReloadModelDiscovery(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, added, "resurrected provider's models count as added")

	p, ok := cfg.Providers.Get("flaky")
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

		p, ok := cfg.Providers.Get("custom")
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

		p, ok := cfg.Providers.Get("custom")
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

		p, ok := cfg.Providers.Get("myprov")
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
