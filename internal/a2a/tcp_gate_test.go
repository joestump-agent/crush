package a2a

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"
)

// postOverTCP posts body to url with an optional bearer token and
// returns the status, the headers and the body of the answer.
func postOverTCP(t *testing.T, client *http.Client, url, token string, body []byte) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(bearerAuthorizationHeader, "Bearer "+token)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(data)
}

// countingBody is a request body of size bytes that records how many of
// them were read: prefix, then filler up to size. It never allocates the
// body, so a test can offer far more than it expects to be read.
type countingBody struct {
	prefix []byte
	size   int64

	mu   sync.Mutex
	read int64
}

func newCountingBody(prefix string, size int64) *countingBody {
	return &countingBody{prefix: []byte(prefix), size: size}
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.read >= b.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && b.read < b.size {
		if b.read < int64(len(b.prefix)) {
			p[n] = b.prefix[b.read]
		} else {
			p[n] = 'a'
		}
		n++
		b.read++
	}
	return n, nil
}

func (b *countingBody) bytesRead() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read
}

// logCapture is a slog sink a test can read and wait on: every write
// signals, so a test waits for a line without polling or sleeping.
type logCapture struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	wrote chan struct{}
}

func newLogCapture() *logCapture {
	return &logCapture{wrote: make(chan struct{}, 1)}
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	n, err := c.buf.Write(p)
	c.mu.Unlock()
	select {
	case c.wrote <- struct{}{}:
	default:
	}
	return n, err
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// logger returns a debug-level text logger writing to the capture.
func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// waitFor blocks until the captured log contains substr, failing the
// test if it does not appear within a generous deadline.
func (c *logCapture) waitFor(t *testing.T, substr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for !strings.Contains(c.String(), substr) {
		select {
		case <-c.wrote:
		case <-ctx.Done():
			t.Fatalf("log never contained %q; got:\n%s", substr, c.String())
		}
	}
}

// startTCPDispatchLogged is startTCPDispatch with the factory's logger
// replaced by the capture before the listener starts.
func startTCPDispatchLogged(t *testing.T, pki *testPKI, clientCA string, logs *logCapture, runner *fakeRunner) (*ServerFactory, *Server) {
	t.Helper()
	opts := pki.listenerOptions()
	opts.ClientCA = clientCA
	factory := NewServerFactory(t.TempDir(), WithTCPListener(opts))
	factory.logger = logs.logger()
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	return factory, server
}

// An unauthenticated TCP request is refused from its headers alone
// (#358): 401 before a byte of its body is read — not by the SDK's
// decoder, not by the unsupported-version answer — and the refusal is
// logged with the peer's address but never the credential it offered.
func TestTCPGateRejectsBeforeReadingBody(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	logs := newLogCapture()
	runner := &fakeRunner{result: textResult("done")}
	factory, _ := startTCPDispatchLogged(t, pki, "", logs, runner)
	addr := factory.tcpAddr()
	const offered = "not-the-host-token"

	for name, authorization := range map[string]string{
		"no credential": "",
		"wrong token":   "Bearer " + offered,
	} {
		for _, version := range []string{"", "99.0"} {
			body := newCountingBody(`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":"`, 8<<20)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+addr+"/agents/dispatch-1", body)
			req.Header.Set("Content-Type", "application/json")
			if authorization != "" {
				req.Header.Set(bearerAuthorizationHeader, authorization)
			}
			if version != "" {
				req.Header.Set(a2aspec.SvcParamVersion, version)
			}
			req.TLS = &tls.ConnectionState{}
			rec := httptest.NewRecorder()
			factory.tcp.serveHTTP(rec, req)

			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s, version %q", name, version)
			require.Zero(t, body.bytesRead(), "%s, version %q: the body must not be read", name, version)
			require.Equal(t, "close", rec.Header().Get("Connection"))
			require.NotEmpty(t, rec.Header().Get("WWW-Authenticate"))
		}
	}
	require.False(t, runner.ran)

	logged := logs.String()
	require.Contains(t, logged, "A2A TCP request rejected: unauthenticated")
	require.Contains(t, logged, "remote=192.0.2.1:1234", "the audit line names the peer")
	require.Contains(t, logged, "reason=\"invalid bearer token\"")
	require.Contains(t, logged, "reason=\"no credential\"")
	require.NotContains(t, logged, offered, "the offered credential is never logged")
	require.NotContains(t, logged, factory.authToken(), "the host token is never logged")
}

