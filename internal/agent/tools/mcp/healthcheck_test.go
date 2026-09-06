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

// The regression this fix exists for.
//
// A channel session whose notification stream is down still answers pings — ping rides ordinary
// POSTs and says nothing about the stream. The first version of this health check handled that by
// calling updateState(StateError) itself and then getOrRenewClient. StateError DELETES the registry
// entry, so the renewal's post-lock guard found nothing and returned "mcp '<name>' not available"
// without ever rebuilding. The health check became a once-a-minute outage: it destroyed a working
// session every tick and never replaced it.
//
// The session must come back, and it must still be registered afterwards.
func TestChannelHealthCheckRebuildsAStreamDownSessionThatPingsFine(t *testing.T) {
	const name = "test-stream-down"
	t.Cleanup(cleanupSession(name))

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	// A LIVE session — it pings fine, which is the whole point.
	live, _ := liveSession(t, "send_message")
	live.channel = true
	sessions.Set(name, live)
	require.NoError(t, pingSession(context.Background(), live, time.Second),
		"precondition: the session must be pingable, or this test proves nothing")

	// ...whose notification stream opened and then died and did not come back.
	// Observed-open-then-closed is the recoverable case a rebuild is for; a
	// never-observed stream is absence of evidence and deliberately reads as
	// healthy, because treating it as down caused a once-a-minute rebuild loop.
	health := &channelStreamHealth{}
	health.opened.Store(true)
	health.closedAt.Store(time.Now().Add(-2 * channelStreamClosedGrace).UnixMilli())
	channelStreamStates.Set(name, health)
	t.Cleanup(func() { channelStreamStates.Del(name) })
	require.False(t, health.healthy(channelStreamClosedGrace),
		"precondition: a stream closed beyond the grace must read as down")

	var created int
	orig := newSession
	newSession = func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, bool) (*ClientSession, error) {
		created++
		sess, _ := liveSession(t, "send_message")
		sess.channel = true
		return sess, nil
	}
	t.Cleanup(func() { newSession = orig })

	checkChannelSessions(context.Background(), cfg)

	require.Equal(t, 1, created, "a stream-down session must be rebuilt, not merely torn down")
	got, ok := sessions.Get(name)
	require.True(t, ok, "the rebuilt session must be registered — the bug left the registry empty")
	require.NotSame(t, live, got, "the dead session must have been replaced")
	require.NoError(t, pingSession(context.Background(), got, time.Second))
}

// A channel session with a healthy stream must be left alone: forcing a rebuild every minute
// would churn every working consumer.
func TestChannelHealthCheckLeavesAHealthyStreamAlone(t *testing.T) {
	const name = "test-stream-ok"
	t.Cleanup(cleanupSession(name))

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})
	live, _ := liveSession(t, "send_message")
	live.channel = true
	sessions.Set(name, live)

	health := &channelStreamHealth{}
	health.opened.Store(true)
	health.active.Store(true)
	channelStreamStates.Set(name, health)
	t.Cleanup(func() { channelStreamStates.Del(name) })

	var created int
	orig := newSession
	newSession = func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, bool) (*ClientSession, error) {
		created++
		s, _ := liveSession(t, "send_message")
		return s, nil
	}
	t.Cleanup(func() { newSession = orig })

	checkChannelSessions(context.Background(), cfg)

	require.Zero(t, created, "a healthy channel session must not be rebuilt")
	got, _ := sessions.Get(name)
	require.Same(t, live, got, "the healthy session must be the same object afterwards")
}
