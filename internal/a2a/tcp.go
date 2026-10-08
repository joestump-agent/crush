package a2a

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
)

// a2aTLSConfigPath names the config block the TCP listener's settings
// come from, for the errors its TLS material reports (#358).
const a2aTLSConfigPath = "options.a2a"

// defaultHTTPSPort is the port a Host header without one implies on the
// TLS listener (#358).
const defaultHTTPSPort = "443"

// maxTCPRequestBody caps a TCP request body (#358). The SDK decodes a
// JSON-RPC body whole, in memory, before anything else runs, so the cap
// bounds what even an authenticated peer can make the host buffer. It is
// sized for the largest legitimate request, a steer carrying pasted
// attachments: the TUI caps one attachment at 5 MB, base64 inside the
// JSON inflates that by 4/3 to about 6.7 MB, and 32 MiB holds four
// full-size attachments plus the prompt and the envelope.
const maxTCPRequestBody = 32 << 20

// tcpReadHeaderTimeout bounds the TLS handshake and the request headers
// on the TCP listener (#358); net/http applies it to both.
const tcpReadHeaderTimeout = 30 * time.Second

// tcpIdleTimeout closes a keep-alive TCP connection that has carried no
// request for this long (#358). A stream is an active request, so it is
// never cut by it.
const tcpIdleTimeout = 2 * time.Minute

// WithTCPListener configures the host's optional TCP listener (#358):
// when opts sets a listen address, the host also serves every route over
// TLS on that address, beside its unix socket. Plain TCP is never
// served — without both TLS files the listener does not start. A nil or
// empty opts keeps the host on its unix socket alone.
func WithTCPListener(opts *config.A2AOptions) ServerFactoryOption {
	return func(f *ServerFactory) {
		if !opts.Enabled() {
			return
		}
		cp := *opts
		f.tcpOpts = &cp
	}
}

// remotePeerContextKey marks a served call that arrived over the TCP
// listener (#358). The auth interceptor reads it to apply the TCP rule —
// a verified client certificate or the bearer token — instead of the
// socket's local-user rule.
type remotePeerContextKey struct{}

// remotePeer is what the TCP listener knows about a caller: whether its
// client certificate was verified against client_ca, and whose it is.
// It never carries a uid; TCP has no peer credentials.
type remotePeer struct {
	certVerified bool
	certIdentity string
}

// remotePeerFromContext returns the TCP caller stamped on the serve
// context; ok is false for a call that arrived on the unix socket.
func remotePeerFromContext(ctx context.Context) (remotePeer, bool) {
	peer, ok := ctx.Value(remotePeerContextKey{}).(remotePeer)
	return peer, ok
}

// hostListener is what the shared request middleware needs to know about
// the listener a request arrived on: which Host headers it answers to,
// and what it adds to the route's serve context for the auth interceptor.
type hostListener interface {
	hostAllowed(host string) bool
	serveContext(routeCtx context.Context, r *http.Request) context.Context
}

// socketListener is the unix socket's side of the middleware (#346): the
// only Host is the internal crush-a2a label, and the peer's uid — where
// the platform reported one on the connection (#357) — is copied onto
// the route context.
type socketListener struct{}

func (socketListener) hostAllowed(host string) bool { return host == a2aURLHost }

func (socketListener) serveContext(routeCtx context.Context, r *http.Request) context.Context {
	if uid, ok := peerUIDFromContext(r.Context()); ok {
		return context.WithValue(routeCtx, peerUIDContextKey{}, uid)
	}
	return routeCtx
}

// tcpHost is the host's optional TLS-only TCP listener (#358): its own
// HTTP server over the factory's route table and middleware, with a Host
// check of its own and the TCP caller stamped on every serve context.
// Every field is set before Serve starts and never changes after.
type tcpHost struct {
	factory  *ServerFactory
	listener net.Listener
	server   *http.Server
	done     chan struct{}

	// listen is the configured address, bound the address the listener
	// actually bound (they differ for port 0), and advertised the
	// host:port the cards list — empty when there is none to dial.
	listen     string
	bound      string
	advertised string

	// leaf is the server certificate; its DNS and IP SANs are Host
	// names the listener answers to.
	leaf *x509.Certificate

	// mutualTLS reports that client certificates are required and
	// verified against client_ca.
	mutualTLS bool

	// maxBody caps a request body (maxTCPRequestBody; tests lower it).
	maxBody int64

	// logger receives the listener's audit lines: rejected requests and,
	// through the server's ErrorLog, failed handshakes.
	logger *slog.Logger
}

var _ hostListener = (*tcpHost)(nil)

