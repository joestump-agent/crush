package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// gatedDispatchAgent is a fake dispatched agent whose run blocks on a gate
// so a test can inject messages while the dispatch is genuinely running.
// EnqueueWhenBusy records every injected call under a mutex — the
// coordinator's queue is exercised end to end, the agent is the boundary.
type gatedDispatchAgent struct {
	SessionAgent
	model   Model
	gate    chan struct{}
	entered chan struct{}
	result  *fantasy.AgentResult

	// enterOnce latches the entered notification; gateOnce makes release
	// idempotent (#422): the env's cleanup may release a gate the test
	// body already released.
	enterOnce sync.Once
	gateOnce  sync.Once
	mu        sync.Mutex
	queued    []SessionAgentCall
	lastCall  *SessionAgentCall
	canceled  []string

	// cancelCh closes on the first Cancel so a parked Run returns as if
	// the cancel reached it (#373); cancelOnce keeps the close idempotent.
	cancelCh   chan struct{}
	cancelOnce sync.Once
}

func (f *gatedDispatchAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	callCopy := call
	f.lastCall = &callCopy
	f.mu.Unlock()
	f.enterOnce.Do(func() { close(f.entered) })
	select {
	case <-f.gate:
	case <-f.cancelCh:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.result, nil
}

// lastRunCall returns a copy of the call the dispatched agent ran with,
// for asserting the call shaping (#71).
func (f *gatedDispatchAgent) lastRunCall() (SessionAgentCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastCall == nil {
		return SessionAgentCall{}, false
	}
	return *f.lastCall, true
}

// ranOnce reports whether the agent's Run was entered.
func (f *gatedDispatchAgent) ranOnce() bool {
	_, ok := f.lastRunCall()
	return ok
}

// waitRunning blocks until the dispatched run has entered the agent's Run
// call, which is after runDispatch registered the injection target.
func (f *gatedDispatchAgent) waitRunning(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch run never started")
	}
}

// release opens the gate exactly once so a parked Run can finish. It is
// idempotent (#422): the env's cleanup releases any gate the test body
// already released.
func (f *gatedDispatchAgent) release() {
	f.gateOnce.Do(func() { close(f.gate) })
}

func (f *gatedDispatchAgent) EnqueueWhenBusy(call SessionAgentCall) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, call)
	return true
}

// Cancel records the cancel so a test can assert the coordinator tore
// down an orphaned dispatched run after a transport stream error (#344)
// and that CancelDispatch reached the dispatched agent, never the parent
// (#373). Closing cancelCh un-parks a Run parked on the gate, mirroring a
// real agent whose active request aborts on Cancel.
func (f *gatedDispatchAgent) Cancel(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, sessionID)
	f.cancelOnce.Do(func() { close(f.cancelCh) })
}

// IsSessionBusy reports the session not busy, so the coordinator's
// bounded post-cancel wait ends immediately in tests.
func (f *gatedDispatchAgent) IsSessionBusy(sessionID string) bool { return false }

// cancels returns a copy of the session ids Cancel recorded.
func (f *gatedDispatchAgent) cancels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.canceled...)
}

func (f *gatedDispatchAgent) Model() Model { return f.model }

func (f *gatedDispatchAgent) WaitReady() error { return nil }

func (f *gatedDispatchAgent) injected() []SessionAgentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionAgentCall(nil), f.queued...)
}

// newInjectionEnv builds a dispatch tool environment whose dispatched
// agent blocks on a gate, so the dispatch stays running until the test
// releases it.
func newInjectionEnv(t *testing.T, agent *gatedDispatchAgent) (*coordinator, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)
	reapDispatchRuns(t, c, agent.release)
	c.dispatchAgentBuilder = func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error) {
		return &dispatchedAgent{
			agent:       agent,
			model:       agent.model,
			providerCfg: config.ProviderConfig{ID: "test-provider"},
		}, nil
	}
	return c, env
}

func newGatedDispatchAgent() *gatedDispatchAgent {
	return &gatedDispatchAgent{
		model:    dispatchTestModel(),
		gate:     make(chan struct{}),
		entered:  make(chan struct{}),
		cancelCh: make(chan struct{}),
		result:   &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
	}
}

