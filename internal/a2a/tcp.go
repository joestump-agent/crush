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
	// host:port the cards list.
	listen     string
	bound      string
	advertised string

	// leaf is the server certificate; its DNS and IP SANs are Host
	// names the listener answers to.
	leaf *x509.Certificate

	// mutualTLS reports that client certificates are required and
	// verified against client_ca.
	mutualTLS bool
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
	}
	t.advertised = advertisedAddr(t.listen, t.bound, leaf)
	t.server = &http.Server{
		Handler:           http.HandlerFunc(t.serveHTTP),
		ReadHeaderTimeout: 30 * time.Second,
		// Failed handshakes — a scanner, a client without a certificate,
		// plain HTTP — go to the debug log, not to stderr under the TUI.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
	go func() {
		defer close(t.done)
		if err := t.server.Serve(t.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("A2A TCP listener died early", "error", err)
		}
	}()
	slog.Info("A2A host listening on TCP", "addr", t.bound, "url", t.baseURL(), "mutual_tls", t.mutualTLS)
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

// baseURL is the listener's https:// origin as the cards advertise it.
func (t *tcpHost) baseURL() string {
	return "https://" + t.advertised
}

// close shuts the TCP listener down within ctx: it stops accepting,
// drains in-flight requests, closes the listener and reaps the Serve
// goroutine.
func (t *tcpHost) close(ctx context.Context) error {
	err := t.server.Shutdown(ctx)
	if cerr := t.listener.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
		err = cerr
	}
	<-t.done
	return err
}

// serveHTTP is the TCP listener's root handler: the factory's middleware
// and route table, with this listener's Host check and caller (#358).
func (t *tcpHost) serveHTTP(w http.ResponseWriter, r *http.Request) {
	t.factory.serveRequest(w, r, t)
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
	if t.mutualTLS && r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
		peer.certVerified = true
		peer.certIdentity = certIdentity(r.TLS.VerifiedChains[0][0])
	}
	return context.WithValue(routeCtx, remotePeerContextKey{}, peer)
}

// certIdentity names a verified client certificate: its subject, or its
// SHA-256 fingerprint when the subject is empty.
func certIdentity(cert *x509.Certificate) string {
	if subject := cert.Subject.String(); subject != "" {
		return subject
	}
	sum := sha256.Sum256(cert.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// advertisedAddr is the host:port the cards list for the TCP listener:
// the configured host with the bound port, so port 0 advertises the
// port the kernel picked. A wildcard listen address (empty, 0.0.0.0 or
// ::) names no reachable host, so the certificate's first DNS SAN — or
// its first specific IP SAN — stands in, and the bound address is the
// last resort.
func advertisedAddr(listen, bound string, leaf *x509.Certificate) string {
	boundHost, port, err := net.SplitHostPort(bound)
	if err != nil {
		return bound
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || isWildcardHost(host) {
		host = certHost(leaf)
	}
	if host == "" {
		host = boundHost
	}
	return net.JoinHostPort(host, port)
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
