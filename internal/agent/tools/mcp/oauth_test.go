package mcp

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/auth"
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

func TestTokenStore_GetUnknownServer(t *testing.T) {
	t.Parallel()
	store := &tokenStore{
		path: filepath.Join(t.TempDir(), "tokens.json"),
		data: make(map[string]mcptoken),
	}
	_, ok := store.get("https://never-saved.example.com")
	require.False(t, ok, "get should report ok=false for an unknown server")
}

func TestTokenStore_LoadMissingFile(t *testing.T) {
	t.Parallel()
	// Pointing at a non-existent file must not panic and must leave the
	// store empty (first-run / no-tokens-yet case).
	store := &tokenStore{
		path: filepath.Join(t.TempDir(), "does-not-exist.json"),
		data: make(map[string]mcptoken),
	}
	store.load()
	require.Empty(t, store.data, "load of a missing file should leave the store empty")
	_, ok := store.get("https://x.example.com")
	require.False(t, ok)
}

func TestTokenStore_LoadCorruptFile(t *testing.T) {
	t.Parallel()
	// A corrupt token file must be ignored gracefully rather than crashing
	// Crush on startup.
	path := filepath.Join(t.TempDir(), "tokens.json")
	require.NoError(t, os.WriteFile(path, []byte("{ not valid json"), 0o600))
	store := &tokenStore{path: path, data: make(map[string]mcptoken)}
	require.NotPanics(t, func() { store.load() })
	require.Empty(t, store.data, "a corrupt token file should be ignored, leaving the store empty")
}

func TestMCPOAuthHandler_RestoresSavedToken(t *testing.T) {
	// With a token already persisted for the server, a new handler should
	// restore a non-nil token source so the browser flow is skipped.
	globalDir := t.TempDir()
	t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)

	serverURL := "https://mcp.example.com/mcp"
	seed := &tokenStore{
		path: filepath.Join(filepath.Dir(config.GlobalConfig()), tokenFileName),
		data: make(map[string]mcptoken),
	}
	saved := mcptoken{
		Token:    &oauth2.Token{AccessToken: "abc123", RefreshToken: "refresh456", TokenType: "Bearer"},
		ClientID: "test-client",
	}
	saved.Endpoints.AuthURL = "https://auth.example.com/authorize"
	saved.Endpoints.TokenURL = "https://auth.example.com/token"
	seed.save(serverURL, saved)

	h := newMCPOAuthHandler(serverURL)
	ts, err := h.TokenSource(t.Context())
	require.NoError(t, err)
	require.NotNil(t, ts, "a saved token should produce a non-nil token source")

	tok, err := ts.Token()
	require.NoError(t, err)
	require.Equal(t, "abc123", tok.AccessToken, "restored token source should yield the saved (unexpired) token")
}

// TestTokenStore_SaveMergesWithDisk pins the multi-server persistence fix:
// each HTTP MCP server gets its own handler with its own store snapshot, and
// a save used to rewrite the whole file from that snapshot — server B's save
// erased server A's token. Save now merges with what's on disk.
func TestTokenStore_SaveMergesWithDisk(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tokens.json")

	// Handler A's store saves server A.
	storeA := &tokenStore{path: path, data: make(map[string]mcptoken)}
	storeA.load()
	storeA.save("https://a.example.com/mcp", mcptoken{Token: &oauth2.Token{AccessToken: "tok-a"}})

	// Handler B's store loaded before/without A's save (its own snapshot).
	storeB := &tokenStore{path: path, data: make(map[string]mcptoken)}
	storeB.save("https://b.example.com/mcp", mcptoken{Token: &oauth2.Token{AccessToken: "tok-b"}})

	// Both tokens must survive on disk.
	check := &tokenStore{path: path, data: make(map[string]mcptoken)}
	check.load()
	gotA, okA := check.get("https://a.example.com/mcp")
	require.True(t, okA, "server A's token was erased by server B's save")
	require.Equal(t, "tok-a", gotA.Token.AccessToken)
	gotB, okB := check.get("https://b.example.com/mcp")
	require.True(t, okB)
	require.Equal(t, "tok-b", gotB.Token.AccessToken)
}

