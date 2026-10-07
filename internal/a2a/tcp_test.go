package a2a

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
)

// testPKI is a throwaway certificate authority for the TCP listener
// tests (#358): a CA, a server certificate for localhost, 127.0.0.1 and
// ::1 written to disk the way a user configures one, and client
// certificates kept in memory for the HTTPS clients.
type testPKI struct {
	dir string

	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caFile string

	serverCert string
	serverKey  string
	serverLeaf *x509.Certificate
}

// newTestPKI generates the CA and the server certificate into a fresh
// temp directory.
func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	p := &testPKI{dir: t.TempDir()}
	p.ca, p.caKey = newTestCA(t, "crush test CA")
	p.caFile = filepath.Join(p.dir, "ca.pem")
	writePEM(t, p.caFile, "CERTIFICATE", p.ca.Raw)

	der, key := p.issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "crush a2a host"},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	p.serverLeaf = leaf
	p.serverCert = filepath.Join(p.dir, "server.pem")
	p.serverKey = filepath.Join(p.dir, "server-key.pem")
	writePEM(t, p.serverCert, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	writePEM(t, p.serverKey, "EC PRIVATE KEY", keyDER)
	return p
}

// newTestCA generates a self-signed CA certificate.
func newTestCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return ca, key
}

// issue signs tmpl with the PKI's CA under a fresh key.
func (p *testPKI) issue(t *testing.T, tmpl *x509.Certificate) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	return issueCert(t, p.ca, p.caKey, tmpl)
}

// issueCert signs tmpl with the given CA under a fresh key.
func issueCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, tmpl *x509.Certificate) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(24 * time.Hour)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	return der, key
}

