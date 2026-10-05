package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// buildEmptyWorkspace returns a controller wired to a backend that owns
// a single workspace with no sessions.
func buildEmptyWorkspace(t *testing.T) (*controllerV1, string) {
	t.Helper()

	b := backend.New(context.Background(), nil, nil)
	wsID := uuid.New().String()
	a := &app.App{AgentCoordinator: &stubCoordinator{busy: map[string]bool{}}}
	a.Sessions = &stubSessions{}

	ws := &backend.Workspace{
		ID:   wsID,
		Path: t.TempDir(),
		App:  a,
	}
	backend.InsertWorkspaceForTest(b, ws)

	s := &Server{backend: b}
	return &controllerV1{backend: b, server: s}, wsID
}

// TestChildSessionsRouteEmptyWorkspace lists every child session in an
// empty workspace: the route must return an empty list, not null.
func TestChildSessionsRouteEmptyWorkspace(t *testing.T) {
	t.Parallel()
	c, wsID := buildEmptyWorkspace(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/workspaces/"+wsID+"/child-sessions", nil)
	req.SetPathValue("id", wsID)
	rec := httptest.NewRecorder()
	c.handleGetWorkspaceChildSessions(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got []proto.Session
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got)
	require.Empty(t, got)
}

// TestChildSessionsRouteUnknownWorkspace: an unknown workspace is a 404.
func TestChildSessionsRouteUnknownWorkspace(t *testing.T) {
	t.Parallel()
	c, _ := buildEmptyWorkspace(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/workspaces/"+uuid.New().String()+"/child-sessions", nil)
	req.SetPathValue("id", uuid.New().String())
	rec := httptest.NewRecorder()
	c.handleGetWorkspaceChildSessions(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
