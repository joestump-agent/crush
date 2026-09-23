package agent

import "sync"

// readiness is a one-shot latch over an agent's build-time setup work.
//
// buildAgent hands a SessionAgent back before its system prompt and initial
// tool list exist, and builds both in background goroutines. The latch is how
// a caller waits for its own agent's setup to finish: settle records the
// outcome once and releases every waiter, wait observes it.
//
// One-shot is the point. The latch is closed exactly once and never rearmed,
// so a later agent build cannot mutate state a waiter is already parked on.
// The coordinator previously shared a single errgroup.Group across every
// build and every run, so buildAgent's Add could land while a run's Wait was
// outstanding — which Go 1.27's sync.WaitGroup treats as reuse and panics on,
// crash-looping Crush in channel mode (joestump-agent/crush#298).
//
// @joestump-agent 09/22/2026 - Added, replacing coordinator.readyWg.
type readiness struct {
	done chan struct{}
	once sync.Once
	// err is written before done is closed, so a waiter that observes the
	// close also observes the error.
	err error
}

// newReadiness returns an unsatisfied latch for work that is about to start.
func newReadiness() *readiness {
	return &readiness{done: make(chan struct{})}
}

// readyNow returns a latch that is already satisfied, for agents whose system
// prompt and tools are supplied at construction time and so have no setup
// work to wait for.
func readyNow() *readiness {
	r := newReadiness()
	r.settle(nil)
	return r
}

// settle records the outcome of the setup work and releases every waiter.
// Only the first call has any effect.
func (r *readiness) settle(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.done)
	})
}

// wait blocks until the setup work has settled, then returns its error. It is
// safe to call from any number of goroutines, before or after settling.
func (r *readiness) wait() error {
	<-r.done
	return r.err
}
