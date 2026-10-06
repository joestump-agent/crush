package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/charmbracelet/crush/internal/a2a"
)

// wellKnownCardPath is the A2A well-known agent card path — where the
// TCK discovers the SUT's card.
const wellKnownCardPath = "/.well-known/agent-card.json"

// Proxy fronts Crush's unix-socket A2A host with a loopback TCP
// listener the TCK can reach (#363): the card is served directly, with
// its endpoint rewritten to this listener, and every other request is
// reverse-proxied to the host with the Authorization header and
// Content-Type the served card demands injected.
type Proxy struct {
	listener net.Listener
	server   *http.Server
	// cardJSON is the rewritten card the proxy serves at the well-known
	// path: the host's card with every interface URL pointed here, and
	// the security requirements in the spec's on-the-wire shape.
	cardJSON []byte
	// cardETag and cardLastModified are the caching headers the card
	// response carries (TCK CARD-CACHE-001/002/003): a content hash,
	// and the moment the proxy built the card.
	cardETag         string
	cardLastModified time.Time
}

// NewProxy starts the proxy on 127.0.0.1:port (port 0 picks a free
// one) in front of the factory's host, serving server's dispatch. Use
// [Proxy.BaseURL] as the TCK's --sut-host.
func NewProxy(ctx context.Context, factory *a2a.ServerFactory, server *a2a.Server, port int) (*Proxy, error) {
	endpoint, err := url.Parse(server.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("tck: parse endpoint %q: %w", server.Endpoint, err)
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("tck: bind proxy listener: %w", err)
	}

	card := rewriteCard(server.Card, "http://"+listener.Addr().String())
	cardJSON, err := normalizeCardJSON(card)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("tck: normalize card: %w", err)
	}
	proxy := &Proxy{
		listener:         listener,
		cardJSON:         cardJSON,
		cardETag:         fmt.Sprintf("%x", sha256.Sum256(cardJSON)),
		cardLastModified: time.Now(),
	}

	// The upstream dialer always lands on the host's unix socket,
	// regardless of the address the reverse proxy would otherwise
	// resolve.
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", factory.SocketPath())
		},
	}
	rp := &httputil.ReverseProxy{
		// SSE streams (message/stream) must not sit in a buffer.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: endpoint.Host})
			// The host routes exactly /agents/<dispatch id>
			// (#346); A2A clients post JSON-RPC at the card's base
			// URL, so every proxied call is pinned to the dispatch's
			// route path.
			pr.Out.URL.Path = endpoint.Path
			pr.Out.URL.RawPath = ""
			// The host rejects requests whose Host is not its
			// internal routing label (#346), and requires JSON
			// content even on the header-less well-known GET.
			pr.Out.Host = endpoint.Host
			if pr.Out.Header.Get("Content-Type") == "" {
				pr.Out.Header.Set("Content-Type", "application/json")
			}
			// The card declares the bearer scheme as required
			// (#357); the proxy holds the host's per-process token
			// and stamps it on every call, so the TCK — which knows
			// nothing of the credential — is authenticated.
			pr.Out.Header.Set("Authorization", "Bearer "+factory.AuthToken())
		},
		Transport: transport,
	}

	mux := http.NewServeMux()
	mux.HandleFunc(wellKnownCardPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The card is static for the proxy's lifetime, so it answers
		// with the caching headers a static card endpoint carries
		// (TCK CARD-CACHE-001/002/003), and a revalidation hit with a
		// matching If-None-Match is a 304.
		w.Header().Set("Cache-Control", "max-age=300")
		etag := `"` + proxy.cardETag + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", proxy.cardLastModified.UTC().Format(http.TimeFormat))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if _, err := w.Write(proxy.cardJSON); err != nil {
			slog.Warn("TCK: write card", "err", err)
		}
	})
	mux.Handle("/", rp)

	proxy.server = &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		_ = proxy.server.Serve(listener)
	}()
	return proxy, nil
}

// BaseURL is the loopback URL the TCK points --sut-host at.
func (p *Proxy) BaseURL() string {
	return "http://" + p.listener.Addr().String()
}

// Close stops the listener and drains the server.
func (p *Proxy) Close() error {
	return p.server.Close()
}

// rewriteCard copies the served card with every interface URL pointed
// at the proxy's own base URL — the card the TCK discovers must route
// back through the proxy, or its JSON-RPC calls would target a host
// label only the unix socket answers to.
func rewriteCard(card *a2aspec.AgentCard, baseURL string) *a2aspec.AgentCard {
	out := *card
	out.SupportedInterfaces = make([]*a2aspec.AgentInterface, 0, len(card.SupportedInterfaces))
	for _, iface := range card.SupportedInterfaces {
		if iface == nil {
			continue
		}
		rewritten := *iface
		rewritten.URL = baseURL + ifacePath(iface.URL)
		out.SupportedInterfaces = append(out.SupportedInterfaces, &rewritten)
	}
	return &out
}

// ifacePath extracts the path of an interface URL, so the rewrite
// swaps the authority and keeps the route the host serves.
func ifacePath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return raw
	}
	return u.Path
}

// normalizeCardJSON renders card as the JSON the proxy serves: the SDK
// marshals each security requirement's per-scheme scope list as a bare
// JSON array, while the spec's Security Requirement object wants the
// values as StringList objects ({"list": [...]}) — the TCK's schema
// validation rejects the array (CARD-STRUCT-001). The proxy normalizes
// on the way out and leaves the SDK's card alone; upstream fix tracked
// in internal/a2a/doc.go.
func normalizeCardJSON(card *a2aspec.AgentCard) ([]byte, error) {
	var raw map[string]any
	encoded, err := json.Marshal(card)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return nil, err
	}
	requirements, ok := raw["securityRequirements"].([]any)
	if !ok {
		return json.MarshalIndent(raw, "", "  ")
	}
	for i, req := range requirements {
		obj, ok := req.(map[string]any)
		if !ok {
			continue
		}
		schemes, ok := obj["schemes"].(map[string]any)
		if !ok {
			continue
		}
		for name, scopes := range schemes {
			if list, ok := scopes.([]any); ok {
				schemes[name] = map[string]any{"list": list}
			}
		}
		requirements[i] = obj
	}
	return json.MarshalIndent(raw, "", "  ")
}
