package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// newSidekickTestCoordinator wires a real coordinator through the
// production NewCoordinator constructor against an offline (or
// test-server-backed) openai-compatible provider, mirroring
// agenttest.NewCoordinator but in-package so tests can reach unexported
// internals.
func newSidekickTestCoordinator(t *testing.T, env fakeEnv, baseURL string) *coordinator {
	t.Helper()
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	const (
		providerID = "test-openai-compat"
		modelID    = "test-model"
	)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: baseURL,
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: modelID, DefaultMaxTokens: 4096, ContextWindow: 200000}},
	})
	selected := config.SelectedModel{Provider: providerID, Model: modelID}
	cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)
	cfg.SetupAgents()

	// Keep buildTools light: no sub-agent or agentic-fetch construction.
	coderCfg := cfg.Config().Agents[config.AgentCoder]
	coderCfg.AllowedTools = nil
	cfg.Config().Agents[config.AgentCoder] = coderCfg

	// Close the package-global MCP init gate (no MCP servers are
	// configured) so the coordinator's readyWg never blocks on it.
	mcp.Initialize(t.Context(), env.permissions, cfg)

	coord, err := NewCoordinator(
		t.Context(),
		cfg,
		env.sessions,
		env.messages,
		env.permissions,
		env.history,
		*env.filetracker,
		nil,
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	return coord.(*coordinator)
}

// newSidekickSSEServer serves a minimal OpenAI-compatible streaming chat
// completion that always answers with text.
func newSidekickSSEServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)
		chunks := []string{
			fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, text),
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":3,"total_tokens":4}}`,
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSidekickAgentConstructed verifies NewCoordinator builds the second,
// independent Sidekick agent when the sidekick agent config is present.
func TestSidekickAgentConstructed(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	require.NotNil(t, coord.Sidekick())
	require.NotSame(t, coord.currentAgent, coord.Sidekick().SessionAgent)

	sa := coord.Sidekick().SessionAgent.(*sessionAgent)
	require.True(t, sa.ephemeral)
	require.True(t, sa.isSubAgent, "sidekick must not receive the todo system reminder")
}

// TestBuildSidekickToolsReadOnly verifies the Sidekick tool list is the
// read-only subset (with the read-only bash variant) and that no tool is
// wrapped with PreToolUse hook interception.
func TestBuildSidekickToolsReadOnly(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	agentCfg, ok := coord.cfg.Config().Agents[config.AgentSidekick]
	require.True(t, ok, "sidekick agent config must exist")
	require.Equal(t, config.SelectedModelTypeSmall, agentCfg.Model, "sidekick defaults to the small model")

	sidekickTools := coord.buildSidekickTools(agentCfg)
	var names []string
	for _, tool := range sidekickTools {
		names = append(names, tool.Info().Name)
		_, hooked := tool.(*hookedTool)
		require.False(t, hooked, "sidekick tools must never be hook-wrapped: %s", tool.Info().Name)
	}
	require.Equal(t, []string{"bash", "glob", "grep", "ls", "sourcegraph", "view"}, names)
}

// TestRunSidekickNotConfigured verifies RunSidekick fails cleanly when no
// sidekick agent was built.
func TestRunSidekickNotConfigured(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	_, err := c.RunSidekick(t.Context(), "hello")
	require.ErrorIs(t, err, errSidekickAgentNotConfigured)
}

// TestRunSidekick drives a full Sidekick turn against a fake streaming
// provider and verifies the independence guarantees: the run lands in the
// Sidekick's private in-memory stores, nothing persists to the main
// session/message services, and the main agent's busy state is untouched.
func TestRunSidekick(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	srv := newSidekickSSEServer(t, "sidekick says hi")
	coord := newSidekickTestCoordinator(t, env, srv.URL+"/v1")

	result, err := coord.RunSidekick(t.Context(), "hello")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "sidekick says hi", result.Response.Content.Text())

	// The turn ran in the Sidekick's private ephemeral session.
	ea := coord.Sidekick()
	sessions, err := ea.Sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "Sidekick", sessions[0].Title, "sidekick sessions never generate titles")

	msgs, err := ea.Messages.List(t.Context(), sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, message.Assistant, msgs[1].Role)

	// Nothing leaked into the main (persistent) stores.
	mainSessions, err := env.sessions.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, mainSessions)

	// Main agent busy state is fully isolated from Sidekick activity.
	require.False(t, coord.IsBusy())
	require.False(t, coord.IsSessionBusy(sessions[0].ID))

	// A follow-up prompt reuses the same ephemeral session.
	_, err = coord.RunSidekick(t.Context(), "again")
	require.NoError(t, err)
	sessions, err = ea.Sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	msgs, err = ea.Messages.List(t.Context(), sessions[0].ID)
	require.NoError(t, err)
	require.Len(t, msgs, 4)
}