// startTCPHost binds the TCP listener and starts serving TLS on it
// (#358). The TLS material loads through the same config helper load
// validation runs, so a config that loaded is one this can serve. The
// listener is bound with a context-aware ListenConfig and wrapped by
// tls.NewListener, which is what tls.Listen does.
func startTCPHost(ctx context.Context, f *ServerFactory, opts *config.A2AOptions) (*tcpHost, error) {
	tlsCfg, err := opts.ServerTLSConfig(a2aTLSConfigPath)
	if err != nil {
		return nil, err
	}
	leaf, err := leafCertificate(tlsCfg.Certificates[0])
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	raw, err := lc.Listen(ctx, "tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("a2a: bind tcp listener: %w", err)
	}
	t := &tcpHost{
		factory:   f,
		listener:  tls.NewListener(raw, tlsCfg),
		done:      make(chan struct{}),
		listen:    opts.Listen,
		bound:     raw.Addr().String(),
		leaf:      leaf,
		mutualTLS: tlsCfg.ClientAuth == tls.RequireAndVerifyClientCert,
		maxBody:   maxTCPRequestBody,
		logger:    f.log(),
	}
	t.advertised, _ = advertisedAddr(t.listen, t.bound, leaf)
	t.logStartupWarnings()
	t.server = &http.Server{
		Handler:           http.HandlerFunc(t.serveHTTP),
		ReadHeaderTimeout: tcpReadHeaderTimeout,
		IdleTimeout:       tcpIdleTimeout,
		// Failed handshakes — a scanner, a client without a certificate
		// or with one from another CA, plain HTTP — are audit lines with
		// the peer's address, logged rather than written to stderr under
		// the TUI.
		ErrorLog: slog.NewLogLogger(t.logger.Handler(), slog.LevelInfo),
	}
	closeSilentConnsOnShutdown(t.server)
	go func() {
		defer close(t.done)
		if err := t.server.Serve(t.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.logger.Error("A2A TCP listener died early", "error", err)
		}
	}()
	t.logger.Info("A2A host listening on TCP", "addr", t.bound, "url", t.baseURL(), "mutual_tls", t.mutualTLS)
	return t, nil
}

// leafCertificate returns the parsed server certificate, which
// tls.LoadX509KeyPair normally fills in already.
func leafCertificate(cert tls.Certificate) (*x509.Certificate, error) {
	if cert.Leaf != nil {
		return cert.Leaf, nil
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("a2a: tls certificate is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("a2a: parse tls certificate: %w", err)
	}
	return leaf, nil
}

// baseURL is the listener's https:// origin as the cards advertise it,
// or empty when it has no address to advertise.
func (t *tcpHost) baseURL() string {
	if t.advertised == "" {
		return ""
	}
	return "https://" + t.advertised
}

// close shuts the TCP listener down within ctx: it stops accepting,
// closes connections that never sent a request, drains in-flight
// requests — closing the server outright if ctx ends first — closes the
// listener and reaps the Serve goroutine.
func (t *tcpHost) close(ctx context.Context) error {
	err := shutdownServer(ctx, t.server)
	if cerr := t.listener.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
		err = cerr
	}
	<-t.done
	return err
}

// serveHTTP is the TCP listener's root handler (#358). It authenticates
// first, from the connection and the headers alone: a request without a
// verified client certificate or the bearer token is answered 401 before
// a byte of its body is read and before the route table is consulted, so
// an unauthenticated peer can neither make the host decode a body nor
// tell a live route from an unknown one. An authenticated request's body
// is capped, then it takes the factory's middleware and route table with
// this listener's Host check and caller.
func (t *tcpHost) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !t.authenticated(r) {
		reason := "no credential"
		if len(r.Header.Values(bearerAuthorizationHeader)) > 0 {
			reason = "invalid bearer token"
		}
		// The audit line names the peer and why, never the credential.
		t.logger.Warn("A2A TCP request rejected: unauthenticated",
			"remote", r.RemoteAddr, "reason", reason, "mutual_tls", t.mutualTLS)
		w.Header().Set("WWW-Authenticate", `Bearer realm="crush-a2a"`)
		// Closing the connection keeps net/http from draining the
		// unread body for keep-alive after the handler returns.
		w.Header().Set("Connection", "close")
		http.Error(w, "a2a: unauthenticated", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, t.maxBody)
	t.factory.serveRequest(w, r, t)
}

// authenticated is the TCP listener's gate (#358): a client certificate
// the handshake verified against client_ca, or exactly one Authorization
// header carrying the host's bearer token, compared in constant time.
// The route's call interceptor checks again after the body is decoded.
func (t *tcpHost) authenticated(r *http.Request) bool {
	if t.verifiedChain(r) != nil {
		return true
	}
	return authDecision{
		token:         t.factory.authToken(),
		authorization: r.Header.Values(bearerAuthorizationHeader),
	}.tokenMatches()
}

// verifiedCert returns the client certificate the handshake verified
// against client_ca, or nil without mutual TLS or a verified chain.
func (t *tcpHost) verifiedChain(r *http.Request) []*x509.Certificate {
	if !t.mutualTLS || r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return nil
	}
	return r.TLS.VerifiedChains[0]
}

