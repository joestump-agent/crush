package agent_test

// End-to-end sub-agent tests over the real A2A runtime (#392): a real
// a2a.ServerFactory serves the sub-agent's turn — one card, one context
// (the task session, per #350), one task — the coordinator drives it
// with the A2A client, and the shared finishSubAgent contract maps the
// terminal state onto the tool response. This is the composition
// production wires for the agentic_fetch entry point; every assertion
// here is the visible behavior a later entry-point PR inherits.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/agent/agenttest"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// subAgentTestProvider is the offline provider agenttest.NewCoordinator
// registers; the sub-agent mocks name it so prepareSubAgent resolves
// their model config through the real provider lookup.
const subAgentTestProvider = "test-openai-compat"

// newSubAgentCoordinator builds a real production coordinator over
// DB-backed stores, wired to a real a2a.ServerFactory host — the
// composition internal/app assembles, minus the TUI. The factory is
// returned so tests can read the host's own surfaces (card listing).
func newSubAgentCoordinator(t *testing.T) (agent.Coordinator, session.Service, *a2a.ServerFactory) {
	t.Helper()
	factory := a2a.NewServerFactory(t.TempDir())
	co, sessions := newSubAgentCoordinatorOn(t, factory)
	return co, sessions, factory
}

// newSubAgentCoordinatorOn is newSubAgentCoordinator over a given host.
func newSubAgentCoordinatorOn(t *testing.T, host agent.DispatchHost) (agent.Coordinator, session.Service) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	co, err := agenttest.NewCoordinator(t.Context(), t.TempDir(), sessions, messages,
		agenttest.WithDispatchHost(host))
	require.NoError(t, err)
	return co, sessions
}

// taskSeenHost is the real host, closing taskSeen once the client has
// read the served task's ID off the stream: from then on a client cancel
// can recover the run through tasks/cancel.
type taskSeenHost struct {
	*a2a.ServerFactory
	taskSeen chan struct{}
	once     sync.Once
}

func (h *taskSeenHost) StreamDispatch(ctx context.Context, p agent.DispatchTransportParams) (agent.DispatchTransportOutcome, error) {
	onTask := p.OnTask
	p.OnTask = func(id string) {
		if onTask != nil {
			onTask(id)
		}
		h.once.Do(func() { close(h.taskSeen) })
	}
	return h.ServerFactory.StreamDispatch(ctx, p)
}

// subAgentResult is the minimal successful AgentResult a mock returns.
func subAgentResult(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: text}},
		},
	}
}

// fetchParams is the sub-agent turn the agentic_fetch tool builds.
func fetchParams(parentID string, mock agent.SessionAgent) agent.SubAgentParams {
	return agent.SubAgentParams{
		Agent:            mock,
		SessionID:        parentID,
		AgentMessageID:   "msg-1",
		ToolCallID:       "call-1",
		Prompt:           "look into it",
		SessionTitle:     "Fetch Analysis",
		AgentName:        "Fetch",
		AgentDescription: "Fetches and analyzes web content and search results.",
	}
}

// A completed sub-agent turn: the agent runs exactly once, served —
// non-interactive, the task echoed back as RunID — against its own child
// task session under the parent, and the terminal text is the tool
// response.
func TestSubAgentOverA2ACompleted(t *testing.T) {
	t.Parallel()

	co, sessions, _ := newSubAgentCoordinator(t)
	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	var (
		mu      sync.Mutex
		gotCall agent.SessionAgentCall
		runs    atomic.Int32
	)
	mock := agent.NewSubAgentMock(subAgentTestProvider, 4096, func(_ context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
		runs.Add(1)
		mu.Lock()
		gotCall = call
		mu.Unlock()
		return subAgentResult("done"), nil
	})

	resp, err := agent.RunSubAgentOverA2A(co, t.Context(), fetchParams(parent.ID, mock))
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "done", resp.Content)
	require.Equal(t, int32(1), runs.Load(), "the served turn ran the agent exactly once")

	mu.Lock()
	call := gotCall
	mu.Unlock()
	require.True(t, call.NonInteractive, "served sub-agent turns are non-interactive")
	require.Equal(t, "look into it", call.Prompt, "the served turn runs with the caller's prompt")
	require.NotEmpty(t, call.RunID, "the run echoes the A2A task id as RunID")

	children, err := sessions.ListChildren(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1, "the turn runs against its own child task session")
	require.Equal(t, "Fetch Analysis", children[0].Title)
}

