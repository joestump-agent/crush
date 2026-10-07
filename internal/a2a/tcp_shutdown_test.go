package a2a

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requireClosedByServer reads from conn and requires the server to have
// closed it: the read ends in EOF or a reset, not in the read deadline,
// which is far longer than the server needs.
func requireClosedByServer(t *testing.T, conn net.Conn, r io.Reader, msg string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, err := io.Copy(io.Discard, r)
	var netErr net.Error
	if errors.As(err, &netErr) {
		require.False(t, netErr.Timeout(), "%s: the connection was left open", msg)
	}
}

// A peer that connects and sends nothing — on the TCP listener or the
// socket — does not hold Close (#358): net/http would wait five seconds
// for such a connection, and Close would end at its deadline with an
// error. The deadline here is shorter than five seconds, so a stall
// fails the test, while a working Close finishes far inside it.
func TestCloseNotStalledBySilentConnections(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	factory := NewServerFactory(t.TempDir(), WithTCPListener(pki.listenerOptions()))
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	var dialer net.Dialer
	silentTCP, err := dialer.DialContext(t.Context(), "tcp", factory.tcpAddr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = silentTCP.Close() })
	silentSock, err := dialer.DialContext(t.Context(), "unix", factory.socketPath())
	require.NoError(t, err)
	t.Cleanup(func() { _ = silentSock.Close() })

	// Each server accepts connections in order and registers one before
	// accepting the next, so a request that completes on a later
	// connection proves the silent one is accepted and tracked.
	status, _, _ := postOverTCP(t, pki.httpsClient(t, nil), "https://"+factory.tcpAddr()+"/agents/dispatch-1", factory.authToken(), sendMessageBody(t, "dispatch-session", "warm up"))
	require.Equal(t, http.StatusOK, status)
	sockClient := unixDialClient(factory)
	resp, err := postJSONRPCAuthed(t, factory, sockClient, "http://"+a2aURLHost+"/agents/dispatch-1", sendMessageBody(t, "dispatch-session", "warm up"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	sockClient.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, factory.Close(ctx), "silent connections must not hold Close to its deadline")

	requireClosedByServer(t, silentTCP, silentTCP, "TCP")
	requireClosedByServer(t, silentSock, silentSock, "socket")
}

// A request that never finishes is cut when Close's deadline passes
// (#358): the server is closed outright instead of leaving the
// connection open, and Close reports the deadline. The request here is
// stuck reading its body — the handler asked for it (100 Continue) and
// the client never sends it.
func TestCloseForcesStuckRequestAtDeadline(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	factory := NewServerFactory(t.TempDir(), WithTCPListener(pki.listenerOptions()))
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	pool := x509.NewCertPool()
	pool.AddCert(pki.ca)
	dialer := tls.Dialer{Config: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, ServerName: "127.0.0.1"}}
	conn, err := dialer.DialContext(t.Context(), "tcp", factory.tcpAddr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = fmt.Fprintf(conn, "POST /agents/dispatch-1 HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\nExpect: 100-continue\r\n\r\n",
		factory.tcpAddr(), factory.authToken())
	require.NoError(t, err)
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, "100 Continue", "the handler is reading the body")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, factory.Close(ctx), context.DeadlineExceeded)

	requireClosedByServer(t, conn, br, "stuck request")
}