// TestPersistingTokenSource_RepersistsRotatedToken pins the refresh fix:
// OAuth 2.1 public clients rotate the refresh token on use; a refresh that
// stayed only in memory invalidated the copy on disk, so the NEXT restart
// fell back to the browser flow. The wrapping source re-persists on change.
func TestPersistingTokenSource_RepersistsRotatedToken(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("CRUSH_GLOBAL_CONFIG", globalDir)

	serverURL := "https://rotate.example.com/mcp"
	h := newMCPOAuthHandler(serverURL)
	h.clientID = "client-1"
	h.endpoints = oauthEndpoints{AuthURL: "https://as.example.com/auth", TokenURL: "https://as.example.com/token"}

	rotated := &oauth2.Token{AccessToken: "new-access", RefreshToken: "new-refresh"}
	src := &persistingTokenSource{
		h:    h,
		src:  oauth2.StaticTokenSource(rotated),
		last: &oauth2.Token{AccessToken: "old-access", RefreshToken: "old-refresh"},
	}

	tok, err := src.Token()
	require.NoError(t, err)
	require.Equal(t, "new-access", tok.AccessToken)

	// The rotated token must be on disk, with the client ID and endpoints.
	check := newTokenStore()
	check.load()
	got, ok := check.get(serverURL)
	require.True(t, ok, "rotated token was not re-persisted")
	require.Equal(t, "new-access", got.Token.AccessToken)
	require.Equal(t, "new-refresh", got.Token.RefreshToken)
	require.Equal(t, "client-1", got.ClientID)
	require.Equal(t, "https://as.example.com/token", got.Endpoints.TokenURL)
}

// TestCaptureFromAuthURL pins the client-ID persistence fix: the SDK never
// exposes the dynamically registered client ID, but it necessarily appears
// as client_id in the authorization URL handed to the code fetcher. Before
// the fix, ClientID was persisted as "" on every path, so refresh after
// restart could never work (a public client must send client_id).
func TestCaptureFromAuthURL(t *testing.T) {
	t.Setenv("CRUSH_GLOBAL_CONFIG", t.TempDir())
	h := newMCPOAuthHandler("https://cap.example.com/mcp")

	h.captureFromAuthURL("https://as.example.com/authorize?client_id=dyn-client-42&response_type=code&state=xyz")

	require.Equal(t, "dyn-client-42", h.clientID)
	require.Equal(t, "https://as.example.com/authorize", h.endpoints.AuthURL)

	// persistToken must write the captured ID (TokenURL pre-set so the
	// test performs no network discovery).
	h.endpoints.TokenURL = "https://as.example.com/token"
	h.persistToken(&oauth2.Token{AccessToken: "t"})
	check := newTokenStore()
	check.load()
	got, ok := check.get("https://cap.example.com/mcp")
	require.True(t, ok)
	require.Equal(t, "dyn-client-42", got.ClientID, "registered client ID must be persisted")
}

// TestCallbackHandler_DuplicateHitDoesNotBlock pins the wedge fix: a second
// /callback request (browser refresh, prefetcher, AS retry) used to block
// its handler goroutine forever on the full capacity-1 channel; the deferred
// srv.Shutdown then waited for that handler indefinitely and the whole
// authorization flow leaked.
func TestCallbackHandler_DuplicateHitDoesNotBlock(t *testing.T) {
	t.Parallel()
	resultCh := make(chan auth.AuthorizationResult, 1)
	errCh := make(chan error, 1)
	handler := callbackHandler(resultCh, errCh)

	hit := func() {
		req := httptest.NewRequest("GET", "/callback?code=abc&state=s1", nil)
		handler(httptest.NewRecorder(), req)
	}

	hit() // first delivery fills the channel

	done := make(chan struct{})
	go func() {
		hit() // duplicate — must not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate callback hit blocked (would wedge srv.Shutdown and leak the auth flow)")
	}

	require.Len(t, resultCh, 1, "exactly one result delivered")
	got := <-resultCh
	require.Equal(t, "abc", got.Code)
}
