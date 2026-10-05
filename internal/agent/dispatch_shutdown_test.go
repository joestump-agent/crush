package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/stretchr/testify/require"
)

// shutdownDispatchAgent is a dispatched-agent fake whose Run blocks until
// one of its exits fires: its Cancel (unless ignoreCancel), its run
// context (unless ignoreContext), or unblockCh. It models the dispatched
// agents shutdown has to stop (#372): well-behaved ones honor Cancel,
// stubborn ones only the root cancel, and wedged ones neither.
type shutdownDispatchAgent struct {
	SessionAgent
	model   Model
	started chan struct{}

	cancelCh      chan struct{}
	ignoreCancel  bool
	ignoreContext bool
	unblockCh     chan struct{}

	cancelOnce  sync.Once
	startOnce   sync.Once
	startedOnce chan struct{}
}

func newShutdownDispatchAgent(ignoreCancel, ignoreContext bool) *shutdownDispatchAgent {
	return &shutdownDispatchAgent{
		model:         dispatchTestModel(),
		started:       make(chan struct{}),
		startedOnce:   make(chan struct{}),
		cancelCh:      make(chan struct{}),
		ignoreCancel:  ignoreCancel,
		ignoreContext: ignoreContext,
		unblockCh:     make(chan struct{}),
	}
}

func (g *shutdownDispatchAgent) Run(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
	g.startOnce.Do(func() { close(g.started) })
	select {
	case <-g.cancelCh:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.unblockCh:
		return nil, context.Canceled
	}
}

func (g *shutdownDispatchAgent) Cancel(string) {
	if g.ignoreCancel {
		return
	}
	g.cancelOnce.Do(func() { close(g.cancelCh) })
}

func (g *shutdownDispatchAgent) Model() Model     { return g.model }
func (g *shutdownDispatchAgent) WaitReady() error { return nil }

func (g *shutdownDispatchAgent) hasStarted() bool {
	select {
	case <-g.started:
		return true
	default:
		return false
	}
}

// shutdownMainAgent is a fake main agent that records delivery turns and
// CancelAll calls: the shutdown tests use it to prove a dispatch
// finishing during shutdown records its terminal state but starts no
// parent delivery turn, while the main agent itself is still canceled.
type shutdownMainAgent struct {
	SessionAgent
	model      Model
	mu         sync.Mutex
	runs       []SessionAgentCall
	cancelAlls int
}

func (m *shutdownMainAgent) Run(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	m.mu.Lock()
	m.runs = append(m.runs, call)
	m.mu.Unlock()
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "ack"}}}}, nil
}

func (m *shutdownMainAgent) Model() Model     { return m.model }
func (m *shutdownMainAgent) WaitReady() error { return nil }
func (m *shutdownMainAgent) CancelAll() {
	m.mu.Lock()
	m.cancelAlls++
	m.mu.Unlock()
}

func (m *shutdownMainAgent) runCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.runs)
}

func (m *shutdownMainAgent) cancelAllCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelAlls
}

// newShutdownTestCoordinator builds a git-backed dispatch coordinator
// with main installed as the main agent, so the delivery path would run
// a parent turn if the shutdown flag did not stop it.
func newShutdownTestCoordinator(t *testing.T, main *shutdownMainAgent) (*coordinator, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)
	if main != nil {
		c.agents = map[string]SessionAgent{config.AgentCoder: main}
		c.mainAgent = main
		c.mainAgentName = config.AgentCoder
	}
	return c, env
}

// setGatedBuilder swaps the dispatched-agent builder to hand out the
// given fakes in order, one per dispatch.
func setGatedBuilder(c *coordinator, agents ...*shutdownDispatchAgent) {
	var next atomic.Int32
	c.dispatchAgentBuilder = func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error) {
		a := agents[int(next.Add(1))-1]
		return &dispatchedAgent{agent: a, model: a.model, providerCfg: config.ProviderConfig{ID: "test-provider"}}, nil
	}
}

// runShutdownDispatchCall invokes the DispatchAgent tool with a full
// tool-call context, like runDispatchToolCall, with an explicit call ID
// and parent session so a test can dispatch more than once.
func runShutdownDispatchCall(t *testing.T, tool fantasy.AgentTool, callID, parentSessionID string, params DispatchAgentParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, parentSessionID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "shutdown-parent-message")
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: DispatchAgentToolName, Input: string(input)})
	require.NoError(t, err)
	return resp
}

// Two live dispatches at shutdown are both killed with the shutdown
// reason, their live records are gone, and neither delivers a terminal
// turn to the parent (#372).
func TestShutdownCancelAllKillsLiveDispatches(t *testing.T) {
	t.Parallel()
	main := &shutdownMainAgent{model: dispatchTestModel()}
	c, env := newShutdownTestCoordinator(t, main)

	first := newShutdownDispatchAgent(false, false)
	second := newShutdownDispatchAgent(false, false)
	setGatedBuilder(c, first, second)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	tool := c.dispatchTool()
	var handles []dispatch.DispatchResult
	for i, callID := range []string{"shutdown-call-1", "shutdown-call-2"} {
		params := DispatchAgentParams{Prompt: "do work"}
		if i == 1 {
			params.Prompt = "do more work"
		}
		handles = append(handles, decodeDispatchHandle(t, runShutdownDispatchCall(t, tool, callID, parent.ID, params)))
	}

	require.Eventually(t, func() bool {
		return first.hasStarted() && second.hasStarted()
	}, 10*time.Second, 10*time.Millisecond)

	c.CancelAll()

	for _, h := range handles {
		entry, ok := c.dispatchRegistry().Get(h.DispatchID)
		require.True(t, ok)
		require.Equal(t, dispatch.StatusKilled, entry.Status)
		require.NotNil(t, entry.Result)
		require.Equal(t, dispatch.ReasonShutdown, entry.Result.KilledReason)
		require.Equal(t, "crush exited", entry.Result.KilledReason)
	}

	c.dispatchMu.Lock()
	require.Empty(t, c.liveDispatches)
	c.dispatchMu.Unlock()

	require.Equal(t, 0, main.runCount(), "no parent delivery turn may start during shutdown")
	require.Equal(t, 1, main.cancelAllCount(), "the main agent is canceled exactly once, as before")
}