// clientCert issues a client certificate for commonName, signed by ca.
func clientCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string) tls.Certificate {
	t.Helper()
	der, key := issueCert(t, ca, caKey, &x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// writePEM writes one PEM block to path.
func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// listenerOptions is the options.a2a block for a loopback listener on a
// kernel-picked port, without mutual TLS.
func (p *testPKI) listenerOptions() *config.A2AOptions {
	return &config.A2AOptions{
		Listen:  "127.0.0.1:0",
		TLSCert: p.serverCert,
		TLSKey:  p.serverKey,
	}
}

// httpsClient returns a client that trusts the PKI's CA and presents
// cert when one is given. Its idle connections close before the test's
// factory does, so no connection outlives the test.
func (p *testPKI) httpsClient(t *testing.T, cert *tls.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	transport := &http.Transport{TLSClientConfig: cfg}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

// startTCPDispatch starts a factory with the TCP listener configured by
// opts and serves one dispatch on it.
func startTCPDispatch(t *testing.T, opts *config.A2AOptions, runner *fakeRunner) (*ServerFactory, *Server) {
	t.Helper()
	factory := NewServerFactory(t.TempDir(), WithTCPListener(opts))
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
		Name:       "tester",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	return factory, server
}

// bindRemote clears LocalOnly on a served dispatch's context binding —
// the shape a remotely callable agent from the peer registry (#334) will
// have — so a test can drive a full run over the TCP listener. Every
// binding StartServer makes is local-only (#358).
func bindRemote(t *testing.T, factory *ServerFactory, contextID string) {
	t.Helper()
	binding, ok := factory.contexts.Lookup(contextID)
	require.True(t, ok)
	require.True(t, binding.LocalOnly, "StartServer binds local-only")
	binding.LocalOnly = false
	factory.contexts.Bind(contextID, binding)
}

// rpcError is the JSON-RPC error a rejected call answers with.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// sendOverTCP posts a message/send for the dispatch to the TCP listener
// and decodes the JSON-RPC answer. token, when set, rides as the bearer
// credential; host, when set, replaces the request's Host header.
func sendOverTCP(t *testing.T, client *http.Client, url, token, host string) (*a2aspec.Task, *rpcError) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(bearerAuthorizationHeader, "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rpcResp struct {
		Result struct {
			Task *a2aspec.Task `json:"task"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
	return rpcResp.Result.Task, rpcResp.Error
}

// requireRefusedHandshake posts a message/send to url and requires the
// request to fail: the TLS handshake, not a route, refuses the client.
// The exact error is not asserted — under TLS 1.3 the server's alert can
// arrive as a reset connection instead.
func requireRefusedHandshake(t *testing.T, client *http.Client, url, msg string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, msg)
}

// With no TCP config there is no TCP listener (#358): the host serves
// its unix socket alone and the card lists only the socket interface.
func TestTCPListenerOffByDefault(t *testing.T) {
	t.Parallel()

	for name, opts := range map[string][]ServerFactoryOption{
		"no option":     nil,
		"nil options":   {WithTCPListener(nil)},
		"empty options": {WithTCPListener(&config.A2AOptions{})},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			factory := NewServerFactory(t.TempDir(), opts...)
			server, err := factory.StartServer(t.Context(), ServerParams{
				DispatchID: "dispatch-1",
				Runner:     &fakeRunner{result: textResult("done")},
				SessionID:  "dispatch-session",
				ContextID:  "dispatch-session",
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = factory.Close(context.Background()) })

			require.Empty(t, factory.tcpAddr(), "no TCP listener without config")
			require.Len(t, server.Card.SupportedInterfaces, 1, "the card lists only the socket")
			require.Equal(t, server.Endpoint, server.Card.SupportedInterfaces[0].URL)
			require.NotContains(t, server.Card.SecuritySchemes, mtlsSchemeName)
		})
	}
}

// A listen address without TLS material never becomes a plain TCP
// listener (#358): load validation refuses it, and the host — handed
// one anyway — serves its socket alone.
func TestTCPListenerNeverServesPlainTCP(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{result: textResult("done")}
	factory, server := startTCPDispatch(t, &config.A2AOptions{Listen: "127.0.0.1:0"}, runner)

	require.Empty(t, factory.tcpAddr(), "no TCP listener without tls_cert and tls_key")
	require.Len(t, server.Card.SupportedInterfaces, 1)

	resp, err := postJSONRPCAuthed(t, factory, unixDialClient(factory), server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the socket still serves")
	require.True(t, runner.ran)
}

// The TCP listener serves a full dispatch over TLS (#358): the card
// lists the HTTPS interface after the socket one, and the production
// dispatch client — bearer token and all — streams the dispatch to its
// terminal state through that interface alone.
func TestTCPListenerServesDispatchOverTLS(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	runner := &fakeRunner{result: textResult("done over tls")}
	factory, server := startTCPDispatch(t, pki.listenerOptions(), runner)
	bindRemote(t, factory, "dispatch-session")

	addr := factory.tcpAddr()
	require.NotEmpty(t, addr)
	require.Len(t, server.Card.SupportedInterfaces, 2, "the socket and the HTTPS interface")
	require.Equal(t, server.Endpoint, server.Card.SupportedInterfaces[0].URL, "the socket stays first")
	httpsIface := server.Card.SupportedInterfaces[1]
	require.Equal(t, "https://"+addr+"/agents/dispatch-1", httpsIface.URL)
	require.Equal(t, a2aspec.TransportProtocolJSONRPC, httpsIface.ProtocolBinding)

	// Dispatch through the HTTPS interface only: a card that lists the
	// socket first would let the client stay on the socket.
	httpsCard := *server.Card
	httpsCard.SupportedInterfaces = []*a2aspec.AgentInterface{httpsIface}
	factory.httpClient = pki.httpsClient(t, nil)

	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint:  server.Endpoint,
		Card:      &httpsCard,
		Prompt:    "fix the bug over tls",
		ContextID: "dispatch-session",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "done over tls", outcome.Text)
	require.Equal(t, "fix the bug over tls", runner.gotCall.Prompt)
}

// The TCP listener is TLS only (#358): plain HTTP to the same port is
// answered with the TLS server's 400 and never reaches a route.
func TestTCPListenerRejectsPlainHTTP(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	runner := &fakeRunner{result: textResult("done")}
	factory, _ := startTCPDispatch(t, pki.listenerOptions(), runner)

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	plain := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+factory.tcpAddr()+"/agents/dispatch-1", bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(bearerAuthorizationHeader, "Bearer "+factory.authToken())
	resp, err := plain.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, string(body), "HTTPS server")
	require.False(t, runner.ran, "plain HTTP must never reach the runner")
}

// Without a token a TCP call is rejected (#358): there are no peer
// credentials to fall back on, so the call is never treated as the
// host's own user — on a dispatch route or on a definition route, which
// the socket serves without credentials. The listener answers 401 before
// the JSON-RPC layer sees the request.
func TestTCPListenerRequiresBearerToken(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	runner := &fakeRunner{result: textResult("done")}
	factory, server := startTCPDispatch(t, pki.listenerOptions(), runner)
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), agent.AgentDefinitionCard{ID: "coder", Name: "Coder"}))
	client := pki.httpsClient(t, nil)
	base := "https://" + factory.tcpAddr()

	for name, token := range map[string]string{
		"no token":    "",
		"wrong token": "not-the-token",
	} {
		for _, url := range []string{server.Card.SupportedInterfaces[1].URL, base + agentsPathPrefix + "coder"} {
			status, _, _ := postOverTCP(t, client, url, token, sendMessageBody(t, "dispatch-session", "run the task"))
			require.Equal(t, http.StatusUnauthorized, status, "%s on %s must be rejected", name, url)
		}
	}
	require.False(t, runner.ran, "an unauthenticated TCP call must not reach the runner")

	task, rpcErr := sendOverTCP(t, client, base+agentsPathPrefix+"coder", factory.authToken(), "")
	require.Nil(t, rpcErr, "the token passes a definition route over TCP")
	require.Equal(t, a2aspec.TaskStateRejected, task.Status.State, "and the unbound route rejects the run")
}

// A Host that is neither the listen address nor a SAN of the server
// certificate is a DNS-rebinding or misdirected request (#358): 400
// before any dispatch work. A SAN name passes.
func TestTCPListenerRejectsWrongHost(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	runner := &fakeRunner{result: textResult("done")}
	factory, server := startTCPDispatch(t, pki.listenerOptions(), runner)
	bindRemote(t, factory, "dispatch-session")
	client := pki.httpsClient(t, nil)
	url := server.Card.SupportedInterfaces[1].URL
	_, port, err := net.SplitHostPort(factory.tcpAddr())
	require.NoError(t, err)

	for _, host := range []string{"rebind.evil.example:" + port, "rebind.evil.example", a2aURLHost} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(bearerAuthorizationHeader, "Bearer "+factory.authToken())
		req.Host = host
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "Host %q must be rejected", host)
	}
	require.False(t, runner.ran, "a wrong Host must not reach the runner")

	task, rpcErr := sendOverTCP(t, client, url, factory.authToken(), "localhost:"+port)
	require.Nil(t, rpcErr, "a DNS SAN of the server certificate is a valid Host")
	require.Equal(t, a2aspec.TaskStateCompleted, task.Status.State)
}

// With client_ca the TCP listener requires a verified client certificate
// (#358): a client without one, or with one from another CA, fails the
// handshake; a client with one is authenticated by it — no bearer token
// needed — and its tasks are stored under the certificate's identity.
func TestTCPListenerMutualTLS(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	opts := pki.listenerOptions()
	opts.ClientCA = pki.caFile
	runner := &fakeRunner{result: textResult("done")}
	factory, server := startTCPDispatch(t, opts, runner)
	bindRemote(t, factory, "dispatch-session")
	url := server.Card.SupportedInterfaces[1].URL

	require.Contains(t, server.Card.SecuritySchemes, mtlsSchemeName, "the card declares mutual TLS")
	require.Len(t, server.Card.SecurityRequirements, 2, "bearer or a client certificate")

	requireRefusedHandshake(t, pki.httpsClient(t, nil), url, "a client without a certificate must be rejected")
	otherCA, otherKey := newTestCA(t, "someone else's CA")
	foreign := clientCert(t, otherCA, otherKey, "intruder")
	requireRefusedHandshake(t, pki.httpsClient(t, &foreign), url, "a certificate from another CA must be rejected")
	require.False(t, runner.ran)

	cert := clientCert(t, pki.ca, pki.caKey, "peer-host")
	client := pki.httpsClient(t, &cert)
	task, rpcErr := sendOverTCP(t, client, url, "", "")
	require.Nil(t, rpcErr, "a verified client certificate authenticates without a token")
	require.Equal(t, a2aspec.TaskStateCompleted, task.Status.State)
	require.True(t, runner.ran)

	// The task belongs to the certificate's identity: the same holder
	// lists it.
	listBody, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ListTasks", "params": map[string]any{}})
	require.NoError(t, err)
	resp, err := postJSONRPC(t, client, url, listBody)
	require.NoError(t, err)
	defer resp.Body.Close()
	var listResp struct {
		Result struct {
			Tasks []*a2aspec.Task `json:"tasks"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listResp))
	require.Nil(t, listResp.Error)
	require.Len(t, listResp.Result.Tasks, 1)
	require.Equal(t, task.ID, listResp.Result.Tasks[0].ID)
}

// steerCountingRunner is a paced runner that counts the steers and
// cancels it is offered, refusing every steer.
type steerCountingRunner struct {
	*pacedRunner
	steers  atomic.Int32
	cancels atomic.Int32
}

func (r *steerCountingRunner) EnqueueWhenBusy(agent.SessionAgentCall) bool {
	r.steers.Add(1)
	return false
}

func (r *steerCountingRunner) Cancel(string) { r.cancels.Add(1) }

// The TCP listener exposes no local run (#358): an authenticated remote
// caller that knows a live local dispatch's context ID gets the same
// rejection as an unknown context, and the local run is untouched — no
// steer reaches it and it is not canceled.
func TestTCPListenerCannotReachLocalContexts(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	opts := pki.listenerOptions()
	opts.ClientCA = pki.caFile
	runner := &steerCountingRunner{pacedRunner: newPacedRunner("local done")}
	release := sync.OnceFunc(func() { close(runner.release) })
	factory := NewServerFactory(t.TempDir(), WithTCPListener(opts))
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	t.Cleanup(release)

	// The local run: the coordinator's own dispatch, over the socket.
	var outcome agent.DispatchTransportOutcome
	streamErr := make(chan error, 1)
	go func() {
		var err error
		outcome, err = factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
			Endpoint:  server.Endpoint,
			Card:      server.Card,
			Prompt:    "local work",
			ContextID: "dispatch-session",
		})
		streamErr <- err
	}()
	<-runner.started

	cert := clientCert(t, pki.ca, pki.caKey, "peer-host")
	remote := pki.httpsClient(t, &cert)
	task, rpcErr := sendOverTCP(t, remote, server.Card.SupportedInterfaces[1].URL, "", "")
	require.Nil(t, rpcErr)
	require.NotNil(t, task)
	require.Equal(t, a2aspec.TaskStateRejected, task.Status.State)
	require.NotNil(t, task.Status.Message)
	require.Contains(t, partsText(task.Status.Message.Parts), "no running agent for context dispatch-session")
	require.Zero(t, runner.steers.Load(), "no steer reaches the local run")
	require.Zero(t, runner.cancels.Load(), "the local run is not canceled")

	release()
	require.NoError(t, <-streamErr)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "local done", outcome.Text)
	require.Equal(t, 1, runner.ranCount)
	require.Equal(t, "local work", runner.gotCall.Prompt)
}

