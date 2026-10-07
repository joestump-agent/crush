package agent

// Recovery-path regressions for the sub-agent A2A turn (#392): when the
// stream breaks before a terminal state, every direct-cancel fallback
// must name the run's own task session — the served run lives there,
// not on the parent session.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// breakingStreamHost is a canned host whose stream always breaks before
// a terminal state, optionally after naming a task, so the recovery
// branches are reachable without a real server.
type breakingStreamHost struct {
	mu      sync.Mutex
	taskID  string
	started DispatchServerParams
	stopped int
}

func (h *breakingStreamHost) StartDispatchServer(_ context.Context, params DispatchServerParams) (string, any, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = params
	return "http://127.0.0.1:19999", "canned-card", func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.stopped++
	}, nil
}

func (h *breakingStreamHost) StreamDispatch(_ context.Context, params DispatchTransportParams) (DispatchTransportOutcome, error) {
	if h.taskID != "" && params.OnTask != nil {
		params.OnTask(h.taskID)
	}
	return DispatchTransportOutcome{}, errors.New("stream broke")
}

func (h *breakingStreamHost) servedParams() DispatchServerParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started
}

// failingCancelHost is a breakingStreamHost whose tasks/cancel fails and
// whose tasks/get answers with a canned terminal status, covering the
// branch where the protocol cancel cannot be delivered.
type failingCancelHost struct {
	breakingStreamHost
	cancelErr  error
	taskStatus DispatchTaskStatus
}

func (h *failingCancelHost) CancelDispatch(_ context.Context, _ DispatchCancelParams) error {
	return h.cancelErr
}

func (h *failingCancelHost) GetDispatchTask(_ context.Context, _ GetDispatchTaskParams) (DispatchTaskStatus, error) {
	return h.taskStatus, nil
}

func newSubAgentRecoveryTest(t *testing.T, host DispatchHost) (*coordinator, session.Service, string) {
	t.Helper()
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)
	c.SetDispatchHost(host)

	const providerID = "test-provider"
	c.cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	return c, env.sessions, parent.ID
}

func runRecoveryTurn(t *testing.T, c *coordinator, parentID string) fantasy.ToolResponse {
	t.Helper()
	mock := newMockAgent("test-provider", 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("unreachable"), nil
	})
	resp, err := c.runSubAgentOverA2A(t.Context(), subAgentParams{
		Agent:            mock,
		SessionID:        parentID,
		AgentMessageID:   "msg-1",
		ToolCallID:       "call-1",
		Prompt:           "look into it",
		SessionTitle:     "Fetch Analysis",
		AgentName:        "Fetch",
		AgentDescription: "Fetches and analyzes web content and search results.",
	})
	require.NoError(t, err)
	return resp
}

// No canceler on the host seam and no task named: the direct cancel is
// the only way the run ends, so it must name the run's task session.
func TestSubAgentA2ARecoveryCancelsRunSession(t *testing.T) {
	host := &breakingStreamHost{}
	c, sessions, parentID := newSubAgentRecoveryTest(t, host)

	resp := runRecoveryTurn(t, c, parentID)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "stream broke")

	children, err := sessions.ListChildren(t.Context(), parentID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	runSessionID := host.servedParams().SessionID
	require.Equal(t, children[0].ID, runSessionID, "the turn is served on its task session")
	require.NotEqual(t, parentID, runSessionID, "the run session is not the parent session")

	mock := host.servedParams().Runner
	cancels := SubAgentMockCancellations(mock)
	require.Equal(t, []string{runSessionID}, cancels,
		"the fallback cancel names the run's task session, not the parent session")
}

// The protocol cancel fails: the direct cancel still names the run's
// task session, and the task's real terminal state round-trips to the
// caller through tasks/get.
func TestSubAgentA2ARecoveryFailingCancelFallsBackToDirect(t *testing.T) {
	host := &failingCancelHost{
		breakingStreamHost: breakingStreamHost{taskID: "task-1"},
		cancelErr:          errors.New("cancel unreachable"),
		taskStatus:         DispatchTaskStatus{Status: transportStatusCanceled, Text: "parent turn canceled"},
	}
	c, _, parentID := newSubAgentRecoveryTest(t, host)

	resp := runRecoveryTurn(t, c, parentID)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "canceled")
	require.Contains(t, resp.Content, "parent turn canceled")

	runSessionID := host.servedParams().SessionID
	require.NotEqual(t, parentID, runSessionID)
	mock := host.servedParams().Runner
	cancels := SubAgentMockCancellations(mock)
	require.Equal(t, []string{runSessionID}, cancels,
		"the direct fallback cancel names the run's task session, not the parent session")
}
