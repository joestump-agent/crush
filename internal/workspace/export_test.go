package workspace

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// ConsumeEventsForTest runs the event-handling loop on the given
// channel, invoking send for translated domain messages and refreshing
// the cached workspace snapshot on ConfigChanged. Exposed for
// cross-package integration tests that cannot rely on a real
// *tea.Program. It returns when evc is closed.
func (w *ClientWorkspace) ConsumeEventsForTest(evc <-chan any, send func(tea.Msg)) {
	w.consumeEvents(evc, send)
}

// RunSubscriptionForTest runs the full subscribe/reconnect/recovery loop.
// Exposed for cross-package integration tests. It returns once the
// workspace is shut down.
func (w *ClientWorkspace) RunSubscriptionForTest(send func(tea.Msg)) {
	w.runSubscription(send)
}

// WorkspaceIDForTest returns the currently cached workspace ID, which
// recovery may have re-minted.
func (w *ClientWorkspace) WorkspaceIDForTest() string {
	return w.workspaceID()
}

// SetSSEBackoffForTest shrinks the subscription reconnect backoff and
// returns a restore function for t.Cleanup. Stored atomically: the
// subscription goroutine reads the bounds while a test's cleanup restores
// them, and unsynchronized reads and writes there are a data race.
func SetSSEBackoffForTest(initial, maxBackoff time.Duration) (restore func()) {
	origInitial, origMax := sseReconnectInitialBackoffNS.Load(), sseReconnectMaxBackoffNS.Load()
	sseReconnectInitialBackoffNS.Store(int64(initial))
	sseReconnectMaxBackoffNS.Store(int64(maxBackoff))
	return func() {
		sseReconnectInitialBackoffNS.Store(origInitial)
		sseReconnectMaxBackoffNS.Store(origMax)
	}
}

// recoveryCreateTimeoutForTest shrinks the bound on a single workspace
// re-registration attempt and returns a restore function for t.Cleanup.
func recoveryCreateTimeoutForTest(d time.Duration) (restore func()) {
	orig := recoveryCreateTimeoutNS.Load()
	recoveryCreateTimeoutNS.Store(int64(d))
	return func() { recoveryCreateTimeoutNS.Store(orig) }
}
