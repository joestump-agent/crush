package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestMergeCatalog(t *testing.T) {
	t.Parallel()

	catalog := []catwalk.Model{
		{
			ID:                     "glm-5.3",
			Name:                   "GLM-5.3",
			CostPer1MIn:            1,
			CostPer1MOut:           3,
			CostPer1MInCached:      0.5,
			CostPer1MOutCached:     0.25,
			ContextWindow:          1_000_000,
			DefaultMaxTokens:       128_000,
			CanReason:              true,
			ReasoningLevels:        []string{"low", "high", "max"},
			DefaultReasoningEffort: "high",
			SupportsImages:         true,
		},
		{ID: "catalog-only", Name: "Catalog Only"},
	}

	t.Run("fills bare discovered models", func(t *testing.T) {
		t.Parallel()
		models := []catwalk.Model{{ID: "glm-5.3", Name: "glm-5.3"}}

		got := MergeCatalog(models, catalog)
		require.Len(t, got, 1, "catalog-only models must not be added")
		require.Equal(t, catalog[0], got[0])
	})

	t.Run("fills an unnamed model", func(t *testing.T) {
		t.Parallel()
		got := MergeCatalog([]catwalk.Model{{ID: "glm-5.3"}}, catalog)
		require.Equal(t, "GLM-5.3", got[0].Name)
	})

	t.Run("keeps values the user set", func(t *testing.T) {
		t.Parallel()
		models := []catwalk.Model{{
			ID:                     "glm-5.3",
			Name:                   "My GLM",
			ContextWindow:          200_000,
			DefaultMaxTokens:       32_000,
			CostPer1MIn:            9,
			ReasoningLevels:        []string{"high"},
			DefaultReasoningEffort: "high",
		}}

		got := MergeCatalog(models, catalog)
		require.Equal(t, "My GLM", got[0].Name)
		require.Equal(t, int64(200_000), got[0].ContextWindow)
		require.Equal(t, int64(32_000), got[0].DefaultMaxTokens)
		require.InDelta(t, 9, got[0].CostPer1MIn, 0)
		require.Equal(t, []string{"high"}, got[0].ReasoningLevels)
		// Unset fields are still filled around the user's values.
		require.InDelta(t, 3, got[0].CostPer1MOut, 0)
		require.True(t, got[0].CanReason)
	})

	t.Run("leaves models the catalog does not know", func(t *testing.T) {
		t.Parallel()
		models := []catwalk.Model{{ID: "mystery", Name: "mystery"}}
		got := MergeCatalog(models, catalog)
		require.Equal(t, catwalk.Model{ID: "mystery", Name: "mystery"}, got[0])
	})

	t.Run("does not alias the catalog's reasoning levels", func(t *testing.T) {
		t.Parallel()
		cat := []catwalk.Model{{ID: "m", ReasoningLevels: []string{"low", "high"}}}
		got := MergeCatalog([]catwalk.Model{{ID: "m"}}, cat)
		got[0].ReasoningLevels[0] = "changed"
		require.Equal(t, "low", cat[0].ReasoningLevels[0])
	})

	t.Run("no catalog is a no-op", func(t *testing.T) {
		t.Parallel()
		models := []catwalk.Model{{ID: "glm-5.3", Name: "glm-5.3"}}
		require.Equal(t, models, MergeCatalog(models, nil))
	})
}

func TestFetchProviderFeed(t *testing.T) {
	t.Parallel()

	t.Run("reads the provider document under the base URL", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/provider", r.URL.Path)
			require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": "hyper",
				"models": [
					{"id": "glm-5.3", "name": "GLM 5.3", "context_window": 1000000,
					 "can_reason": true, "reasoning_levels": ["low", "high", "max"],
					 "default_reasoning_effort": "high"}
				]
			}`))
		}))
		defer srv.Close()

		cfg := Config{ID: "hyper", BaseURL: srv.URL + "/v1", APIKey: "test-key"}
		p, err := FetchProviderFeed(context.Background(), cfg, &mockResolver{})
		require.NoError(t, err)
		require.Len(t, p.Models, 1)
		require.Equal(t, "GLM 5.3", p.Models[0].Name)
		require.Equal(t, []string{"low", "high", "max"}, p.Models[0].ReasoningLevels)
	})

	t.Run("reports a non-200 response", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		_, err := FetchProviderFeed(context.Background(), Config{ID: "hyper", BaseURL: srv.URL}, &mockResolver{})
		require.ErrorContains(t, err, "404")
	})

	t.Run("reports an undecodable body", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		}))
		defer srv.Close()

		_, err := FetchProviderFeed(context.Background(), Config{ID: "hyper", BaseURL: srv.URL}, &mockResolver{})
		require.Error(t, err)
	})
}
