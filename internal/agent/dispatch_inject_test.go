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

	enterOnce sync.Once
	mu        sync.Mutex
	queued    []SessionAgentCall
}

func (f *gatedDispatchAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	f.enterOnce.Do(func() { close(f.entered) })
	select {
	case <-f.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.result, nil
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

func (f *gatedDispatchAgent) EnqueueWhenBusy(call SessionAgentCall) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, call)
	return true
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
		model:   dispatchTestModel(),
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
		result:  &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}},
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

	// Finish the run; the injection target is gone with it.
	close(agent.gate)
	require.Eventually(t, func() bool {
		ws, _ := c.dispatchWorkspace()
		entry, ok := ws.Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	err = c.DeliverAgentMessage(t.Context(), AgentMessage{SessionID: handle.SessionID, Text: "one more thing"})
	require.ErrorContains(t, err, "finished")
	require.ErrorContains(t, err, "dispatch a new agent")
}

// parkedSessions parks the first Get of one session ID until released.
// runDispatch's first read of the dispatched session is its cost
// propagation, which runs after the registry entry turns terminal, so
// parking it holds the dispatch inside the window between "the registry
// says finished" and runDispatch returning — where its deferred cleanup
// has not run yet.
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

func (s *parkedSessions) Get(ctx context.Context, id string) (session.Session, error) {
	s.mu.Lock()
	park := s.id != "" && id == s.id
	s.mu.Unlock()
	if park {
		s.once.Do(func() {
			close(s.parked)
			<-s.release
		})
	}
	return s.Service.Get(ctx, id)
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

	sessions.park(handle.SessionID)
	close(agent.gate)
	select {
	case <-sessions.parked:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never reached cost propagation")
	}

	ws, _ := c.dispatchWorkspace()
	entry, ok := ws.Get(handle.DispatchID)
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

	close(agent.gate)
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

	close(agent.gate)
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

	resp := runTool(t, messageTool, MessageAgentToolName, MessageAgentParams{
		SessionID: handle.SessionID,
		Message:   "please add tests",
	})
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	require.Contains(t, resp.Content, handle.SessionID)
	require.Contains(t, resp.Content, "next input")
	require.Len(t, agent.injected(), 1)

	close(agent.gate)
	require.Eventually(t, func() bool {
		ws, _ := c.dispatchWorkspace()
		entry, ok := ws.Get(handle.DispatchID)
		return ok && entry.Status == dispatch.StatusCompleted
	}, 10*time.Second, 50*time.Millisecond)

	resp = runTool(t, messageTool, MessageAgentToolName, MessageAgentParams{
		SessionID: handle.SessionID,
		Message:   "too late",
	})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "dispatch a new agent")

	resp = runTool(t, messageTool, MessageAgentToolName, MessageAgentParams{Message: "no session"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "session id is required")
}
