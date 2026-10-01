package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/stretchr/testify/require"
)

func TestHyperEnricher(t *testing.T) {
	t.Parallel()

	feed := `{"id": "hyper", "models": [{"id": "glm-5.3", "name": "GLM-5.3", "context_window": 1000000, "can_reason": true}]}`

	t.Run("fills metadata from the live feed", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/provider", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(feed))
		}))
		defer srv.Close()

		cfg := Config{ID: "hyper", BaseURL: srv.URL, APIType: string(catwalk.TypeOpenAICompat)}
		models := []catwalk.Model{{ID: "glm-5.3", Name: "glm-5.3"}}

		e := &hyperEnricher{}
		result, err := e.EnrichModels(context.Background(), cfg, &mockResolver{}, models)
		require.NoError(t, err)
		require.Equal(t, "GLM-5.3", result[0].Name)
		require.Equal(t, int64(1000000), result[0].ContextWindow)
		require.True(t, result[0].CanReason)
	})

	t.Run("falls back to the bundled copy when the feed fails", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		bundled := hyper.Embedded().Models
		require.NotEmpty(t, bundled, "the bundled Hyper catalog must not be empty")

		cfg := Config{ID: "hyper", BaseURL: srv.URL, APIType: string(catwalk.TypeOpenAICompat)}
		models := []catwalk.Model{{ID: bundled[0].ID, Name: bundled[0].ID}}

		e := &hyperEnricher{}
		result, err := e.EnrichModels(context.Background(), cfg, &mockResolver{}, models)
		require.NoError(t, err)
		require.Equal(t, bundled[0].Name, result[0].Name)
		require.Equal(t, bundled[0].ContextWindow, result[0].ContextWindow)
	})

	t.Run("no-ops for other API types", func(t *testing.T) {
		t.Parallel()
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		cfg := Config{ID: "hyper", BaseURL: srv.URL + "/v1", APIType: string(catwalk.TypeAnthropic)}
		models := []catwalk.Model{{ID: "some-model", Name: "some-model"}}

		e := &hyperEnricher{}
		result, err := e.EnrichModels(context.Background(), cfg, &mockResolver{}, models)
		require.NoError(t, err)
		require.Equal(t, 0, calls)
		require.Equal(t, "some-model", result[0].Name)
	})
}
