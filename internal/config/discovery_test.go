package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestProviderWantsDiscovery(t *testing.T) {
	t.Parallel()

	discoverTrue := true
	discoverFalse := false

	tests := []struct {
		name  string
		pc    ProviderConfig
		force bool
		want  bool
	}{
		{
			name:  "load: empty models auto-triggers",
			pc:    ProviderConfig{},
			force: false,
			want:  true,
		},
		{
			name:  "load: non-empty models does not trigger",
			pc:    ProviderConfig{Models: []catwalk.Model{{ID: "a"}}},
			force: false,
			want:  false,
		},
		{
			name:  "load: explicit discover_models:true triggers even with models",
			pc:    ProviderConfig{Models: []catwalk.Model{{ID: "a"}}, AutoDiscoverModels: &discoverTrue},
			force: false,
			want:  true,
		},
		{
			name:  "load: discover_models:false never triggers",
			pc:    ProviderConfig{AutoDiscoverModels: &discoverFalse},
			force: false,
			want:  false,
		},
		{
			name:  "reload: non-empty models still triggers",
			pc:    ProviderConfig{Models: []catwalk.Model{{ID: "a"}}},
			force: true,
			want:  true,
		},
		{
			name:  "reload: discover_models:false is honored",
			pc:    ProviderConfig{Models: []catwalk.Model{{ID: "a"}}, AutoDiscoverModels: &discoverFalse},
			force: true,
			want:  false,
		},
		{
			name:  "reload: discover_models:true triggers",
			pc:    ProviderConfig{AutoDiscoverModels: &discoverTrue},
			force: true,
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, providerWantsDiscovery(tt.pc, tt.force))
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

	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {
				ID:      "custom",
				APIKey:  "test-key",
				BaseURL: server.URL + "/v1",
				Models: []catwalk.Model{
					{ID: "existing-model", Name: "Existing"},
				},
			},
		}),
	}
	cfg.setDefaults(t.TempDir(), "")

	store := testStore(cfg)
	store.resolver = NewShellVariableResolver(env.NewFromMap(map[string]string{}))

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
