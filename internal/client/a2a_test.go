package client

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The proxy prefix is the workspace's a2a path on the server, addressed
// the way every other call is: the dummy host on a socket or pipe, the
// listen address on TCP (#421).
func TestA2AProxyBaseURL(t *testing.T) {
	t.Parallel()

	unix, err := NewClient(t.TempDir(), "unix", "/tmp/crush.sock")
	require.NoError(t, err)
	proxy := unix.A2AProxy("ws-1")
	require.Equal(t, "http://"+DummyHost+"/v1/workspaces/ws-1/a2a", proxy.BaseURL)
	require.Same(t, unix.h, proxy.HTTP, "calls ride the client's own connection to the server")

	tcp, err := NewClient(t.TempDir(), "tcp", "127.0.0.1:7777")
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:7777/v1/workspaces/a%2Fb/a2a", tcp.A2AProxy("a/b").BaseURL)
}
