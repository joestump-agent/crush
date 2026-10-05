package cmd

import (
	"context"
	"fmt"
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// resolveWorkspaceStub is a minimal workspace for
// resolveWorkspaceSessionID tests. Unimplemented methods panic through
// the embedded interface.
type resolveWorkspaceStub struct {
	workspace.Workspace

	sessions map[string]session.Session
}

func (w *resolveWorkspaceStub) GetSession(_ context.Context, id string) (session.Session, error) {
	sess, ok := w.sessions[id]
	if !ok {
		return session.Session{}, fmt.Errorf("session %q not found", id)
	}
	return sess, nil
}

// ListSessions returns only top-level sessions, as the real workspaces
// do (the query filters on parent_session_id IS NULL).
func (w *resolveWorkspaceStub) ListSessions(_ context.Context) ([]session.Session, error) {
	var out []session.Session
	for _, s := range w.sessions {
		if s.ParentSessionID == "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (w *resolveWorkspaceStub) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	for i := 0; i+1 < len(sessionID); i++ {
		if sessionID[i] == '$' && sessionID[i+1] == '$' {
			return sessionID[:i], sessionID[i+2:], true
		}
	}
	return "", "", false
}

// TestResolveWorkspaceSessionID_RefusesTitleChild pins #413: crush -s
// must not open a child session that is not an agent-tool task session
// (e.g. a title-generation session) as the active TUI session.
func TestResolveWorkspaceSessionID_RefusesTitleChild(t *testing.T) {
	ws := &resolveWorkspaceStub{sessions: map[string]session.Session{
		"parent-1":       {ID: "parent-1", Title: "Parent"},
		"title-parent-1": {ID: "title-parent-1", ParentSessionID: "parent-1", Title: "Generate a title"},
	}}

	_, err := resolveWorkspaceSessionID(t.Context(), ws, "title-parent-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot open a child session")
}

// TestResolveWorkspaceSessionID_AllowsTaskChild pins the other side of
// #413: an agent-tool task session is accepted by the resolver; the TUI
// routes it into inspect mode under its parent.
func TestResolveWorkspaceSessionID_AllowsTaskChild(t *testing.T) {
	ws := &resolveWorkspaceStub{sessions: map[string]session.Session{
		"parent-1":      {ID: "parent-1", Title: "Parent"},
		"msg-1$$call-1": {ID: "msg-1$$call-1", ParentSessionID: "parent-1", Title: "Dispatched Agent"},
	}}

	sess, err := resolveWorkspaceSessionID(t.Context(), ws, "msg-1$$call-1")
	require.NoError(t, err)
	require.Equal(t, "msg-1$$call-1", sess.ID)
}

// TestResolveWorkspaceSessionID_TopLevelUnchanged pins that a
// top-level session, full ID or hash prefix, resolves exactly as before
// #413.
func TestResolveWorkspaceSessionID_TopLevelUnchanged(t *testing.T) {
	ws := &resolveWorkspaceStub{sessions: map[string]session.Session{
		"3b241116-5a79-4f5f-8c1f-9d2b1a3e5c67": {ID: "3b241116-5a79-4f5f-8c1f-9d2b1a3e5c67", Title: "Full"},
		"9f86d081-8a45-4c2e-9b3a-1c2d3e4f5a6b": {ID: "9f86d081-8a45-4c2e-9b3a-1c2d3e4f5a6b", Title: "Prefixed"},
	}}

	sess, err := resolveWorkspaceSessionID(t.Context(), ws, "3b241116-5a79-4f5f-8c1f-9d2b1a3e5c67")
	require.NoError(t, err)
	require.Equal(t, "3b241116-5a79-4f5f-8c1f-9d2b1a3e5c67", sess.ID)

	prefix := session.HashID("9f86d081-8a45-4c2e-9b3a-1c2d3e4f5a6b")[:8]
	sess, err = resolveWorkspaceSessionID(t.Context(), ws, prefix)
	require.NoError(t, err)
	require.Equal(t, "9f86d081-8a45-4c2e-9b3a-1c2d3e4f5a6b", sess.ID)
}