// TestSidekickBusyIsolation verifies busy tracking is independent in both
// directions: main-agent activity never marks the Sidekick busy, and
// Sidekick activity never marks the coordinator (main agent) busy. It also
// verifies the two agents share no queue state.
func TestSidekickBusyIsolation(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	const sid = "some-session"
	sidekick := coord.Sidekick().SessionAgent.(*sessionAgent)
	main := coord.currentAgent.(*sessionAgent)

	// Sidekick busy -> coordinator (main agent) not busy.
	sidekick.activeRequests.Set(sid, func() {})
	require.True(t, coord.Sidekick().IsSessionBusy(sid))
	require.False(t, coord.IsSessionBusy(sid))
	require.False(t, coord.IsBusy())
	sidekick.activeRequests.Del(sid)

	// Main agent busy -> sidekick not busy.
	main.activeRequests.Set(sid, func() {})
	require.True(t, coord.IsSessionBusy(sid))
	require.False(t, coord.Sidekick().IsSessionBusy(sid))
	require.False(t, coord.Sidekick().IsBusy())
	main.activeRequests.Del(sid)

	// Queues are per-agent: enqueueing on the main agent is invisible to
	// the Sidekick (and a busy Sidekick rejects instead of queueing, per
	// the ephemeral contract tests).
	main.enqueueCall(SessionAgentCall{SessionID: sid, Prompt: "queued"})
	require.Equal(t, 1, main.QueuedPrompts(sid))
	require.Equal(t, 0, sidekick.QueuedPrompts(sid))
	main.ClearQueue(sid)
}

// TestRunSidekickBusyRejects verifies a busy Sidekick rejects a second
// prompt with ErrSessionBusy instead of queueing it.
func TestRunSidekickBusyRejects(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	srv := newSidekickSSEServer(t, "hi")
	coord := newSidekickTestCoordinator(t, env, srv.URL+"/v1")

	// Prime the session so we can mark it busy.
	_, err := coord.RunSidekick(t.Context(), "hello")
	require.NoError(t, err)

	sidekick := coord.Sidekick().SessionAgent.(*sessionAgent)
	sidekick.activeRequests.Set(coord.sidekickSessionID, func() {})
	defer sidekick.activeRequests.Del(coord.sidekickSessionID)

	_, err = coord.RunSidekick(context.WithoutCancel(t.Context()), "while busy")
	require.ErrorIs(t, err, ErrSessionBusy)
	require.Equal(t, 0, sidekick.QueuedPrompts(coord.sidekickSessionID),
		"busy sidekick must reject, never queue")
}

// TestClearSidekick verifies /clear semantics: the ephemeral session and
// its messages are destroyed and the next run starts a fresh session.
func TestClearSidekick(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	srv := newSidekickSSEServer(t, "hi")
	coord := newSidekickTestCoordinator(t, env, srv.URL+"/v1")

	_, err := coord.RunSidekick(t.Context(), "hello")
	require.NoError(t, err)
	firstSession := coord.sidekickSessionID
	require.NotEmpty(t, firstSession)

	require.NoError(t, coord.ClearSidekick(t.Context()))
	require.Empty(t, coord.sidekickSessionID, "clear must reset the session ID")

	ea := coord.Sidekick()
	sessions, err := ea.Sessions.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, sessions, "clear must destroy the ephemeral session")
	msgs, err := ea.Messages.List(t.Context(), firstSession)
	require.NoError(t, err)
	require.Empty(t, msgs, "clear must destroy the ephemeral messages")

	// Clearing an already-empty conversation is a no-op.
	require.NoError(t, coord.ClearSidekick(t.Context()))

	// The next run starts a brand-new session.
	_, err = coord.RunSidekick(t.Context(), "fresh start")
	require.NoError(t, err)
	require.NotEmpty(t, coord.sidekickSessionID)
	require.NotEqual(t, firstSession, coord.sidekickSessionID)
	msgs, err = ea.Messages.List(t.Context(), coord.sidekickSessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "the fresh conversation must not inherit history")
}

// TestClearSidekickNotConfigured verifies ClearSidekick fails cleanly
// without a sidekick agent.
func TestClearSidekickNotConfigured(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	require.ErrorIs(t, c.ClearSidekick(t.Context()), errSidekickAgentNotConfigured)
	require.False(t, c.IsSidekickBusy())
	c.CancelSidekick() // must not panic
}

