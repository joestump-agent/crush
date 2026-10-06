package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClientListChildSessionsDecodesArray pins the client decode of
// the per-parent child listing (#314, #427): the server's JSON array
// round-trips into proto sessions.
func TestClientListChildSessionsDecodesArray(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/workspaces/ws-1/sessions/parent-1/children", r.URL.Path)
		_, err := w.Write([]byte(`[{"id":"child-1","parent_session_id":"parent-1","title":"child one"}]`))
		require.NoError(t, err)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	sessions, err := c.ListChildSessions(t.Context(), "ws-1", "parent-1")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "child-1", sessions[0].ID)
	require.Equal(t, "parent-1", sessions[0].ParentSessionID)
	require.Equal(t, "child one", sessions[0].Title)
}

// TestClientListChildSessionsNon200SurfacesStatusCode pins the error
// path (#427): a non-200 response is an error that names the status,
// so callers can tell a missing workspace apart from a decode failure.
func TestClientListChildSessionsNon200SurfacesStatusCode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	_, err := c.ListChildSessions(t.Context(), "ws-1", "no-such-session")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 404")
}
