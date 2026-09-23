package backend

import (
	"context"
	"sync"
)

// workspaceTracker counts workspaces that exist but whose teardown hook
// has not yet returned.
//
// It exists because "the workspace is gone" and "the resources the
// workspace held are released" are two different moments:
// [Backend.teardown] drops the workspace from the index and releases
// b.mu *before* calling ws.invokeShutdown, so the workspace map is
// already empty while the pooled DB connection is still open. Polling
// the map is therefore a proxy, not the property — and on Windows the
// difference is a failed directory removal, because the OS refuses to
// unlink a file another handle still has open.
//
// A workspace is registered at birth (as soon as it owns resources) and
// deregistered once its teardown hook has *returned*, which is the only
// point at which the release is genuinely complete.
type workspaceTracker struct {
	mu      sync.Mutex
	live    int
	waiters []chan struct{}
}

// born records a workspace that now owns resources.
func (t *workspaceTracker) born() {
	t.mu.Lock()
	t.live++
	t.mu.Unlock()
}

// retired records that a workspace's teardown hook has returned,
// waking every waiter once the last one is done.
func (t *workspaceTracker) retired() {
	t.mu.Lock()
	t.live--
	if t.live == 0 {
		for _, w := range t.waiters {
			close(w)
		}
		t.waiters = nil
	}
	t.mu.Unlock()
}

// wait blocks until every registered workspace's teardown hook has
// returned, or ctx is done. It returns ctx.Err() on timeout so callers
// can report the stall rather than hang.
func (t *workspaceTracker) wait(ctx context.Context) error {
	t.mu.Lock()
	if t.live == 0 {
		t.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	t.waiters = append(t.waiters, done)
	t.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