// TestIsSidekickBusy verifies the busy probe reads the Sidekick's own
// activeRequests and nothing else.
func TestIsSidekickBusy(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	require.False(t, coord.IsSidekickBusy())
	sidekick := coord.Sidekick().SessionAgent.(*sessionAgent)
	sidekick.activeRequests.Set("sk", func() {})
	require.True(t, coord.IsSidekickBusy())
	require.False(t, coord.IsBusy(), "sidekick busy must not leak into the main agent")
	sidekick.activeRequests.Del("sk")
	require.False(t, coord.IsSidekickBusy())
}

// TestSidekickModelDefaultsAndFallback pins the Sidekick model
// resolution order (#54): without the zai provider the Sidekick falls
// back to the configured small model; once zai is configured with
// glm-5.2 discovered, that pair becomes the default.
func TestSidekickModelDefaultsAndFallback(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	// No zai provider configured: graceful fallback to the small model.
	small := coord.cfg.Config().Models[config.SelectedModelTypeSmall]
	require.Equal(t, small, coord.SidekickModel(),
		"without zai the sidekick must fall back to the configured small model")

	// Configure zai with glm-5.2 (as model discovery would): it becomes
	// the default.
	coord.cfg.Config().Providers.Set(sidekickDefaultProvider, config.ProviderConfig{
		ID:      sidekickDefaultProvider,
		Name:    "Z.ai",
		Type:    openaicompat.Name,
		BaseURL: "https://api.z.ai/api/coding/paas/v4",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: sidekickDefaultModelID, DefaultMaxTokens: 4096, ContextWindow: 200000}},
	})
	sel := coord.SidekickModel()
	require.Equal(t, sidekickDefaultProvider, sel.Provider)
	require.Equal(t, sidekickDefaultModelID, sel.Model)

	// A zai provider without the model (discovery failed / token
	// missing) must not be selected.
	coord.cfg.Config().Providers.Set(sidekickDefaultProvider, config.ProviderConfig{
		ID:      sidekickDefaultProvider,
		Type:    openaicompat.Name,
		BaseURL: "https://api.z.ai/api/coding/paas/v4",
	})
	require.Equal(t, small, coord.SidekickModel(),
		"zai without discovered models must fall back to the small model")
}

// TestSetSidekickModelSessionScoped verifies the Sidekick model override
// (#54): it wins the resolution, validates against configured providers,
// and never leaks into the main agent's model selection.
func TestSetSidekickModelSessionScoped(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	coord := newSidekickTestCoordinator(t, env, "http://127.0.0.1:0/v1")

	cfg := coord.cfg.Config()
	largeBefore := cfg.Models[config.SelectedModelTypeLarge]
	smallBefore := cfg.Models[config.SelectedModelTypeSmall]

	// Another configured provider/model to switch to.
	cfg.Providers.Set("other", config.ProviderConfig{
		ID:      "other",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "other-model", DefaultMaxTokens: 4096, ContextWindow: 200000}},
	})

	// Unknown provider or model: rejected.
	require.Error(t, coord.SetSidekickModel(config.SelectedModel{Provider: "nope", Model: "x"}))
	require.Error(t, coord.SetSidekickModel(config.SelectedModel{Provider: "other", Model: "nope"}))
	require.Error(t, coord.SetSidekickModel(config.SelectedModel{}))

	// Valid override: it becomes the Sidekick's model...
	sel := config.SelectedModel{Provider: "other", Model: "other-model"}
	require.NoError(t, coord.SetSidekickModel(sel))
	got := coord.SidekickModel()
	require.Equal(t, "other", got.Provider)
	require.Equal(t, "other-model", got.Model)

	// ...and the override actually resolves to a runnable model.
	agentCfg := cfg.Agents[config.AgentSidekick]
	model, err := coord.buildSidekickModel(t.Context(), agentCfg)
	require.NoError(t, err)
	require.Equal(t, "other-model", model.ModelCfg.Model)

	// Session-scoped: the main agent's selections are untouched.
	require.Equal(t, largeBefore, cfg.Models[config.SelectedModelTypeLarge])
	require.Equal(t, smallBefore, cfg.Models[config.SelectedModelTypeSmall])

	// An override whose provider later disappears degrades gracefully.
	cfg.Providers.Del("other")
	require.Equal(t, smallBefore, coord.SidekickModel(),
		"a stale override must fall back instead of erroring")
}

// TestSetSidekickModelNotConfigured verifies the setter fails cleanly
// without a sidekick agent.
func TestSetSidekickModelNotConfigured(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	require.ErrorIs(t, c.SetSidekickModel(config.SelectedModel{Provider: "p", Model: "m"}), errSidekickAgentNotConfigured)
}