// A client certificate's identity is its issuer and subject (#358), so
// two CAs issuing the same subject name two identities, and a
// certificate without a subject is named by its fingerprint.
func TestCertIdentity(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	otherCA, otherKey := newTestCA(t, "second CA")
	first := clientCert(t, pki.ca, pki.caKey, "peer-host")
	second := clientCert(t, otherCA, otherKey, "peer-host")

	require.Equal(t, "CN=crush test CA/CN=peer-host", certIdentity(first.Leaf))
	require.Equal(t, "CN=second CA/CN=peer-host", certIdentity(second.Leaf))

	der, _ := pki.issue(t, &x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	bare, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	require.Regexp(t, `^CN=crush test CA/sha256:[0-9a-f]{64}$`, certIdentity(bare))
}

// Two CAs in one client_ca bundle that issue the same subject do not
// share a task namespace (#358): each holder lists only its own tasks
// and cannot read the other's.
func TestTCPListenerMutualTLSSeparatesCAs(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	otherCA, otherKey := newTestCA(t, "second CA")
	bundle := filepath.Join(pki.dir, "bundle.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.ca.Raw})
	data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherCA.Raw})...)
	require.NoError(t, os.WriteFile(bundle, data, 0o600))

	opts := pki.listenerOptions()
	opts.ClientCA = bundle
	_, server := startTCPDispatch(t, opts, &fakeRunner{result: textResult("done")})
	url := server.Card.SupportedInterfaces[1].URL

	firstCert := clientCert(t, pki.ca, pki.caKey, "peer-host")
	secondCert := clientCert(t, otherCA, otherKey, "peer-host")
	first := pki.httpsClient(t, &firstCert)
	second := pki.httpsClient(t, &secondCert)

	task, rpcErr := sendOverTCP(t, first, url, "", "")
	require.Nil(t, rpcErr)
	require.NotNil(t, task)

	listTasks := func(client *http.Client) []*a2aspec.Task {
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ListTasks", "params": map[string]any{}})
		require.NoError(t, err)
		resp, err := postJSONRPC(t, client, url, body)
		require.NoError(t, err)
		defer resp.Body.Close()
		var listResp struct {
			Result struct {
				Tasks []*a2aspec.Task `json:"tasks"`
			} `json:"result"`
			Error any `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&listResp))
		require.Nil(t, listResp.Error)
		return listResp.Result.Tasks
	}
	require.Len(t, listTasks(first), 1, "the holder sees its own task")
	require.Empty(t, listTasks(second), "the same subject from another CA sees nothing")

	getBody, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "GetTask", "params": map[string]any{"id": task.ID}})
	require.NoError(t, err)
	resp, err := postJSONRPC(t, second, url, getBody)
	require.NoError(t, err)
	defer resp.Body.Close()
	var getResp struct {
		Result *a2aspec.Task `json:"result"`
		Error  *rpcError     `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&getResp))
	require.NotNil(t, getResp.Error, "another CA's holder cannot read the task")
	require.Nil(t, getResp.Result)
}