// hostAllowed implements [hostListener] (#358): a Host is accepted when
// it names the listen address — as configured, as bound, or as the cards
// advertise it — or when its host name is a DNS or IP SAN of the server
// certificate. Anything else is a DNS-rebinding or misdirected request.
func (t *tcpHost) hostAllowed(host string) bool {
	if host == "" {
		return false
	}
	for _, addr := range []string{t.listen, t.bound, t.advertised} {
		if sameAuthority(host, addr) {
			return true
		}
	}
	name, _ := splitAuthority(host)
	return t.leaf.VerifyHostname(name) == nil
}

// serveContext implements [hostListener]: every TCP call is marked
// remote, so the auth interceptor never treats it as the local user, and
// carries the client certificate the handshake verified, if any.
func (t *tcpHost) serveContext(routeCtx context.Context, r *http.Request) context.Context {
	var peer remotePeer
	if chain := t.verifiedChain(r); chain != nil {
		peer.certVerified = true
		peer.certIdentity = certIdentity(chain)
	}
	return context.WithValue(routeCtx, remotePeerContextKey{}, peer)
}

// certIdentity names a verified client certificate "ca-sha256:<ca>/
// <subject>" (#358): ca is the SHA-256 fingerprint of the client_ca
// certificate its chain verified against, the last in the chain. A
// client_ca bundle can hold several CAs, and two of them can issue the
// same subject under the same issuer name: keyed by names alone, their
// holders would share one task namespace. An empty subject falls back
// to the certificate's own SHA-256 fingerprint.
func certIdentity(chain []*x509.Certificate) string {
	leaf, anchor := chain[0], chain[len(chain)-1]
	ca := sha256.Sum256(anchor.Raw)
	subject := leaf.Subject.String()
	if subject == "" {
		sum := sha256.Sum256(leaf.Raw)
		subject = "sha256:" + hex.EncodeToString(sum[:])
	}
	return "ca-sha256:" + hex.EncodeToString(ca[:]) + "/" + subject
}

// advertisedAddr is the host:port the cards list for the TCP listener:
// the configured host with the bound port, so port 0 advertises the
// port the kernel picked. A wildcard listen address (empty, 0.0.0.0 or
// ::) names no reachable host, so the certificate's first non-wildcard
// DNS SAN stands in, else its first specific IP SAN. With neither, ok is
// false: there is no address a client could dial and pass the Host
// check with, so the cards list no HTTPS interface.
func advertisedAddr(listen, bound string, leaf *x509.Certificate) (addr string, ok bool) {
	_, port, err := net.SplitHostPort(bound)
	if err != nil {
		return "", false
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || isWildcardHost(host) {
		host = certHost(leaf)
	}
	if host == "" {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

// isLoopbackHost reports a listen host only this machine can reach:
// localhost or a loopback IP. A wildcard is not one.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// logStartupWarnings flags a listener that cannot do what it is
// probably meant for (#358): one with no dialable address to advertise,
// and one reachable from other machines that only takes this process's
// bearer token — which never leaves the process, so no remote client can
// authenticate without client_ca.
func (t *tcpHost) logStartupWarnings() {
	if t.advertised == "" {
		t.logger.Warn("A2A TCP listener has no address to advertise; cards list no HTTPS interface",
			"listen", t.listen, "hint", "listen on a specific host, or give tls_cert a DNS or IP SAN")
	}
	host, _, err := net.SplitHostPort(t.listen)
	if !t.mutualTLS && (err != nil || !isLoopbackHost(host)) {
		t.logger.Warn("A2A TCP listener accepts only this process's bearer token, which never leaves the process; remote clients cannot authenticate without options.a2a.client_ca",
			"listen", t.listen)
	}
}

// isWildcardHost reports a listen host that binds every interface.
func isWildcardHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// certHost returns the first host name a client can dial the
// certificate as: a non-wildcard DNS SAN, else a specific IP SAN.
func certHost(leaf *x509.Certificate) string {
	for _, name := range leaf.DNSNames {
		if name != "" && !strings.HasPrefix(name, "*") {
			return name
		}
	}
	for _, ip := range leaf.IPAddresses {
		if !ip.IsUnspecified() {
			return ip.String()
		}
	}
	return ""
}

// splitAuthority splits a Host header into its host name and port. A
// Host without a port implies the HTTPS default, and IPv6 brackets are
// removed.
func splitAuthority(host string) (name, port string) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return h, p
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), defaultHTTPSPort
}

// sameAuthority reports whether a Host header names addr: the same port,
// and the same host name — compared as IP addresses when both are, and
// case-insensitively otherwise. A wildcard address names no host, so
// nothing matches it.
func sameAuthority(host, addr string) bool {
	addrHost, addrPort, err := net.SplitHostPort(addr)
	if err != nil || isWildcardHost(addrHost) {
		return false
	}
	name, port := splitAuthority(host)
	if port != addrPort {
		return false
	}
	if a, b := net.ParseIP(name), net.ParseIP(addrHost); a != nil && b != nil {
		return a.Equal(b)
	}
	return strings.EqualFold(name, addrHost)
}
