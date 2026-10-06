package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ListChildren filters the fixed session list by parent, mirroring the
// service's per-parent listing closely enough for the handler tests.
func (s *stubSessions) ListChildren(_ context.Context, parentID string) ([]session.Session, error) {
	kids := make([]session.Session, 0)
	for _, sess := range s.all {
		if sess.ParentSessionID == parentID {
			kids = append(kids, sess)
		}
	}
	return kids, nil
}

// buildChildrenWorkspace returns a controller wired to a backend that
// owns a single workspace serving the given sessions, with the named
// sessions reported busy by the coordinator.
func buildChildrenWorkspace(t *testing.T, all []session.Session, busy map[string]bool) (*controllerV1, string) {
	t.Helper()

	b := backend.New(context.Background(), nil, nil)
	wsID := uuid.New().String()
	coord := &stubCoordinator{busy: busy}
	a := &app.App{AgentCoordinator: coord}
	a.Sessions = &stubSessions{all: all}

	ws := &backend.Workspace{
		ID:   wsID,
		Path: t.TempDir(),
		App:  a,
	}
	backend.InsertWorkspaceForTest(b, ws)

	s := &Server{backend: b}
	return &controllerV1{backend: b, server: s}, wsID
}

func requestSessionChildren(t *testing.T, c *controllerV1, wsID, parentID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/v1/workspaces/"+wsID+"/sessions/"+parentID+"/children", nil)
	req.SetPathValue("id", wsID)
	req.SetPathValue("sid", parentID)
	rec := httptest.NewRecorder()
	c.handleGetWorkspaceSessionChildren(rec, req)
	return rec
}

// TestSessionChildrenEmptyListsAsEmptyArray pins the happy-path
// contract the picker relies on (#314, #427): a session with no
// children answers 200 with an empty JSON array, not null.
func TestSessionChildrenEmptyListsAsEmptyArray(t *testing.T) {
	t.Parallel()
	const parentID = "s-parent"
	c, wsID := buildChildrenWorkspace(t,
		[]session.Session{{ID: parentID, Title: "parent"}},
		map[string]bool{})

	rec := requestSessionChildren(t, c, wsID, parentID)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "[]", strings.TrimSpace(rec.Body.String()),
		"no children must marshal as an empty JSON array, not null")
}

// TestSessionChildrenListsDirectChildrenWithBusy pins that the handler
// returns exactly the parent's direct children and takes IsBusy from
// the coordinator (#314, #427).
func TestSessionChildrenListsDirectChildrenWithBusy(t *testing.T) {
	t.Parallel()
	const (
		parentID = "s-parent"
		childID  = "s-child-busy"
		otherID  = "s-child-idle"
	)
	all := []session.Session{
		{ID: parentID, Title: "parent"},
		{ID: childID, ParentSessionID: parentID, Title: "busy child"},
		{ID: otherID, ParentSessionID: parentID, Title: "idle child"},
		{ID: "s-unrelated", Title: "unrelated"},
	}
	c, wsID := buildChildrenWorkspace(t, all, map[string]bool{childID: true})

	rec := requestSessionChildren(t, c, wsID, parentID)
	require.Equal(t, http.StatusOK, rec.Code)

	var got []proto.Session
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	byID := map[string]proto.Session{}
	for _, s := range got {
		byID[s.ID] = s
	}
	require.Len(t, got, 2, "exactly the direct children")
	require.Equal(t, parentID, byID[childID].ParentSessionID)
	require.True(t, byID[childID].IsBusy, "expected IsBusy=true from the coordinator")
	require.False(t, byID[otherID].IsBusy, "expected IsBusy=false for the idle child")
	_, ok := byID["s-unrelated"]
	require.False(t, ok, "unrelated top-level sessions are not children")
}

// TestSessionChildrenUnknownWorkspace404 pins the error contract: a
// request for a workspace the backend does not own answers 404.
func TestSessionChildrenUnknownWorkspace404(t *testing.T) {
	t.Parallel()
	c, _ := buildChildrenWorkspace(t, nil, map[string]bool{})

	rec := requestSessionChildren(t, c, "no-such-workspace", "s-1")
	require.Equal(t, http.StatusNotFound, rec.Code)
}
