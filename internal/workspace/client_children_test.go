package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/charmbracelet/crush/internal/client"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestClientWorkspaceListChildSessionsRoundTrip pins the client-mode
// child listing (#314, #427): the proto sessions come back through
// protoToSession with ID, ParentSessionID and Title intact.
func TestClientWorkspaceListChildSessionsRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/workspaces/ws-1/sessions/parent-1/children", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode([]proto.Session{
			{ID: "child-1", ParentSessionID: "parent-1", Title: "child one"},
			{ID: "child-2", ParentSessionID: "parent-1", Title: "child two"},
		}))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)
	ws := NewClientWorkspace(c, proto.Workspace{ID: "ws-1"})

	got, err := ws.ListChildSessions(t.Context(), "parent-1")
	require.NoError(t, err)
	byID := map[string]session.Session{}
	for _, s := range got {
		byID[s.ID] = s
	}
	require.Len(t, got, 2)
	require.Equal(t, "parent-1", byID["child-1"].ParentSessionID)
	require.Equal(t, "child one", byID["child-1"].Title)
	require.Equal(t, "parent-1", byID["child-2"].ParentSessionID)
	require.Equal(t, "child two", byID["child-2"].Title)
}
