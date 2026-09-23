package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestBuildAgentReadinessSurvivesCallerCancellation is a regression test for
// the CRUSH_CLIENT_SERVER=1 "new session hangs" bug.
//
// buildAgent starts readiness goroutines that build the system prompt and the
// initial tool list. Several server entry points build an agent from a
// short-lived HTTP request context — the InitAgent/UpdateAgent handlers, and
// the sub-agent build reached through UpdateModels -> buildTools -> agentTool.
// When that request context was canceled the moment the handler returned, the
// readiness work recorded context.Canceled and every later coordinator.run
// failed at its readiness wait before emitting anything — the session hung
// with no visible LLM response. (This was made worse while the tool-list
// goroutine also blocked in mcp.WaitForInit, which kept it parked long enough
// to observe the cancellation; the readiness work no longer waits on MCP init
// — see coordinator.run — but the cancellation detachment still matters.)
//
// The fix detaches the readiness work from the caller context via
// context.WithoutCancel, so canceling the context that triggered the build no
// longer poisons readiness. Here we build an agent with a cancelable context,
// cancel it, and require that the agent still becomes ready cleanly.
//
// @joestump-agent 09/22/2026 - Waits on the built agent's own readiness latch
// instead of the coordinator-wide readyWg errgroup, which #298 removed.
func TestBuildAgentReadinessSurvivesCallerCancellation(t *testing.T) {
	env := testEnv(t)

	// Minimal hermetic config: one openai-typed provider with selected large
	// and small models so buildAgentModels and the system-prompt build both
	// succeed. No MCP servers are configured, so initialization would complete
	// instantly if we let it — we arm the gate anyway to prove the readiness
	// goroutines no longer block on it.
	crushJSON := `{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crush.json"), []byte(crushJSON), 0o644))

	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
	}

	// Arm the MCP init gate. We never complete init; the readiness goroutines
	// must not care, since they build the tool list from the registry as it
	// stands rather than waiting for initialization to finish.
	mcp.ArmInit()
	t.Cleanup(mcp.DisarmInit)

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)
	agentCfg := cfg.Config().Agents[config.AgentCoder]

	ctx, cancel := context.WithCancel(context.Background())
	built, err := coord.buildAgent(ctx, p, agentCfg, false)
	require.NoError(t, err)

	// The caller goes away, mirroring an HTTP handler returning and canceling
	// its request context.
	cancel()

	done := make(chan error, 1)
	go func() { done <- built.WaitReady() }()

	select {
	case err := <-done:
		// context.Canceled is the regression: the caller's cancellation
		// leaked into the readiness work and poisoned the latch.
		require.NotErrorIs(t, err, context.Canceled,
			"readiness was poisoned by caller cancellation (client/server new-session hang regression)")
		require.NoError(t, err, "unexpected buildAgent readiness error")
	case <-time.After(2 * time.Second):
		t.Fatal("readiness did not settle; the readiness goroutines must not block on MCP init")
	}
}

// TestConcurrentRunsDoNotPanicOnAgentReadiness is a regression test for the
// channel-mode crash loop in joestump-agent/crush#298.
//
// Readiness used to live in one errgroup.Group on the coordinator: every run
// waited on it, and every buildAgent added to it. buildAgent is not
// once-per-process — coordinator.run calls UpdateModels, which reaches
// buildTools -> agentTool -> buildAgent for the sub-agent on every single run —
// so with two runs in flight one could register readiness work while the other
// was parked in Wait. Go 1.27's sync.WaitGroup treats that as reuse before Wait
// returned and panics, taking the process down; channel mode hit it constantly
// because messages arrive unsolicited and concurrently.
//
// Each run below waits for readiness and then builds a sub-agent, so a handful
// of concurrent runs reproduces the interleaving. The runs are expected to fail
// (the session does not exist and the provider port is closed); the assertion
// is that the process survives them. A panic in a run goroutine takes the test
// binary down with it, which is the failure signal.
//
// @joestump-agent 09/22/2026 - Added with the per-agent readiness latch.
func TestConcurrentRunsDoNotPanicOnAgentReadiness(t *testing.T) {
	coord := newGateTestCoordinator(t, true)

	const (
		workers = 8
		rounds  = 25
	)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				// The error is irrelevant — reaching the readiness wait and
				// the sub-agent build inside UpdateModels is the point.
				_, _ = coord.run(context.Background(), nil, "missing-session", "hello")
			}
		}()
	}
	wg.Wait()
}