// A provider failure maps through the served Failed status onto the
// shared error response — the same text the direct path produced, so a
// caller cannot tell which runtime ran the turn.
func TestSubAgentOverA2AProviderError(t *testing.T) {
	t.Parallel()

	co, sessions, _ := newSubAgentCoordinator(t)
	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	mock := agent.NewSubAgentMock(subAgentTestProvider, 4096, func(_ context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
		return nil, errors.New("boom from provider")
	})

	resp, err := agent.RunSubAgentOverA2A(co, t.Context(), fetchParams(parent.ID, mock))
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Failed to generate response")
	require.Contains(t, resp.Content, "boom from provider", "the served reason reaches the caller")
}

// A client-side cancel mid-turn recovers the orphaned served run through
// the protocol path (#348): tasks/cancel carries the parent's reason,
// tasks/get observes exactly one terminal Canceled — whose reason the
// shared finish maps onto the error response — and the served runner saw
// the cancel exactly once.
func TestSubAgentOverA2ACancelRecoversThroughProtocol(t *testing.T) {
	t.Parallel()

	host := &taskSeenHost{ServerFactory: a2a.NewServerFactory(t.TempDir()), taskSeen: make(chan struct{})}
	co, sessions := newSubAgentCoordinatorOn(t, host)
	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	mock := agent.NewSubAgentMock(subAgentTestProvider, 4096, func(_ context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
		runs.Add(1)
		close(entered)
		<-release
		return subAgentResult("unreachable"), nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		resp fantasy.ToolResponse
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		resp, err := agent.RunSubAgentOverA2A(co, ctx, fetchParams(parent.ID, mock))
		ch <- outcome{resp, err}
	}()

	<-entered
	// Cut the client turn only once the client has the task's ID, so
	// recovery can name it: the server emitting it is not enough, as the
	// stream may not have reached the client yet.
	select {
	case <-host.taskSeen:
	case <-time.After(30 * time.Second):
		t.Fatal("the client never read the served task's ID")
	}
	cancel()

	res := <-ch
	require.NoError(t, res.err)
	require.True(t, res.resp.IsError)
	require.Contains(t, res.resp.Content, "canceled")
	// The reason only round-trips through tasks/cancel → tasks/get: the
	// recovery observed the served task's single terminal Canceled.
	require.Contains(t, res.resp.Content, "parent turn canceled")
	require.NotEmpty(t, agent.SubAgentMockCancellations(mock), "the served runner saw the cancel")
	close(release)
	require.Equal(t, int32(1), runs.Load(), "the served run started exactly once")
}

// Building the coordinator publishes one stable card per non-disabled
// agent definition on the wired host (#392): the listing exists before
// any entry point routes its turns through the runtime.
func TestCoordinatorPublishesAgentDefinitionCards(t *testing.T) {
	t.Parallel()

	co, _, factory := newSubAgentCoordinator(t)
	_ = co

	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	definitions := cfg.Config().Agents
	require.NotEmpty(t, definitions)

	cards := factory.AgentCards()
	byName := make(map[string]string, len(cards))
	for _, card := range cards {
		byName[card.Name] = card.Description
	}
	for id, ag := range definitions {
		require.NotEmpty(t, byName[ag.Name], "definition %s (%q) must be published", id, ag.Name)
	}
}
