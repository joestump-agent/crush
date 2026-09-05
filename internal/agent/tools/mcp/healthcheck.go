package mcp

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/config"
)

// channelHealthCheckInterval is how often the health check pings each
// channel-enabled MCP session.
const channelHealthCheckInterval = time.Minute

// StartChannelHealthCheck launches the periodic channel session health
// check. It runs for the lifetime of ctx.
func StartChannelHealthCheck(ctx context.Context, cfg *config.ConfigStore) {
	go runChannelHealthCheck(ctx, cfg, channelHealthCheckInterval)
}

// runChannelHealthCheck periodically renews channel sessions that no other
// traffic would renew.
//
// A channel-enabled MCP connection carries no traffic of its own: the
// consumer never calls its tools, so the lazy getOrRenewClient path behind
// every tool call never runs, and pingSession is never reached. When such a
// connection's stream drops, nothing surfaces the failure — the session
// looks established from the client's side, the server keeps writing
// notifications into a dead stream, and only a restart clears it. This loop
// closes that gap: it pings every registered channel session on a fixed
// interval and renews the dead ones through the same serialized renewal path
// tool calls use, without waiting for traffic that will never come.
func runChannelHealthCheck(ctx context.Context, cfg *config.ConfigStore, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkChannelSessions(ctx, cfg)
		}
	}
}

// checkChannelSessions pings every channel session and renews any whose ping
// fails. Only channel sessions are checked: ordinary servers have their
// sessions renewed lazily by tool calls, so proactively pinging them would
// add traffic for no benefit.
//
// A ping alone cannot see the failure mode that matters most: on a
// streamable-HTTP channel server, doorbells arrive on the standalone SSE
// stream, and a session whose stream is gone keeps answering pings over
// ordinary request/response. So a channel session on a streamable transport
// is only healthy when its notification stream is connected too; when the
// stream has stayed down past the reconnect grace the session is forced
// through the same rebuild path a failed ping takes.
func checkChannelSessions(ctx context.Context, cfg *config.ConfigStore) {
	for name, sess := range sessions.Seq2() {
		if !sess.IsChannel() {
			continue
		}
		if health, ok := channelStreamStates.Get(name); ok && !health.healthy(channelStreamClosedGrace) {
			channelStreamReportable(name, health)
			state, _ := states.Get(name)
			updateState(name, StateError, errChannelStreamDown, sess, state.Counts)
		}
		if _, err := getOrRenewClient(ctx, cfg, name); err != nil {
			slog.Warn("MCP channel health check failed to renew session", "name", name, "error", err)
		}
	}
}

// errChannelStreamDown forces a channel session rebuild when the standalone
// SSE notification stream has been down longer than the reconnect grace.
var errChannelStreamDown = errors.New("standalone SSE notification stream not connected")
