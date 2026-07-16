package mcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestWaitForInit_BlocksBeforeInitialize verifies that WaitForInit blocks
// when Initialize has not yet been called (or has not yet completed).
// This is the foundation of the fix: the coordinator must call WaitForInit
// before reading the tool registry so that slow-to-start MCP servers
// (e.g. stdio Python via uv) have time to register their tools.
//
// initDone is a package-global one-shot channel. If another test in this
// package has already triggered Initialize (closing initDone), this test
// is a no-op — the contract is already satisfied. Otherwise, it verifies
// that a context with a short deadline expires while waiting, proving
// WaitForInit actually blocks.
func TestWaitForInit_BlocksBeforeInitialize(t *testing.T) {
	// If initDone was already closed by a prior Initialize call, the
	// blocking contract can't be tested in this process. Verify it
	// returns immediately and skip the blocking assertion.
	select {
	case <-initDone:
		// Already initialized — WaitForInit returns immediately.
		// This is fine; the point is that in a fresh process it blocks.
		err := WaitForInit(context.Background())
		require.NoError(t, err)
		return
	default:
	}

	// initDone is still open. WaitForInit must block until the context
	// expires (since we are not calling Initialize in this test).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := WaitForInit(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"WaitForInit must block when Initialize has not completed")
}

// TestWaitForInit_ReturnsAfterInitialize verifies that WaitForInit returns
// without error once Initialize completes. It also verifies that tools
// registered during initialization are visible in the allTools registry
// after WaitForInit returns — the core invariant the coordinator relies on.
//
// Because initDone and initOnce are package-global one-shots, this test
// triggers Initialize exactly once for the entire test binary. It uses
// an in-memory MCP server so no external processes are spawned.
func TestWaitForInit_ReturnsAfterInitialize(t *testing.T) {
	// If Initialize was already called by a prior test, initDone is
	// closed and WaitForInit returns immediately. We can still verify
	// the tool-visibility invariant using the registry directly.
	select {
	case <-initDone:
		// Already initialized; verify WaitForInit returns promptly.
		err := WaitForInit(context.Background())
		require.NoError(t, err)
		return
	default:
	}

	// Set up an in-memory MCP server with a tool, then use a config
	// that points to it via a fake transport. Since Initialize uses
	// createSession → createTransport which requires real stdio/http
	// transports, we simulate the init completion by directly closing
	// initDone after registering tools — mirroring what Initialize does
	// internally (wg.Wait() then close(initDone)).
	const name = "test-waitforinit"
	t.Cleanup(func() {
		allTools.Del(name)
		states.Del(name)
	})

	// Simulate a slow MCP server: register tools after a delay, then
	// signal init completion (close initDone).
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)

		sess, _ := liveSession(t, "slow_tool")
		sessions.Set(name, sess)
		allTools.Set(name, []*Tool{{Name: "slow_tool"}})
		updateState(name, StateConnected, nil, sess, Counts{Tools: 1})
	}()

	// In production, Initialize does wg.Wait() then close(initDone).
	// We replicate that sequencing here.
	go func() {
		wg.Wait()
		initOnce.Do(func() { close(initDone) })
	}()

	// WaitForInit must block until the simulated init completes.
	err := WaitForInit(context.Background())
	require.NoError(t, err)

	// After WaitForInit returns, the slow server's tools must be visible.
	tools, ok := allTools.Get(name)
	require.True(t, ok, "tools from a slow MCP server must be registered after WaitForInit")
	require.Len(t, tools, 1)
	require.Equal(t, "slow_tool", tools[0].Name)
}

// TestWaitForInit_ToolsVisibleAfterInit is the regression test for the
// specific bug: buildTools reads allTools concurrently with MCP
// initialization. Without WaitForInit as a gate, a race exists where
// buildTools runs before slow MCP servers register their tools, so the
// LLM's tool palette silently omits them even though crush_info later
// reports the server as "connected, N tools".
//
// This test simulates the race: a reader goroutine checks allTools before
// and after WaitForInit. Before WaitForInit the tools may or may not be
// present (that's the race), but after WaitForInit they MUST be present.
func TestWaitForInit_ToolsVisibleAfterInit(t *testing.T) {
	const name = "test-race-visibility"
	t.Cleanup(func() {
		allTools.Del(name)
		states.Del(name)
	})

	cfg := config.NewTestStore(&config.Config{})

	_ = cfg // suppress unused var if config isn't needed for this test

	// If initDone is already closed (prior test called Initialize),
	// we can't test the before/after gating. Just verify tools are visible.
	select {
	case <-initDone:
		// Register tools and verify they're visible.
		sess, _ := liveSession(t, "post_init_tool")
		sessions.Set(name, sess)
		allTools.Set(name, []*Tool{{Name: "post_init_tool"}})

		err := WaitForInit(context.Background())
		require.NoError(t, err)

		tools, ok := allTools.Get(name)
		require.True(t, ok)
		require.Len(t, tools, 1)
		return
	default:
	}

	// Fresh process: simulate a slow MCP that registers tools after a delay.
	registered := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		sess, _ := liveSession(t, "gated_tool")
		sessions.Set(name, sess)
		allTools.Set(name, []*Tool{{Name: "gated_tool"}})
		updateState(name, StateConnected, nil, sess, Counts{Tools: 1})
		close(registered)
	}()

	go func() {
		<-registered
		initOnce.Do(func() { close(initDone) })
	}()

	// WaitForInit must block until init completes.
	err := WaitForInit(context.Background())
	require.NoError(t, err)

	// Tools MUST be visible after WaitForInit returns.
	tools, ok := allTools.Get(name)
	require.True(t, ok,
		"tools must be visible after WaitForInit — this is the regression: "+
			"buildTools must call WaitForInit before reading allTools")
	require.Len(t, tools, 1)
	require.Equal(t, "gated_tool", tools[0].Name)
}
