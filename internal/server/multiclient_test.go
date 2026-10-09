package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// installSyntheticWorkspace creates a synthetic [backend.Workspace]
// registered with the controller's backend, suitable for handler-level
// tests that do not need a real [app.App]. The workspace's ID is a
// fresh UUID and its path is a tempdir; teardown is the caller's
// responsibility (handlers should not rely on synthetic workspaces
// disappearing automatically).
func installSyntheticWorkspace(t *testing.T, c *controllerV1) *backend.Workspace {
	t.Helper()
	ws := &backend.Workspace{
		ID:   uuid.New().String(),
		Path: t.TempDir(),
	}
	backend.InsertWorkspaceForTest(c.backend, ws)
	return ws
}

// newTestController builds a controllerV1 around a backend without a
// real config store, suitable for handler-level 400 tests.
func newTestController() *controllerV1 {
	s := &Server{}
	s.backend = backend.New(context.Background(), nil, nil)
	return &controllerV1{backend: s.backend, server: s}
}

func TestPostWorkspaces_RejectsMissingClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	body, err := json.Marshal(proto.Workspace{Path: t.TempDir()})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.handlePostWorkspaces(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var perr proto.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &perr))
	require.Contains(t, perr.Message, "client_id")
}

func TestPostWorkspaces_RejectsMalformedClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	body, err := json.Marshal(proto.Workspace{Path: t.TempDir(), ClientID: "not-a-uuid"})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.handlePostWorkspaces(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestPostWorkspaces_RefusesParentReferences pins the API boundary for the
// directories the server works under: a ".." that survives cleaning (one
// that climbs above the path's own start) is refused in both path and
// data_dir, an empty path is refused, and a path that cleans to a plain
// directory is accepted as far as this check goes.
func TestPostWorkspaces_RefusesParentReferences(t *testing.T) {
	t.Parallel()

	post := func(t *testing.T, ws proto.Workspace) *httptest.ResponseRecorder {
		t.Helper()
		c := newTestController()
		body, err := json.Marshal(ws)
		require.NoError(t, err)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c.handlePostWorkspaces(rec, req)
		return rec
	}
	message := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var perr proto.Error
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &perr))
		return perr.Message
	}

	t.Run("parent reference in path", func(t *testing.T) {
		t.Parallel()
		rec := post(t, proto.Workspace{Path: "../../../etc"})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, message(t, rec), `path must not contain ".."`)
	})
	t.Run("parent reference in data_dir", func(t *testing.T) {
		t.Parallel()
		rec := post(t, proto.Workspace{Path: t.TempDir(), DataDir: "../../elsewhere"})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, message(t, rec), `data_dir must not contain ".."`)
	})
	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		rec := post(t, proto.Workspace{})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, message(t, rec), "path is required")
	})
	t.Run("clean path passes this check", func(t *testing.T) {
		t.Parallel()
		// An absolute path with a ".." segment cleans to a plain directory
		// and is accepted; the request still fails later (no client_id),
		// which proves the path check let it through.
		rec := post(t, proto.Workspace{Path: t.TempDir() + "/sub/../other"})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, message(t, rec), "client_id")
	})
}

// TestSanitizedWorkspaceRequestCopiesEveryField fills every field of
// proto.Workspace and checks the rebuilt request carries each one, with
// only the directory fields cleaned, so a field added to proto.Workspace
// cannot be silently dropped on the way to the backend.
func TestSanitizedWorkspaceRequestCopiesEveryField(t *testing.T) {
	t.Parallel()

	var args proto.Workspace
	v := reflect.ValueOf(&args).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("/x/./" + v.Type().Field(i).Name)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Ptr:
			f.Set(reflect.New(f.Type().Elem()))
		default:
			t.Fatalf("field %s has kind %s; teach this test to fill it", v.Type().Field(i).Name, f.Kind())
		}
	}

	got, err := sanitizedWorkspaceRequest(args)
	require.NoError(t, err)
	gv := reflect.ValueOf(got)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		want := v.Field(i).Interface()
		if name == "Path" || name == "DataDir" {
			want = filepath.Clean(v.Field(i).String())
		}
		require.Equal(t, want, gv.Field(i).Interface(), "field %s must reach the backend", name)
	}
}