// Close shuts both listeners (#358): the socket file is gone and the
// TCP listener accepts nothing more.
func TestTCPListenerClosedOnShutdown(t *testing.T) {
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
	factory.mu.Lock()
	tcp := factory.tcp
	factory.mu.Unlock()
	require.NotNil(t, tcp)
	sock := factory.socketPath()

	require.NoError(t, factory.Close(context.Background()))

	_, err = tcp.listener.Accept()
	require.ErrorIs(t, err, net.ErrClosed, "the TCP listener is closed")
	select {
	case <-tcp.done:
	default:
		t.Fatal("the TCP Serve goroutine must have returned")
	}
	_, err = os.Stat(sock)
	require.True(t, errors.Is(err, os.ErrNotExist), "the socket file is removed")
}

// The Host check (#358) as a pure decision: the listen address as
// configured, bound and advertised, and any DNS or IP SAN of the server
// certificate, whatever its port; nothing else.
func TestTCPHostAllowed(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	h := &tcpHost{
		listen:     "127.0.0.1:0",
		bound:      "127.0.0.1:5555",
		advertised: "127.0.0.1:5555",
		leaf:       pki.serverLeaf,
	}
	for host, want := range map[string]bool{
		"127.0.0.1:5555":       true,
		"127.0.0.1":            true, // An IP SAN, whatever the port.
		"localhost:5555":       true,
		"LOCALHOST:5555":       true,
		"localhost":            true,
		"[::1]:5555":           true,
		"[::1]":                true,
		"":                     false,
		"crush-a2a":            false,
		"rebind.evil.example":  false,
		"127.0.0.2:5555":       false,
		"localhost.evil.com":   false,
		"evil.example:5555":    false,
		"127.0.0.1:5555:extra": false,
	} {
		require.Equal(t, want, h.hostAllowed(host), "Host %q", host)
	}

	// A configured host name with port 0 matches once advertised with
	// the bound port, even when it is not a SAN.
	named := &tcpHost{
		listen:     "a2a.internal:0",
		bound:      "10.0.0.5:5555",
		advertised: "a2a.internal:5555",
		leaf:       pki.serverLeaf,
	}
	require.True(t, named.hostAllowed("a2a.internal:5555"))
	require.True(t, named.hostAllowed("10.0.0.5:5555"))
	require.False(t, named.hostAllowed("a2a.internal:6666"))

	// A wildcard listen address names no host.
	wildcard := &tcpHost{listen: "0.0.0.0:7443", bound: "0.0.0.0:7443", advertised: "localhost:7443", leaf: pki.serverLeaf}
	require.False(t, wildcard.hostAllowed("0.0.0.0:7443"))
	require.True(t, wildcard.hostAllowed("localhost:7443"))
}

