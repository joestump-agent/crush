package server

import (
	"net/http"

	"github.com/charmbracelet/crush/internal/a2a"
)

// handleGetWorkspaceA2AAgents proxies the workspace A2A host's agent
// index (#421). A stream waits for the host to start, so a TUI that
// opens it before the first dispatch is held rather than refused; a
// snapshot answers at once.
func (c *controllerV1) handleGetWorkspaceA2AAgents(w http.ResponseWriter, r *http.Request) {
	host, err := c.backend.A2AHost(r.Context(), r.PathValue("id"), a2a.AcceptsEventStream(r))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		c.handleError(w, r, err)
		return
	}
	host.ServeProxied(w, r, a2a.AgentsIndexPath)
}

// handlePostWorkspaceA2AAgent proxies one A2A JSON-RPC request to a
// dispatched agent on the workspace's host (#421): steers, cancels and
// task reads from a client/server TUI. The host's own checks judge the
// request; the server only adds the host's credential.
func (c *controllerV1) handlePostWorkspaceA2AAgent(w http.ResponseWriter, r *http.Request) {
	host, err := c.backend.A2AHost(r.Context(), r.PathValue("id"), false)
	if err != nil {
		c.handleError(w, r, err)
		return
	}
	host.ServeProxied(w, r, a2a.AgentPath(r.PathValue("agent")))
}