func TestCleanWorkspacePath(t *testing.T) {
	t.Parallel()
	got, err := cleanWorkspacePath("path", "/a/b/./c/../d")
	require.NoError(t, err)
	require.Equal(t, filepath.Clean("/a/b/d"), got)
	// Parent references inside an absolute path normalize away; only a
	// relative path can keep one.
	got, err = cleanWorkspacePath("path", "/a/../../b")
	require.NoError(t, err)
	require.Equal(t, filepath.Clean("/b"), got)
	_, err = cleanWorkspacePath("path", "../b")
	require.Error(t, err)
	_, err = cleanWorkspacePath("data_dir", "x/../../y")
	require.Error(t, err)
	_, err = cleanWorkspacePath("path", "")
	require.Error(t, err)
}

func TestDeleteWorkspace_RejectsMissingClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/v1/workspaces/abc", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	c.handleDeleteWorkspaces(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDeleteWorkspace_RejectsMalformedClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/v1/workspaces/abc?client_id=nope", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	c.handleDeleteWorkspaces(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSubscribeEvents_RejectsMissingClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/workspaces/abc/events", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	c.handleGetWorkspaceEvents(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSubscribeEvents_RejectsMalformedClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/workspaces/abc/events?client_id=nope", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()

	c.handleGetWorkspaceEvents(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// postCurrentSession is a small helper that POSTs the JSON body to
// /v1/workspaces/{id}/current-session?client_id=cid and returns the
// recorder. It does not require a real listener.
func postCurrentSession(t *testing.T, c *controllerV1, wsID, clientID, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(proto.CurrentSession{SessionID: sessionID})
	require.NoError(t, err)
	url := "/v1/workspaces/" + wsID + "/current-session"
	if clientID != "" {
		url += "?client_id=" + clientID
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	req.SetPathValue("id", wsID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c.handlePostWorkspaceCurrentSession(rec, req)
	return rec
}

func TestPostCurrentSession_RejectsMissingClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	body, err := json.Marshal(proto.CurrentSession{SessionID: "S1"})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/abc/current-session", bytes.NewReader(body))
	req.SetPathValue("id", "abc")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.handlePostWorkspaceCurrentSession(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostCurrentSession_RejectsMalformedClientID(t *testing.T) {
	t.Parallel()
	c := newTestController()

	rec := postCurrentSession(t, c, "abc", "not-a-uuid", "S1")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostCurrentSession_RejectsBadBody(t *testing.T) {
	t.Parallel()
	c := newTestController()

	cid := uuid.New().String()
	url := "/v1/workspaces/abc/current-session?client_id=" + cid
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader([]byte("not-json")))
	req.SetPathValue("id", "abc")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.handlePostWorkspaceCurrentSession(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostCurrentSession_UnknownWorkspace(t *testing.T) {
	t.Parallel()
	c := newTestController()

	rec := postCurrentSession(t, c, uuid.New().String(), uuid.New().String(), "S1")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPostCurrentSession_UnknownClient(t *testing.T) {
	t.Parallel()
	c := newTestController()
	ws := installSyntheticWorkspace(t, c)

	rec := postCurrentSession(t, c, ws.ID, uuid.New().String(), "S1")
	// 409, not 404: the workspace is fine, this client just has no stream
	// on it. A 404 would look like "workspace gone" and send a recovering
	// client off to re-register for no reason.
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestPostCurrentSession_HoldOnly(t *testing.T) {
	t.Parallel()
	c := newTestController()
	ws := installSyntheticWorkspace(t, c)

	cid := uuid.New().String()
	require.NoError(t, backend.RegisterClientForTesting(c.backend, ws, cid))
	t.Cleanup(func() { _ = c.backend.DeleteWorkspace(ws.ID, cid) })

	rec := postCurrentSession(t, c, ws.ID, cid, "S1")
	require.Equal(t, http.StatusConflict, rec.Code, "hold-only client must be rejected")
}

func TestPostCurrentSession_AttachedClientSucceeds(t *testing.T) {
	t.Parallel()
	c := newTestController()
	ws := installSyntheticWorkspace(t, c)

	cid := uuid.New().String()
	require.NoError(t, c.backend.AttachClient(ws.ID, cid))
	t.Cleanup(func() { c.backend.DetachClient(ws.ID, cid) })

	rec := postCurrentSession(t, c, ws.ID, cid, "S1")
	require.Equal(t, http.StatusOK, rec.Code)

	// Clearing also returns 200.
	rec = postCurrentSession(t, c, ws.ID, cid, "")
	require.Equal(t, http.StatusOK, rec.Code)
}