// The advertised address keeps a specific listen host with the bound
// port, and replaces a wildcard with the certificate's first
// non-wildcard DNS SAN, else its first specific IP SAN (#358). With
// neither there is nothing to advertise.
func TestAdvertisedAddr(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	advertised := func(listen, bound string, leaf *x509.Certificate) string {
		t.Helper()
		addr, ok := advertisedAddr(listen, bound, leaf)
		require.True(t, ok, "%s bound at %s", listen, bound)
		return addr
	}
	require.Equal(t, "127.0.0.1:5555", advertised("127.0.0.1:0", "127.0.0.1:5555", pki.serverLeaf))
	require.Equal(t, "a2a.internal:7443", advertised("a2a.internal:7443", "10.0.0.5:7443", pki.serverLeaf))
	require.Equal(t, "localhost:7443", advertised("0.0.0.0:7443", "0.0.0.0:7443", pki.serverLeaf))
	require.Equal(t, "localhost:7443", advertised(":7443", "[::]:7443", pki.serverLeaf))

	ipOnly := &x509.Certificate{
		DNSNames:    []string{"*.example.com"},
		IPAddresses: []net.IP{net.IPv4zero, net.ParseIP("192.0.2.10")},
	}
	require.Equal(t, "192.0.2.10:7443", advertised("0.0.0.0:7443", "0.0.0.0:7443", ipOnly),
		"a wildcard DNS SAN is skipped for the first specific IP SAN")

	for _, leaf := range []*x509.Certificate{{}, {DNSNames: []string{"*.example.com"}, IPAddresses: []net.IP{net.IPv6unspecified}}} {
		addr, ok := advertisedAddr(":7443", "[::]:7443", leaf)
		require.False(t, ok, "no dialable SAN behind a wildcard listen address")
		require.Empty(t, addr)
	}
}

