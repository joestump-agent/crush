package backend

import (
	"context"
	"errors"

	"github.com/charmbracelet/crush/internal/a2a"
)

// ErrA2AHostNotRunning is returned when a workspace's A2A host has not
// started: nothing has been dispatched yet, or the workspace is going.
var ErrA2AHostNotRunning = errors.New("the workspace's agent host is not running")

// A2AHost returns a workspace's running A2A host (#421), the one the
// server proxies a client/server TUI's agent surface to. With wait it
// holds, within ctx and the workspace's life, until the host is set and
// started, so an index stream opened before the first dispatch is kept
// rather than refused; without it a host that is not up yet is
// [ErrA2AHostNotRunning].
func (b *Backend) A2AHost(ctx context.Context, workspaceID string, wait bool) (*a2a.ServerFactory, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	host, set := ws.agentHost()
	if host == nil && wait {
		select {
		case <-set:
			host, _ = ws.agentHost()
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ws.ctx.Done():
			return nil, ErrWorkspaceClosing
		}
	}
	if host == nil {
		return nil, ErrA2AHostNotRunning
	}
	if _, ok := host.AgentIndexConn(); !ok && wait {
		select {
		case <-host.Started():
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ws.ctx.Done():
			return nil, ErrWorkspaceClosing
		}
	}
	// Started is also closed for a host that has since closed: check
	// again rather than trust the wake.
	if _, ok := host.AgentIndexConn(); !ok {
		return nil, ErrA2AHostNotRunning
	}
	return host, nil
}

// agentHost returns the workspace's A2A host and the channel closed once
// it is set: the app's, unless a test supplied its own.
func (ws *Workspace) agentHost() (*a2a.ServerFactory, <-chan struct{}) {
	if ws.a2aHost != nil {
		return ws.a2aHost()
	}
	return ws.A2AHost()
}