// The keystone path: a message addressed to a running dispatched agent is
// delivered as its next input with the run's call shaping, and the same
// session refuses cleanly once the run finishes — the refusal tells the
// caller to dispatch a new agent, because task sessions are never
// continuable.
func TestDeliverAgentMessageMidRunThenRefusalAfterFinish(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))

	// The background run is parked inside the agent's Run call — the
	// dispatch is genuinely running and addressable.
	agent.waitRunning(t)

	err := c.DeliverAgentMessage(t.Context(), AgentMessage{
		SessionID: handle.SessionID,
		Text:      "stop writing Rust and use Go",
	})
	require.NoError(t, err, "a running agent must accept the message")

	injected := agent.injected()
	require.Len(t, injected, 1)
	require.Equal(t, handle.SessionID, injected[0].SessionID)
	require.Equal(t, "stop writing Rust and use Go", injected[0].Prompt)
	// The injected call inherits the run's shaping: the dispatched model's
	// token budget and the parent turn's content width, and the
	// non-interactive + no-RunID contract that folds it into the running
	// turn instead of starting an independent lifecycle.
	require.Equal(t, int64(1024), injected[0].MaxOutputTokens)
	require.Equal(t, 80, injected[0].ContentWidth)
	require.True(t, injected[0].NonInteractive)
	require.Empty(t, injected[0].RunID)
	// The injection is a steer (#410): the mark is what separates it from
	// the dispatch's initial prompt on a rebuilt card.
	require.True(t, injected[0].Steer)

	// Finish the run; the injection target is gone with it.
	agent.release()
	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	err = c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: "one more thing"})
	require.ErrorContains(t, err, "finished")
	require.ErrorContains(t, err, "dispatch a new agent")
}

// parkedSessions parks the first AddSessionUsage for one session ID
// until released. runDispatch's cost propagation (#364) applies the
// served dispatch's usage to the parent after the registry entry turns
// terminal, so parking it holds the dispatch inside the window between
// "the registry says finished" and runDispatch returning — where its
// deferred cleanup has not run yet.
type parkedSessions struct {
	session.Service

	mu      sync.Mutex
	id      string
	once    sync.Once
	parked  chan struct{}
	release chan struct{}
}

func newParkedSessions(inner session.Service) *parkedSessions {
	return &parkedSessions{Service: inner, parked: make(chan struct{}), release: make(chan struct{})}
}

func (s *parkedSessions) park(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = id
}

func (s *parkedSessions) AddSessionUsage(ctx context.Context, id string, promptTokens, completionTokens int64, cost float64) error {
	s.mu.Lock()
	park := s.id != "" && id == s.id
	s.mu.Unlock()
	if park {
		s.once.Do(func() {
			close(s.parked)
			<-s.release
		})
	}
	return s.Service.AddSessionUsage(ctx, id, promptTokens, completionTokens, cost)
}

// A delivery that observes the registry entry as terminal must be
// refused, whatever runDispatch still has left to do (#312). The
// dispatch is parked in the cost propagation that follows the terminal
// status, so the delivery lands deterministically in the window a
// deferred-only unregister leaves open: there, a target that is still
// registered takes the message (the fake agent always accepts), which is
// the Windows flake TestDeliverAgentMessageMidRunThenRefusalAfterFinish
// hit when the unregister ran only on return.
func TestDeliverAgentMessageRefusesOnceRegistryIsTerminal(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	sessions := newParkedSessions(c.sessions)
	c.sessions = sessions
	defer close(sessions.release)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)

	entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	sessions.park(entry.ParentSessionID)
	agent.release()
	select {
	case <-sessions.parked:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never reached cost propagation")
	}

	entry, ok = c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusCompleted, entry.Status, "the dispatch is parked after its terminal status")

	err := c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: "one more thing"})
	require.ErrorContains(t, err, "finished")
	require.ErrorContains(t, err, "dispatch a new agent")
	require.Empty(t, agent.injected(), "a finished dispatch must not take the message")
}

// The queue is concurrency-safe with FIFO delivery for sequential sends,
// and no message is lost under concurrent sends (run under -race).
func TestDeliverAgentMessageOrderingAndConcurrency(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)

	for _, text := range []string{"first", "second", "third"} {
		require.NoError(t, c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: text}))
	}
	var (
		wg      sync.WaitGroup
		results = make(chan error, 32)
	)
	for i := range 32 {
		wg.Go(func() {
			results <- c.DeliverAgentMessage(t.Context(), AgentMessage{
				SessionID: handle.SessionID,
				Text:      "burst " + time.Now().Format(time.RFC3339Nano) + string(rune('a'+i%26)),
			})
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}

	injected := agent.injected()
	require.Len(t, injected, 35)
	require.Equal(t, []string{"first", "second", "third"},
		[]string{injected[0].Prompt, injected[1].Prompt, injected[2].Prompt},
		"sequential sends keep their order at the head of the queue")

	agent.release()
}

// A registry entry that is still running but has no injection target —
// the window between the dispatch handle being returned and the
// background run registering itself — must refuse as "no running agent",
// not claim the agent finished with a running status and send the caller
// off to dispatch a duplicate.
func TestDeliverAgentMessageRunningWithoutTargetIsNotFinished(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)

	// Simulate the registration window: the entry is running, the
	// injection target is not registered yet.
	c.unregisterDispatchRun(handle.SessionID)
	err := c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: "hi"})
	require.ErrorContains(t, err, "no running agent for session")
	require.NotContains(t, err.Error(), "finished")

	agent.release()
}