// A listener with no address to advertise lists no HTTPS interface on
// the cards (#358), rather than one no client could dial.
func TestTCPEndpointWithoutAdvertisedAddress(t *testing.T) {
	t.Parallel()

	factory := NewServerFactory(t.TempDir())
	factory.tcp = &tcpHost{bound: "[::]:7443", advertised: ""}
	url, _ := factory.tcpEndpoint("dispatch-1")
	require.Empty(t, url)

	factory.tcp = &tcpHost{bound: "[::]:7443", advertised: "localhost:7443", mutualTLS: true}
	url, mutual := factory.tcpEndpoint("dispatch-1")
	require.Equal(t, "https://localhost:7443/agents/dispatch-1", url)
	require.True(t, mutual)
}

// The listener warns at startup (#358) when it has no address to
// advertise, and when it is reachable from other machines but takes
// only this process's bearer token, which never leaves the process.
func TestTCPListenerStartupWarnings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		host         tcpHost
		noAdvertised bool
		tokenOnly    bool
	}{
		{name: "loopback without client_ca", host: tcpHost{listen: "127.0.0.1:7443", advertised: "127.0.0.1:7443"}},
		{name: "localhost without client_ca", host: tcpHost{listen: "localhost:7443", advertised: "localhost:7443"}},
		{name: "IPv6 loopback without client_ca", host: tcpHost{listen: "[::1]:7443", advertised: "[::1]:7443"}},
		{name: "wildcard with client_ca", host: tcpHost{listen: "0.0.0.0:7443", advertised: "localhost:7443", mutualTLS: true}},
		{name: "wildcard without client_ca", host: tcpHost{listen: "0.0.0.0:7443", advertised: "localhost:7443"}, tokenOnly: true},
		{name: "empty host without client_ca", host: tcpHost{listen: ":7443", advertised: "localhost:7443"}, tokenOnly: true},
		{name: "public name without client_ca", host: tcpHost{listen: "a2a.example.com:7443", advertised: "a2a.example.com:7443"}, tokenOnly: true},
		{name: "nothing to advertise", host: tcpHost{listen: "0.0.0.0:7443", mutualTLS: true}, noAdvertised: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logs := newLogCapture()
			h := tc.host
			h.logger = logs.logger()
			h.logStartupWarnings()
			out := logs.String()
			if tc.noAdvertised {
				require.Contains(t, out, "no address to advertise")
			} else {
				require.NotContains(t, out, "no address to advertise")
			}
			if tc.tokenOnly {
				require.Contains(t, out, "remote clients cannot authenticate without options.a2a.client_ca")
				require.Contains(t, out, "level=WARN")
			} else {
				require.NotContains(t, out, "client_ca")
			}
		})
	}
}

