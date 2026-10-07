package client

import (
	"net/url"

	"github.com/charmbracelet/crush/internal/a2a"
)

// A2AProxy reaches a workspace's A2A host through the server's proxy
// (#421): the agent index and the dispatched agents' routes, over this
// client's connection to the server. The server authenticates to the
// host, so no token is involved here.
func (c *Client) A2AProxy(workspaceID string) a2a.ProxyClient {
	host := c.addr
	if c.network == "npipe" || c.network == "unix" {
		host = DummyHost
	}
	base := "http://" + host + "/v1/workspaces/" + url.PathEscape(workspaceID) + "/a2a"
	return a2a.ProxyClient{HTTP: c.h, BaseURL: base}
}