// Unknown sessions and argument validation fail fast with actionable
// errors.
func TestDeliverAgentMessageValidationAndUnknownSession(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)

	err := c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: "", Text: "hi"})
	require.ErrorContains(t, err, "session id is required")

	err = c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: "s"})
	require.ErrorContains(t, err, "message text is required")

	err = c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: "never-dispatched", Text: "hi"})
	require.ErrorContains(t, err, "no running agent for session never-dispatched")
}

// The model-facing front door: the message_agent tool delivers through the
// same queue, reports the delivery contract on success, and returns the
// clean refusal as a tool error rather than a failed call.
func TestMessageAgentToolDeliversAndRefuses(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)
	messageTool := c.messageAgentTool()

	resp := runToolAsSession(t, messageTool, MessageAgentToolName, MessageAgentParams{
		SessionID: handle.SessionID,
		Message:   "please add tests",
	}, "dispatch-parent-session")
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	require.Contains(t, resp.Content, handle.SessionID)
	require.Contains(t, resp.Content, "next input")
	require.Len(t, agent.injected(), 1)

	agent.release()
	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	resp = runToolAsSession(t, messageTool, MessageAgentToolName, MessageAgentParams{
		SessionID: handle.SessionID,
		Message:   "too late",
	}, "dispatch-parent-session")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "dispatch a new agent")
	require.Len(t, agent.injected(), 1, "a completed dispatch must never enqueue a message")

	resp = runTool(t, messageTool, MessageAgentToolName, MessageAgentParams{Message: "no session"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "provide exactly one of handle or session_id")
}

// The model sees exactly one required field on message_agent (#400):
// providers that enforce required must not be pushed to invent a
// session_id when the model knows only the @handle the user typed.
func TestMessageAgentToolSchemaRequiresOnlyMessage(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)

	require.Equal(t, []string{"message"}, c.messageAgentTool().Info().Required)
}

// The four input shapes of the message_agent tool (#400) against a
// coordinator with one running fake dispatch: exactly one of handle or
// session_id delivers, neither is a tool error naming both fields, and a
// pair that names different agents is refused — while a matching pair
// names the same agent and delivers once.
func TestMessageAgentToolInputShapes(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main", Handle: "tester"}))
	agent.waitRunning(t)
	messageTool := c.messageAgentTool()

	cases := []struct {
		name     string
		params   MessageAgentParams
		wantErr  string
		injected int
	}{
		{
			name:     "handle only delivers",
			params:   MessageAgentParams{Handle: "tester", Message: "by handle"},
			injected: 1,
		},
		{
			name:     "session id only delivers",
			params:   MessageAgentParams{SessionID: handle.SessionID, Message: "by session"},
			injected: 2,
		},
		{
			name:     "neither is a tool error naming both fields",
			params:   MessageAgentParams{Message: "by nothing"},
			wantErr:  "provide exactly one of handle or session_id",
			injected: 2,
		},
		{
			name:     "both naming different agents is a tool error",
			params:   MessageAgentParams{Handle: "tester", SessionID: "another-session", Message: "contradictory"},
			wantErr:  "name different agents",
			injected: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := runToolAsSession(t, messageTool, MessageAgentToolName, tc.params, "dispatch-parent-session")
			if tc.wantErr != "" {
				require.True(t, resp.IsError)
				require.Contains(t, resp.Content, tc.wantErr)
			} else {
				require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
			}
			require.Len(t, agent.injected(), tc.injected)
		})
	}

	// A matching pair names the same agent and delivers once.
	resp := runToolAsSession(t, messageTool, MessageAgentToolName, MessageAgentParams{
		Handle:    "tester",
		SessionID: handle.SessionID,
		Message:   "same agent",
	}, "dispatch-parent-session")
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	require.Len(t, agent.injected(), 3)
}

