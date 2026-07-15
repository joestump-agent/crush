package mcp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func TestTokenStore_SaveLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &tokenStore{
		path: filepath.Join(dir, "tokens.json"),
		data: make(map[string]mcptoken),
	}

	tok := mcptoken{
		Token: &oauth2.Token{
			AccessToken:  "abc123",
			RefreshToken: "refresh456",
			TokenType:    "Bearer",
		},
		ClientID: "test-client",
	}
	tok.Endpoints.AuthURL = "https://auth.example.com/authorize"
	tok.Endpoints.TokenURL = "https://auth.example.com/token"

	store.save("https://mcp.example.com/mcp", tok)

	// Reload from disk into a fresh store.
	store2 := &tokenStore{
		path: store.path,
		data: make(map[string]mcptoken),
	}
	store2.load()

	got, ok := store2.get("https://mcp.example.com/mcp")
	require.True(t, ok)
	require.Equal(t, "abc123", got.Token.AccessToken)
	require.Equal(t, "refresh456", got.Token.RefreshToken)
	require.Equal(t, "test-client", got.ClientID)
	require.Equal(t, "https://auth.example.com/authorize", got.Endpoints.AuthURL)
	require.Equal(t, "https://auth.example.com/token", got.Endpoints.TokenURL)
}

func TestTokenStore_FilePermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions not supported on Windows")
	}
	dir := t.TempDir()
	store := &tokenStore{
		path: filepath.Join(dir, "tokens.json"),
		data: make(map[string]mcptoken),
	}

	store.save("https://server.example.com", mcptoken{
		Token: &oauth2.Token{AccessToken: "secret"},
	})

	info, err := os.Stat(store.path)
	require.NoError(t, err)
	// Token files should only be readable by the owner.
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestHasAuthHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"nil headers", nil, false},
		{"empty headers", map[string]string{}, false},
		{"non-auth headers only", map[string]string{"Content-Type": "application/json"}, false},
		{"Authorization header", map[string]string{"Authorization": "Bearer token"}, true},
		{"lowercase authorization", map[string]string{"authorization": "Bearer token"}, true},
		{"AUTHORIZATION uppercase", map[string]string{"AUTHORIZATION": "Bearer token"}, true},
		{"mixed with auth", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer token"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, hasAuthHeader(tt.headers))
		})
	}
}

func TestMCPOAuthHandler_TokenSourceEmptyByDefault(t *testing.T) {
	// A handler for a server with no saved tokens should return a nil
	// token source, which tells the transport to send requests without
	// an Authorization header (triggering the 401 → Authorize flow).
	t.Setenv("CRUSH_GLOBAL_CONFIG", t.TempDir())
	h := newMCPOAuthHandler("https://mcp.example.com/mcp")
	ts, err := h.TokenSource(t.Context())
	require.NoError(t, err)
	require.Nil(t, ts)
}