// The TCP half of the auth decision (#358): a verified client
// certificate or the bearer token, and never the socket's local-user
// rule — a remote call with the host's own uid and no credential is
// still rejected.
func TestAuthorizeRemote(t *testing.T) {
	t.Parallel()

	base := authDecision{
		token:   "right",
		wantUID: "1000",
		remote:  true,
	}

	withToken := base
	withToken.authorization = []string{"Bearer right"}
	require.NoError(t, withToken.authorize())
	require.Equal(t, "crush", withToken.userName(), "the token holder carries no uid over TCP")

	require.ErrorIs(t, base.authorize(), a2aspec.ErrUnauthenticated, "no credential at all")

	wrongToken := base
	wrongToken.authorization = []string{"Bearer wrong"}
	require.ErrorIs(t, wrongToken.authorize(), a2aspec.ErrUnauthenticated)

	localLooking := base
	localLooking.peerKnown = true
	localLooking.peerUID = "1000"
	require.ErrorIs(t, localLooking.authorize(), a2aspec.ErrUnauthenticated,
		"a TCP call never authenticates as the local user")

	withCert := base
	withCert.certVerified = true
	withCert.certIdentity = "CN=peer-host"
	require.NoError(t, withCert.authorize())
	require.Equal(t, "mtls:CN=peer-host", withCert.userName())

	unverifiedCert := base
	unverifiedCert.certIdentity = "CN=peer-host"
	require.ErrorIs(t, unverifiedCert.authorize(), a2aspec.ErrUnauthenticated,
		"only a verified certificate authenticates")
}