// refusingDispatchAgent is a fake dispatched agent that mirrors the real
// sessionAgent.EnqueueWhenBusy busy-window contract (#426). The existing
// gatedDispatchAgent accepts every enqueue, so it can never reach
// DeliverAgentMessage's refusal branch — the window where the injection
// target is still registered but the run no longer takes messages. This
// fake refuses until Run passes startGate (the moment the real run sets
// the session busy), accepts only while the run is active, and refuses
// again once Run returns.
//
// alwaysRefuse short-circuits EnqueueWhenBusy to false for the whole run:
// the dispatch stays running (the target stays registered) while every
// enqueue is refused, which is exactly the "the run ended between the
// registry lookup and the enqueue" race the refusal branch exists to
// catch.
type refusingDispatchAgent struct {
	SessionAgent
	model  Model
	result *fantasy.AgentResult

	startGate    chan struct{}
	gate         chan struct{}
	entered      chan struct{}
	busyCh       chan struct{}
	alwaysRefuse bool

	startOnce sync.Once
	gateOnce  sync.Once
	busyOnce  sync.Once
	enterOnce sync.Once
	mu        sync.Mutex
	busy      bool
	queued    []SessionAgentCall
	lastCall  *SessionAgentCall
	canceled  []string

	cancelCh   chan struct{}
	cancelOnce sync.Once
}

func newRefusingDispatchAgent() *refusingDispatchAgent {
	return &refusingDispatchAgent{
		model:     dispatchTestModel(),
		startGate: make(chan struct{}),
		gate:      make(chan struct{}),
		entered:   make(chan struct{}),
		busyCh:    make(chan struct{}),
		cancelCh:  make(chan struct{}),
		result: &fantasy.AgentResult{
			Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}},
		},
	}
}

// newAlwaysRefusingDispatchAgent builds a refusingDispatchAgent whose
// EnqueueWhenBusy refuses for the whole run, whatever its busy state.
func newAlwaysRefusingDispatchAgent() *refusingDispatchAgent {
	f := newRefusingDispatchAgent()
	f.alwaysRefuse = true
	return f
}

// Run models the real run's busy window: it signals entry, then parks in
// the startup window (registered, not yet busy) until startGate passes;
// passing startGate marks the session busy (models activeRequests.Set);
// it then parks in the active run until gate is released, and returns
// with the session no longer busy.
func (f *refusingDispatchAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	callCopy := call
	f.lastCall = &callCopy
	f.mu.Unlock()
	f.enterOnce.Do(func() { close(f.entered) })
	select {
	case <-f.startGate:
	case <-f.cancelCh:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f.busyOnce.Do(func() {
		f.mu.Lock()
		f.busy = true
		f.mu.Unlock()
		close(f.busyCh)
	})
	select {
	case <-f.gate:
	case <-f.cancelCh:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
	return f.result, nil
}

// EnqueueWhenBusy is the delivery primitive under test (#426): it records
// a call and reports true only while the run is busy — and never at all
// when alwaysRefuse is set. Recording only on the accept path is what
// pins the "queued exactly once" invariant.
func (f *refusingDispatchAgent) EnqueueWhenBusy(call SessionAgentCall) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alwaysRefuse || !f.busy {
		return false
	}
	f.queued = append(f.queued, call)
	return true
}

// IsSessionBusy reports the run's busy state, so the coordinator's
// post-cancel wait and the tests' busy assertions see the model.
func (f *refusingDispatchAgent) IsSessionBusy(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

// Cancel records the cancel and un-parks a Run parked on a gate,
// mirroring a real agent whose active request aborts on Cancel.
func (f *refusingDispatchAgent) Cancel(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, sessionID)
	f.cancelOnce.Do(func() { close(f.cancelCh) })
}

func (f *refusingDispatchAgent) Model() Model     { return f.model }
func (f *refusingDispatchAgent) WaitReady() error { return nil }

// waitRunning blocks until the dispatched run has entered Run, which is
// after runDispatch registered the injection target.
func (f *refusingDispatchAgent) waitRunning(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch run never started")
	}
}

// waitBusy blocks until Run has passed startGate and marked the session
// busy — the point after which EnqueueWhenBusy accepts.
func (f *refusingDispatchAgent) waitBusy(t *testing.T) {
	t.Helper()
	select {
	case <-f.busyCh:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch run never became busy")
	}
}

// releaseStart opens startGate so a run parked in the startup window
// marks the session busy.
func (f *refusingDispatchAgent) releaseStart() {
	f.startOnce.Do(func() { close(f.startGate) })
}

// release opens the run gate so a parked run can finish.
func (f *refusingDispatchAgent) release() {
	f.gateOnce.Do(func() { close(f.gate) })
}

