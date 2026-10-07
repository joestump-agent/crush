package agent

// Tool wiring for the agentic_fetch entry point (#392): the fetch tool
// serves its sub-agent turn on the wired A2A host and drives it with the
// A2A client — there is no direct in-process Run left on this path. The
// host here is a canned fake, so only the wiring (params, call shaping,
// response mapping) is under test; the real server + client composition
// is covered by subagent_a2a_e2e_test.go.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// cannedA2AHost records every served turn and streamed drive, answering
// the stream with one canned terminal outcome.
type cannedA2AHost struct {
	mu      sync.Mutex
	started []DispatchServerParams
	streams []DispatchTransportParams
	outcome DispatchTransportOutcome
	stopped int
}

func (h *cannedA2AHost) StartDispatchServer(_ context.Context, params DispatchServerParams) (string, any, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = append(h.started, params)
	return "http://127.0.0.1:19998", "canned-card", func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.stopped++
	}, nil
}

func (h *cannedA2AHost) StreamDispatch(_ context.Context, params DispatchTransportParams) (DispatchTransportOutcome, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.streams = append(h.streams, params)
	return h.outcome, nil
}

func (h *cannedA2AHost) lastStarted() DispatchServerParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.started) == 0 {
		panic("no server was started")
	}
	return h.started[len(h.started)-1]
}

func (h *cannedA2AHost) lastStream() DispatchTransportParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.streams) == 0 {
		panic("no stream was driven")
	}
	return h.streams[len(h.streams)-1]
}

func (h *cannedA2AHost) snapshot() (started, stopped int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.started), h.stopped
}

// The fetch tool's turn is served: the host receives the "Fetch" agent
// with the turn's call shaping, the client addresses the task session as
// the A2A context, the canned outcome becomes the tool response, and the
// served server stops when the turn ends.
func TestAgenticFetchRunsOverA2A(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	const providerID = "test-provider"
	c.cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
	c.cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	c.cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)

	require.NoError(t, os.MkdirAll(c.cfg.Config().Options.DataDirectory, 0o755))

	host := &cannedA2AHost{outcome: DispatchTransportOutcome{Status: transportStatusCompleted, Text: "analysis done"}}
	c.SetDispatchHost(host)

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	tool, err := c.agenticFetchTool(t.Context(), nil)
	require.NoError(t, err)

	input, err := json.Marshal(tools.AgenticFetchParams{Prompt: "what is new in a2a land"})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, parent.ID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-1",
		Name:  tools.AgenticFetchToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "analysis done", resp.Content)

	params := host.lastStarted()
	require.Equal(t, "Fetch", params.Name)
	require.Equal(t, "Fetches and analyzes web content and search results.", params.Description)
	require.NotNil(t, params.Runner, "the turn is served with the fetch agent as its runner")
	require.True(t, params.Call.NonInteractive, "served sub-agent turns are non-interactive")
	require.True(t, strings.HasPrefix(params.DispatchID, "agent-"),
		"sub-agent turns serve under the agent- route namespace")
	require.NotEqual(t, parent.ID, params.SessionID, "the turn runs against its own task session")

	stream := host.lastStream()
	require.Equal(t, params.SessionID, stream.ContextID, "the client addresses the task session as the A2A context")
	require.Contains(t, stream.Prompt, "what is new in a2a land")

	started, stopped := host.snapshot()
	require.Equal(t, 1, started)
	require.Equal(t, 1, stopped, "the served turn's server stops when the turn ends")
}

// No host wired: the fetch turn fails with the shared no-fallback error
// instead of ever running the agent unserved.
func TestAgenticFetchRequiresA2AHost(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)
	// newDispatchTestCoordinator wires a default host; the no-host case
	// under test needs it gone.
	c.SetDispatchHost(nil)

	const providerID = "test-provider"
	c.cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
	c.cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	c.cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)

	require.NoError(t, os.MkdirAll(c.cfg.Config().Options.DataDirectory, 0o755))

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	tool, err := c.agenticFetchTool(t.Context(), nil)
	require.NoError(t, err)

	input, err := json.Marshal(tools.AgenticFetchParams{Prompt: "look it up"})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, parent.ID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "msg-1")
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "call-1",
		Name:  tools.AgenticFetchToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no A2A host is wired")
}
