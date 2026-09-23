package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// forkOnlyRoutes are the v1 routes this fork carries that upstream does
// not. They exist because the fork adds behavior upstream lacks: model
// discovery reload, skills reload, the A2UI MCP round-trip, and MCP
// reconnect.
//
// The sync from upstream replaced the hand-written mux.HandleFunc list
// with the declarative registry in endpoints.go, which meant porting
// these four by hand. A route silently dropped in that port is invisible
// at compile time and only shows up as a 404 from a client that used to
// work, so pin them by method+path.
var forkOnlyRoutes = []struct {
	method string
	path   string
}{
	{"POST", "/v1/workspaces/{id}/config/reload-discovery"},
	{"POST", "/v1/workspaces/{id}/skills/reload"},
	{"POST", "/v1/workspaces/{id}/mcp/call-tool"},
	{"POST", "/v1/workspaces/{id}/mcp/reconnect"},
}

// TestEndpoints_ForkRoutesRegistered asserts every fork-only route is
// still in the registry. TestDocsEndpoints then proves each one reaches
// the served OpenAPI spec, and installHandler registers them on the mux.
func TestEndpoints_ForkRoutesRegistered(t *testing.T) {
	t.Parallel()

	c := &controllerV1{}
	registered := map[string]bool{}
	for _, e := range c.endpoints() {
		registered[e.Method()+" "+e.Path()] = true
	}

	for _, want := range forkOnlyRoutes {
		key := want.method + " " + want.path
		require.True(t, registered[key],
			"fork route %s is missing from the endpoints registry; it was dropped in an upstream sync port", key)
	}
}

// TestEndpoints_CallToolDeclaresForbidden pins the guard on the A2UI
// MCP round-trip route. The handler rejects any tool name outside its
// allow-list with 403, so the route's declared failure set must include
// 403 or the generated API docs under-describe the endpoint.
func TestEndpoints_CallToolDeclaresForbidden(t *testing.T) {
	t.Parallel()

	c := &controllerV1{}
	for _, e := range c.endpoints() {
		if e.Path() != "/v1/workspaces/{id}/mcp/call-tool" {
			continue
		}
		require.Contains(t, e.Errors(), 403,
			"mcp/call-tool must declare 403: the handler rejects non-A2UI tool names")
		require.Contains(t, e.Errors(), 400,
			"mcp/call-tool must declare 400: a malformed body is rejected before the allow-list check")
		return
	}
	t.Fatal("mcp/call-tool route not found in the registry")
}

// TestEndpoints_CallToolAllowListNonEmpty asserts the A2UI allow-list
// the call-tool handler consults is populated. An empty map would make
// the route reject every request with 403, which looks like a working
// 403 guard while actually disabling the feature.
func TestEndpoints_CallToolAllowListNonEmpty(t *testing.T) {
	t.Parallel()

	require.NotEmpty(t, a2uiCallableTools,
		"the A2UI callable-tool allow-list must not be empty, or every call is rejected")
}
