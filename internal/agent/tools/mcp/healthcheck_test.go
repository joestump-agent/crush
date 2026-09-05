package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// seedDeadSession registers a closed (dead) session under name. Ping on it
// always fails, which is what drives the renewal path.
func seedDeadSession(t *testing.T, name string, channel bool) {
	t.Helper()
	dead, _ := liveSession(t, "send_message")
	dead.channel = channel
	require.NoError(t, dead.Close())
	sessions.Set(name, dead)
}

// cleanupSession removes a server's session and state from the package-level
// registries after a test.
func cleanupSession(name string) func() {
	return func() {
		if s, ok := sessions.Take(name); ok {
			_ = s.Close()
		}
		states.Del(name)
		allTools.Del(name)
		allPrompts.Del(name)
		allResources.Del(name)
	}
}

// TestChannelHealthCheck_RenewsDeadChannelSession is the regression test for
// the channel-only MCP failure mode: a channel consumer never calls tools, so
// nothing pings its session and a dropped stream is never renewed. The health
// check must renew a dead channel session (and only channel sessions) without
// any tool call happening. Without the fix, the dead channel session stays
// registered forever and this test fails on the ping assertion.
func TestChannelHealthCheck_RenewsDeadChannelSession(t *testing.T) {
	const channelName = "test-health-channel"
	const plainName = "test-health-plain"
	t.Cleanup(cleanupSession(channelName))
	t.Cleanup(cleanupSession(plainName))

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{
		channelName: {Type: config.MCPStdio},
		plainName:   {Type: config.MCPStdio},
	}})

	seedDeadSession(t, channelName, true)
	// A dead non-channel session must be left alone: ordinary servers renew
	// lazily via tool calls, so the health check must not touch them.
	seedDeadSession(t, plainName, false)

	var created int
	origNewSession := newSession
	newSession = func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, bool) (*ClientSession, error) {
		created++
		sess, _ := liveSession(t, "send_message")
		sess.channel = true
		return sess, nil
	}
	t.Cleanup(func() { newSession = origNewSession })

	checkChannelSessions(context.Background(), cfg)

	require.Equal(t, 1, created, "exactly the dead channel session must be renewed")

	renewed, ok := sessions.Get(channelName)
	require.True(t, ok, "a live session must be registered after the health check renews it")
	require.NoError(t, pingSession(context.Background(), renewed, time.Second))

	untouched, ok := sessions.Get(plainName)
	require.True(t, ok, "the non-channel session must be left untouched by the health check")
	require.Error(t, pingSession(context.Background(), untouched, time.Second),
		"the non-channel session must still be the dead one, not a renewal")

	info, ok := GetState(channelName)
	require.True(t, ok)
	require.Equal(t, StateConnected, info.State)
}

// TestChannelHealthCheckLoop_TickerRenewsDeadSession pins the loop itself:
// left running with a short interval, it must detect and renew a dead channel
// session on its own, with no tool call ever made. This is the property the
// production deployment depends on — the loop is the only thing standing
// between a dropped channel stream and a deaf consumer.
func TestChannelHealthCheckLoop_TickerRenewsDeadSession(t *testing.T) {
	const name = "test-health-loop"
	t.Cleanup(cleanupSession(name))

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	seedDeadSession(t, name, true)

	origNewSession := newSession
	newSession = func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, bool) (*ClientSession, error) {
		sess, _ := liveSession(t, "send_message")
		sess.channel = true
		return sess, nil
	}
	t.Cleanup(func() { newSession = origNewSession })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runChannelHealthCheck(ctx, cfg, 10*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for {
		sess, ok := sessions.Get(name)
		if ok {
			if err := pingSession(context.Background(), sess, time.Second); err == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("health check loop never renewed the dead channel session")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestChannelHealthCheckLoop_StopsOnContextCancel pins that the loop exits
// when its context is canceled rather than leaking a goroutine per app start.
func TestChannelHealthCheckLoop_StopsOnContextCancel(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runChannelHealthCheck(ctx, cfg, 10*time.Millisecond)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("health check loop did not stop after context cancellation")
	}
}
