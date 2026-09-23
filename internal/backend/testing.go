package backend

import (
	"context"
)

// InsertWorkspaceForTest registers ws with b under its current ID and
// path. It is intended for tests in other packages that need to drive
// HTTP handlers against a synthetic workspace without booting a real
// app.App. Production code should go through CreateWorkspace.
//
// If the workspace has no run context yet it is derived from the
// backend context (falling back to context.Background), mirroring the
// initialization CreateWorkspace performs, so dispatched agent runs
// have a non-nil ws.ctx.
func InsertWorkspaceForTest(b *Backend, ws *Workspace) {
	if ws.resolvedPath == "" {
		ws.resolvedPath = ws.Path
	}
	if ws.clients == nil {
		ws.clients = make(map[string]*clientState)
	}
	if ws.ctx == nil {
		parent := b.ctx
		if parent == nil {
			parent = context.Background()
		}
		ws.ctx, ws.cancel = context.WithCancel(parent)
	}
	ws.trackWith(&b.tracker)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.workspaces.Set(ws.ID, ws)
	if ws.resolvedPath != "" {
		b.pathIndex[ws.resolvedPath] = ws.ID
	}
}

// WaitForWorkspaceTeardownForTest blocks until every workspace this
// backend has handed out has had its teardown hook run to completion,
// or ctx is done.
//
// This is deliberately not [Backend.ListWorkspaces] or
// [Backend.Shutdown]. Teardown drops a workspace from the index and
// releases the backend lock BEFORE it invokes the workspace's shutdown
// hook, so an empty workspace map says only that the workspace is
// unreachable — its pooled DB connection may still be open. Backend
// shutdown, meanwhile, runs only the server-level callback and closes
// no workspace resources at all.
//
// Tests that let [t.TempDir] own a workspace's data directory must wait
// on this before the directory is removed. On Unix an open file unlinks
// happily; Windows refuses, and the failure surfaces as a "TempDir
// RemoveAll cleanup ... being used by another process" error attributed
// to whichever test lost the race.
func WaitForWorkspaceTeardownForTest(ctx context.Context, b *Backend) error {
	return b.tracker.wait(ctx)
}

// RegisterClientForTesting installs a creation hold for clientID on
// ws using the backend's normal registerClient path. Intended for
// tests in other packages that need to drive a hold-only client
// (streams == 0) without booting a real CreateWorkspace flow.
func RegisterClientForTesting(b *Backend, ws *Workspace, clientID string) error {
	if _, err := validateClientID(clientID); err != nil {
		return err
	}
	b.registerClient(ws, clientID)
	return nil
}

// SetWorkspaceShutdownFnForTest overrides the workspace teardown
// callback. Useful for tests in other packages that drive synthetic
// workspaces (where the embedded [app.App] is incomplete) through
// detach paths that would otherwise crash inside App.Shutdown.
func SetWorkspaceShutdownFnForTest(ws *Workspace, fn func()) {
	ws.shutdownFn = fn
}

// WorkspaceLiveStreamCountForTest returns the number of clients on ws
// that have at least one live SSE stream. Used by integration tests
// in other packages to wait for SSE attaches before publishing events.
func WorkspaceLiveStreamCountForTest(ws *Workspace) int {
	ws.clientsMu.Lock()
	defer ws.clientsMu.Unlock()
	n := 0
	for _, cs := range ws.clients {
		if cs.streams > 0 {
			n++
		}
	}
	return n
}