// A dispatch whose agent ignores Cancel but honors its context is ended
// by the root-cancel fallback after the graceful bound expires (#372).
func TestShutdownRootCancelFallbackEndsStubbornDispatch(t *testing.T) {
	t.Parallel()
	main := &shutdownMainAgent{model: dispatchTestModel()}
	c, env := newShutdownTestCoordinator(t, main)
	c.dispatchShutdownWait = 200 * time.Millisecond
	c.dispatchShutdownRootWait = 5 * time.Second

	agent := newShutdownDispatchAgent(true, false)
	setGatedBuilder(c, agent)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	handle := decodeDispatchHandle(t, runShutdownDispatchCall(t, c.dispatchTool(), "shutdown-call-stubborn", parent.ID, DispatchAgentParams{Prompt: "do work"}))
	require.Eventually(t, func() bool { return agent.hasStarted() }, 10*time.Second, 10*time.Millisecond)

	start := time.Now()
	c.CancelAll()
	require.Less(t, time.Since(start), 4*time.Second, "CancelAll must return within its bounds")

	entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusKilled, entry.Status)
	require.NotNil(t, entry.Result)
	require.Equal(t, dispatch.ReasonShutdown, entry.Result.KilledReason)

	c.dispatchMu.Lock()
	require.Empty(t, c.liveDispatches)
	c.dispatchMu.Unlock()
}

// A dispatch whose agent ignores both Cancel and its context cannot
// stop CancelAll: the phase waits out its shortened bounds and returns,
// logging the unfinished dispatch (#372).
func TestShutdownBoundExpiresStillReturns(t *testing.T) {
	t.Parallel()
	main := &shutdownMainAgent{model: dispatchTestModel()}
	c, env := newShutdownTestCoordinator(t, main)
	c.dispatchShutdownWait = 100 * time.Millisecond
	c.dispatchShutdownRootWait = 100 * time.Millisecond

	agent := newShutdownDispatchAgent(true, true)
	setGatedBuilder(c, agent)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	handle := decodeDispatchHandle(t, runShutdownDispatchCall(t, c.dispatchTool(), "shutdown-call-wedged", parent.ID, DispatchAgentParams{Prompt: "do work"}))
	require.Eventually(t, func() bool { return agent.hasStarted() }, 10*time.Second, 10*time.Millisecond)

	start := time.Now()
	c.CancelAll()
	require.Less(t, time.Since(start), 4*time.Second, "CancelAll must return even when a dispatch ignores every cancel")

	// The wedged run is still going; unblock it so the test's goroutine
	// finishes and records its (killed) terminal state.
	close(agent.unblockCh)
	require.Eventually(t, func() bool {
		c.dispatchMu.Lock()
		defer c.dispatchMu.Unlock()
		return len(c.liveDispatches) == 0
	}, 10*time.Second, 10*time.Millisecond)
	entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
	require.True(t, ok)
	require.Equal(t, dispatch.StatusKilled, entry.Status)
	require.NotNil(t, entry.Result)
	require.Equal(t, dispatch.ReasonShutdown, entry.Result.KilledReason)
}

// With no dispatches running, CancelAll behaves exactly as before: it
// cancels the main agent and returns immediately (#372).
func TestShutdownCancelAllWithoutDispatches(t *testing.T) {
	t.Parallel()
	main := &shutdownMainAgent{model: dispatchTestModel()}
	c, _ := newShutdownTestCoordinator(t, main)

	start := time.Now()
	c.CancelAll()
	require.Less(t, time.Since(start), time.Second)
	require.Equal(t, 1, main.cancelAllCount())

	c.dispatchMu.Lock()
	require.Empty(t, c.liveDispatches)
	c.dispatchMu.Unlock()
}

// While the coordinator is shutting down, a dispatch result delivery
// starts no parent turn; it logs and returns (#372). The positive path
// (delivery when not shutting down) is covered by
// TestDeliverDispatchResultToParentSession.
func TestDeliverDispatchResultSkippedWhileShuttingDown(t *testing.T) {
	t.Parallel()
	main := &shutdownMainAgent{model: dispatchTestModel()}
	c, env := newShutdownTestCoordinator(t, main)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	c.shuttingDown.Store(true)
	c.deliverDispatchResult(t.Context(), parent.ID, dispatch.DispatchResult{
		DispatchID:  "d-shutdown",
		Branch:      "crush-dispatch-d-shutdown",
		SessionID:   "s-shutdown",
		Status:      dispatch.StatusCompleted,
		KeyFindings: "fixed the bug",
	})

	require.Never(t, func() bool { return main.runCount() > 0 }, 500*time.Millisecond, 25*time.Millisecond)
}
