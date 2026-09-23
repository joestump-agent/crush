package backend

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestWaitForWorkspaceTeardown_BlocksUntilShutdownHookReturns pins the
// property the Windows temp-dir cleanup depends on: the wait must not
// return while a teardown hook is still running.
//
// It is written against the exact ordering that makes the workspace map
// a bad proxy — [Backend.teardown] removes the workspace from the map
// and releases b.mu before it calls invokeShutdown — so the hook here
// parks mid-flight with the map already empty. A wait built on
// ListWorkspaces would report "released" at that moment and fail this
// test.
func TestWaitForWorkspaceTeardown_BlocksUntilShutdownHookReturns(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)

	entered := make(chan struct{})
	unblock := make(chan struct{})
	var hookReturned atomic.Bool

	ws := &Workspace{
		ID:           uuid.New().String(),
		Path:         t.TempDir(),
		resolvedPath: t.TempDir(),
		shutdownFn: func() {
			close(entered)
			<-unblock
			hookReturned.Store(true)
		},
	}
	InsertWorkspaceForTest(b, ws)

	go b.teardown(ws)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown never reached the shutdown hook")
	}

	// The workspace is already out of the map at this point; that is
	// precisely the state in which the old proxy lied.
	require.Empty(t, b.ListWorkspaces(),
		"precondition: teardown empties the map before running the hook")

	waitCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitForWorkspaceTeardownForTest(waitCtx, b), context.DeadlineExceeded,
		"wait must block while the shutdown hook is still running")
	require.False(t, hookReturned.Load(), "hook must still be in flight")

	close(unblock)

	doneCtx, doneCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer doneCancel()
	require.NoError(t, WaitForWorkspaceTeardownForTest(doneCtx, b))
	require.True(t, hookReturned.Load(),
		"wait must not return until the hook has returned")
}

// TestWaitForWorkspaceTeardown_NoWorkspacesReturnsImmediately guards the
// empty case: a backend that never handed out a workspace must not make
// a caller sit out its context deadline.
func TestWaitForWorkspaceTeardown_NoWorkspacesReturnsImmediately(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, WaitForWorkspaceTeardownForTest(ctx, b))
}

// TestWaitForWorkspaceTeardown_ShutdownReportedOnce covers the
// lost-race paths in CreateWorkspace, which call invokeShutdown on a
// workspace another path may also release. A double report would drive
// the counter negative and make every later wait return early.
func TestWaitForWorkspaceTeardown_ShutdownReportedOnce(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)

	var shutdowns atomic.Int32
	first := &Workspace{
		ID:           uuid.New().String(),
		Path:         t.TempDir(),
		resolvedPath: t.TempDir(),
		shutdownFn:   func() { shutdowns.Add(1) },
	}
	InsertWorkspaceForTest(b, first)
	first.invokeShutdown()
	first.invokeShutdown()
	require.Equal(t, int32(2), shutdowns.Load(), "both calls must reach the hook")

	// A second, still-live workspace must keep the wait blocked. If the
	// double invokeShutdown above had decremented twice, the counter
	// would now be zero and this wait would return nil.
	held := make(chan struct{})
	second := &Workspace{
		ID:           uuid.New().String(),
		Path:         t.TempDir(),
		resolvedPath: t.TempDir(),
		shutdownFn:   func() { <-held },
	}
	InsertWorkspaceForTest(b, second)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitForWorkspaceTeardownForTest(ctx, b), context.DeadlineExceeded)

	close(held)
	second.invokeShutdown()

	doneCtx, doneCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer doneCancel()
	require.NoError(t, WaitForWorkspaceTeardownForTest(doneCtx, b))
}
