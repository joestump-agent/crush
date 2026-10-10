package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
//
// @joestump-agent 10/01/2026 - Joins the loop before teardown. Left unjoined,
// a loop still inside a renewal raced the newSession restore and called the
// liveSession stub after the test had completed, panicking the test binary
// ("Fail in goroutine after ... has completed") and turning main red.
//
// @joestump-agent 10/08/2026 - The join did not actually run first. The stub
// called liveSession on the loop goroutine, which registered the renewed
// server's close after the join, so LIFO teardown closed that server while
// the loop was still ticking. The loop saw the session die and renewed again;
// that renewal's own cleanup, appended mid-teardown, was popped next and
// closed its server before initialize ("client is closing: EOF"). The stub
// now leaves t alone and the test goroutine owns the servers it starts.
func TestChannelHealthCheckLoop_TickerRenewsDeadSession(t *testing.T) {
	const name = "test-health-loop"
	t.Cleanup(cleanupSession(name))

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	seedDeadSession(t, name, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	// The stub runs on the loop goroutine, so it must not touch t (see
	// connectLiveSession). The servers it starts are closed here instead, and
	// this cleanup is registered before the join below so it runs after it:
	// closing a server under a running loop is what started the failure.
	var (
		serversMu sync.Mutex
		servers   []*mcp.ServerSession
	)
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			t.Error("the loop must be joined before the servers it renews against are closed")
		}
		serversMu.Lock()
		defer serversMu.Unlock()
		for _, s := range servers {
			_ = s.Close()
		}
	})

	connectErr := make(chan error, 1)
	origNewSession := newSession
	newSession = func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, bool) (*ClientSession, error) {
		sess, _, server, err := connectLiveSession("send_message")
		if err != nil {
			select {
			case connectErr <- err:
			default:
			}
			return nil, err
		}
		serversMu.Lock()
		servers = append(servers, server)
		serversMu.Unlock()
		sess.channel = true
		return sess, nil
	}
	t.Cleanup(func() { newSession = origNewSession })

	events := SubscribeEvents(ctx)
	go func() {
		defer close(done)
		runChannelHealthCheck(ctx, cfg, 10*time.Millisecond)
	}()
	// Stop and join the loop before anything else is torn down. Registered
	// last, and nothing on the loop goroutine registers cleanups of its own,
	// so it runs first: the newSession restore and the server closes above
	// both happen with the loop stopped.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("health check loop did not stop after context cancellation")
		}
	})

	// Wait for the renewal to publish StateConnected, which renewClient does
	// only after registering the new session, instead of pinging the registry
	// while the loop is replacing what is in it.
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Payload.Type != EventStateChanged || e.Payload.Name != name || e.Payload.State != StateConnected {
				continue
			}
			sess, ok := sessions.Get(name)
			require.True(t, ok, "a renewed session must be registered when StateConnected is published")
			require.NoError(t, pingSession(context.Background(), sess, time.Second))
			return
		case err := <-connectErr:
			t.Fatalf("a renewal failed to connect: %v", err)
		case <-timeout:
			t.Fatal("health check loop never renewed the dead channel session")
		}
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

// TestChannelHealthCheckLoop_LeavesNoGoroutineBehind pins that a test which
// starts the loop joins it before tearing down.
//
// A loop left running past the test body keeps calling the newSession stub
// while teardown restores it and closes the servers the stub started. When
// that stub still asserted on t, this panicked the whole test binary with
// "Fail in goroutine after ... has completed" rather than failing one test.
// This is the flake that turned main red; the assertion below fails if the
// join is dropped.
//
// @joestump-agent 10/01/2026 - Added after the unjoined loop in
// TestChannelHealthCheckLoop_TickerRenewsDeadSession panicked CI on main.
func TestChannelHealthCheckLoop_LeavesNoGoroutineBehind(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	// Registered first, so it runs LAST: the join below must already have
	// observed the loop exit by the time this runs.
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			t.Error("the health check loop goroutine outlived the test")
		}
	})

	go func() {
		defer close(done)
		runChannelHealthCheck(ctx, cfg, 10*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	time.Sleep(50 * time.Millisecond)
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

func TestChannelHealthCheckRetriesAfterFailedRebuild(t *testing.T) {
	const name = "test-retry-failed-rebuild"
	t.Cleanup(cleanupSession(name))
	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, ChannelEnabled: true},
	}})

	seedDeadSession(t, name, true)
	dead, ok := sessions.Get(name)
	require.True(t, ok)
	updateState(name, StateConnected, nil, dead, Counts{})

	origNewSession := newSession
	calls := 0
	newSession = func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, bool) (*ClientSession, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("temporary server outage")
		}
		sess, _ := liveSession(t, "send_message")
		sess.channel = true
		return sess, nil
	}
	t.Cleanup(func() { newSession = origNewSession })

	checkChannelSessions(context.Background(), cfg)
	require.Equal(t, 1, calls)
	_, ok = sessions.Get(name)
	require.False(t, ok, "a failed rebuild leaves no live session")
	info, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateError, info.State)
	require.True(t, info.Channel, "the error state must retain channel identity for retries")

	checkChannelSessions(context.Background(), cfg)
	require.Equal(t, 2, calls, "the next health check must retry the missing channel session")
	renewed, ok := sessions.Get(name)
	require.True(t, ok, "a later successful attempt must restore the session")
	require.NoError(t, pingSession(context.Background(), renewed, time.Second))
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
