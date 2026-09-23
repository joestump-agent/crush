package agent

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadiness_ReadyNowDoesNotBlock(t *testing.T) {
	t.Parallel()

	require.NoError(t, readyNow().wait())
}

func TestReadiness_WaitBlocksUntilSettled(t *testing.T) {
	t.Parallel()

	r := newReadiness()
	done := make(chan error, 1)
	go func() { done <- r.wait() }()

	select {
	case <-done:
		t.Fatal("wait returned before settle")
	case <-time.After(50 * time.Millisecond):
	}

	want := errors.New("build failed")
	r.settle(want)

	select {
	case err := <-done:
		require.ErrorIs(t, err, want)
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after settle")
	}
}

// TestReadiness_FirstSettleWins pins the one-shot contract: a later settle
// must neither panic on the closed channel nor overwrite the outcome that
// waiters already observed.
func TestReadiness_FirstSettleWins(t *testing.T) {
	t.Parallel()

	first := errors.New("first")
	r := newReadiness()
	r.settle(first)
	r.settle(nil)
	r.settle(errors.New("second"))

	require.ErrorIs(t, r.wait(), first)
}

func TestReadiness_ConcurrentWaitersAllObserveOutcome(t *testing.T) {
	t.Parallel()

	want := errors.New("build failed")
	r := newReadiness()

	const waiters = 16
	errs := make(chan error, waiters)
	var wg sync.WaitGroup
	for range waiters {
		wg.Go(func() { errs <- r.wait() })
	}
	r.settle(want)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.ErrorIs(t, err, want)
	}
}