// releaseAll opens every gate, for cleanup: a run parked on either the
// start gate or the run gate is unblocked.
func (f *refusingDispatchAgent) releaseAll() {
	f.releaseStart()
	f.release()
}

// injected returns a copy of the calls EnqueueWhenBusy accepted.
func (f *refusingDispatchAgent) injected() []SessionAgentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionAgentCall(nil), f.queued...)
}

// newRefusingInjectionEnv is newInjectionEnv for a refusingDispatchAgent:
// the dispatch stays running until the test releases its gates.
func newRefusingInjectionEnv(t *testing.T, agent *refusingDispatchAgent) (*coordinator, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)
	reapDispatchRuns(t, c, agent.releaseAll)
	c.dispatchAgentBuilder = func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error) {
		return &dispatchedAgent{
			agent:       agent,
			model:       agent.model,
			providerCfg: config.ProviderConfig{ID: "test-provider"},
		}, nil
	}
	return c, env
}

// TestDeliverAgentMessageRefusalBranch covers DeliverAgentMessage's
// refusal branch (#426): the injection target is still registered — the
// dispatch is running — but the agent refuses the enqueue, the "the run
// ended between the registry lookup and the enqueue" race. The delivery
// must error, naming the session, and queue nothing; the message_agent
// tool surfaces the same refusal as a tool error, never a "delivered"
// success.
func TestDeliverAgentMessageRefusalBranch(t *testing.T) {
	agent := newAlwaysRefusingDispatchAgent()
	c, _ := newRefusingInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)

	// The target is registered (the dispatch is running) yet the agent
	// refuses every enqueue: DeliverAgentMessage must take the refusal
	// branch, naming the session, and queue nothing.
	err := c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: "steer me"})
	require.Error(t, err)
	require.ErrorContains(t, err, handle.SessionID)
	// Invariant: DeliverAgentMessage returns nil if and only if the
	// message was queued. It erred, so nothing is queued.
	require.Empty(t, agent.injected(), "a refused delivery must not queue the message")

	// The model-facing front door surfaces the same refusal as a tool
	// error, not a "delivered" success.
	resp := runToolAsSession(t, c.messageAgentTool(), MessageAgentToolName, MessageAgentParams{
		SessionID: handle.SessionID,
		Message:   "steer me",
	}, "dispatch-parent-session")
	require.True(t, resp.IsError, "the refusal must surface as a tool error, got: %s", resp.Content)
	require.NotContains(t, resp.Content, "Message delivered")
	require.Empty(t, agent.injected(), "the tool's refused delivery must not queue the message")
}

// TestDeliverAgentMessageStartupWindow covers the startup window (#426):
// the injection target is registered before Run marks the session busy,
// so a delivery that lands before the run is busy is refused (nothing
// queued), and one that lands after is accepted (queued exactly once).
// Synchronization is by gates only — startGate models the moment Run sets
// the session busy, busyCh signals it — no sleeps.
func TestDeliverAgentMessageStartupWindow(t *testing.T) {
	agent := newRefusingDispatchAgent()
	c, _ := newRefusingInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)
	// Run is entered but has not passed startGate: the injection target
	// is registered, yet the session is not busy.
	require.False(t, agent.IsSessionBusy(handle.SessionID), "the run is parked before it is busy")

	// deliver pins the #426 invariant at every step: DeliverAgentMessage
	// returns nil if and only if the message was queued (and exactly one
	// copy of it).
	deliver := func(text string) error {
		before := len(agent.injected())
		err := c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: text})
		after := len(agent.injected())
		if err == nil {
			require.Equal(t, before+1, after, "a nil error must mean exactly one queued message")
		} else {
			require.Equal(t, before, after, "an error must mean nothing was queued")
		}
		return err
	}

	// Before the run is busy: refused, nothing queued. The branch's
	// current wording ("no longer running; dispatch a new agent instead")
	// is misleading here — the agent is starting, not finished — so this
	// test pins the behavior, not the wording.
	require.Error(t, deliver("early steer"))
	require.Empty(t, agent.injected(), "a pre-busy delivery must not queue")

	// Release startGate: Run marks the session busy and parks in the
	// active run.
	agent.releaseStart()
	agent.waitBusy(t)
	require.True(t, agent.IsSessionBusy(handle.SessionID), "the run is busy after startGate")

	// Now busy: accepted, queued exactly once.
	require.NoError(t, deliver("on-time steer"))
	injected := agent.injected()
	require.Len(t, injected, 1)
	require.Equal(t, "on-time steer", injected[0].Prompt)
}