// An unauthenticated peer cannot tell a live route from an unknown one
// (#358): every path, known or not, gets the same 401.
func TestTCPGateSameAnswerForAnyRoute(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	factory, _ := startTCPDispatch(t, pki.listenerOptions(), &fakeRunner{result: textResult("done")})
	client := pki.httpsClient(t, nil)
	base := "https://" + factory.tcpAddr()
	body := sendMessageBody(t, "dispatch-session", "run the task")

	wantStatus, wantHeader, wantBody := postOverTCP(t, client, base+"/agents/dispatch-1", "", body)
	require.Equal(t, http.StatusUnauthorized, wantStatus)
	for _, path := range []string{"/agents/no-such-dispatch", "/agents/", "/", "/.well-known/agent-card.json"} {
		status, header, got := postOverTCP(t, client, base+path, "", body)
		require.Equal(t, wantStatus, status, "path %s", path)
		require.Equal(t, wantBody, got, "path %s", path)
		require.Equal(t, wantHeader.Get("WWW-Authenticate"), header.Get("WWW-Authenticate"), "path %s", path)
	}

	// With the token the routes are told apart again.
	status, _, _ := postOverTCP(t, client, base+"/agents/no-such-dispatch", factory.authToken(), body)
	require.Equal(t, http.StatusNotFound, status)
}

// Every JSON-RPC method is refused to an unauthenticated TCP peer
// (#358), not only SendMessage: reads, cancels and resubscribes too.
func TestTCPGateRefusesEveryMethod(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	runner := &fakeRunner{result: textResult("done")}
	factory, server := startTCPDispatch(t, pki.listenerOptions(), runner)
	client := pki.httpsClient(t, nil)
	url := server.Card.SupportedInterfaces[1].URL

	rpc := func(method string, params map[string]any) []byte {
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		require.NoError(t, err)
		return body
	}
	for method, params := range map[string]map[string]any{
		"GetTask":         {"id": "some-task"},
		"ListTasks":       {},
		"CancelTask":      {"id": "some-task"},
		"SubscribeToTask": {"id": "some-task"},
	} {
		for _, token := range []string{"", "not-the-token"} {
			status, _, _ := postOverTCP(t, client, url, token, rpc(method, params))
			require.Equal(t, http.StatusUnauthorized, status, "%s with token %q", method, token)
		}
	}

	// The token gets through the gate to the JSON-RPC layer.
	status, _, got := postOverTCP(t, client, url, factory.authToken(), rpc("ListTasks", map[string]any{}))
	require.Equal(t, http.StatusOK, status)
	require.NotContains(t, got, `"error"`)
	require.False(t, runner.ran)
}

// An authenticated TCP request body is capped (#358): a body past the
// cap is cut off after cap+1 bytes instead of being decoded whole.
func TestTCPRequestBodyCapped(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	runner := &fakeRunner{result: textResult("done")}
	factory, _ := startTCPDispatch(t, pki.listenerOptions(), runner)
	require.Equal(t, int64(maxTCPRequestBody), factory.tcp.maxBody)
	// No request reaches the live listener in this test; the cap is
	// lowered so the oversized body stays small.
	const limit = 4 << 10
	factory.tcp.maxBody = limit

	body := newCountingBody(`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","role":"ROLE_USER","contextId":"dispatch-session","parts":[{"text":"`, 1<<20)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+factory.tcpAddr()+"/agents/dispatch-1", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(bearerAuthorizationHeader, "Bearer "+factory.authToken())
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	factory.tcp.serveHTTP(rec, req)

	require.LessOrEqual(t, body.bytesRead(), int64(limit+1), "the body is read no further than the cap")
	require.False(t, runner.ran, "a truncated body never runs")
}

// The unsupported-version answer reads only a bounded prefix of the
// body to echo the request id (#358).
func TestVersionNotSupportedReadsBoundedBody(t *testing.T) {
	t.Parallel()

	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	body := newCountingBody(`{"jsonrpc":"2.0","id":7,"method":"SendMessage","params":"`, 4*versionProbeLimit)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+a2aURLHost+"/agents/dispatch-1", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(a2aspec.SvcParamVersion, "99.0")
	rec := httptest.NewRecorder()
	factory.serveHTTP(rec, req)

	require.LessOrEqual(t, body.bytesRead(), int64(versionProbeLimit))
	var rpcResp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rpcResp))
	require.Equal(t, -32009, rpcResp.Error.Code)
}

// The TCP server closes idle keep-alive connections and bounds the
// handshake and headers (#358).
func TestTCPListenerServerTimeouts(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	factory, _ := startTCPDispatch(t, pki.listenerOptions(), &fakeRunner{result: textResult("done")})
	require.Equal(t, tcpIdleTimeout, factory.tcp.server.IdleTimeout)
	require.Equal(t, tcpReadHeaderTimeout, factory.tcp.server.ReadHeaderTimeout)
}

// A failed TLS handshake is an audit line with the peer's address
// (#358): a client without a certificate on a mutual-TLS listener is
// visible in the log.
func TestTCPListenerLogsFailedHandshakes(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	logs := newLogCapture()
	_, server := startTCPDispatchLogged(t, pki, pki.caFile, logs, &fakeRunner{result: textResult("done")})

	requireRefusedHandshake(t, pki.httpsClient(t, nil), server.Card.SupportedInterfaces[1].URL, "a client without a certificate must be rejected")
	logs.waitFor(t, "TLS handshake error from 127.0.0.1:")
	require.Contains(t, logs.String(), "level=INFO")
}
