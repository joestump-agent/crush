package a2a

// The host behind the server's proxy (#421): in client/server mode the
// TUI runs in another process, so it reaches a workspace's A2A host
// through the server API instead of the host's socket. The server hands
// each proxied request to ServeProxied, which forwards it over the
// socket and authenticates it with the host's own token, so the token
// never leaves this process. The client side is ProxyClient, which
// steers and cancels dispatches through the same proxy.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/charmbracelet/crush/internal/agent"
)

// AgentPath is a dispatch's route on its host: /agents/<dispatch id>.
func AgentPath(dispatchID string) string {
	return agentsPathPrefix + dispatchID
}

// ServeProxied forwards r to path on this host (#421): the agent index
// ([AgentsIndexPath]) or one dispatch's route (/agents/<dispatch id>).
// The request keeps its method, body and headers — Origin, Content-Type
// and A2A-Version included, so the host's own checks still judge what
// the caller sent — while Host becomes the socket's label and the
// caller's Authorization is replaced with the host's bearer token. A
// host that is not up answers 503.
func (f *ServerFactory) ServeProxied(w http.ResponseWriter, r *http.Request, path string) {
	if _, ok := f.AgentIndexConn(); !ok {
		http.Error(w, "a2a: the workspace's agent host is not running", http.StatusServiceUnavailable)
		return
	}
	proxy := &httputil.ReverseProxy{
		Transport: f.indexHTTPClient().Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: a2aURLHost, Path: path}
			pr.Out.Host = a2aURLHost
			pr.Out.Header.Del(bearerAuthorizationHeader)
			pr.Out.Header.Set(bearerAuthorizationHeader, "Bearer "+f.authToken())
		},
		// Index and dispatch streams are SSE: every event goes out as
		// it arrives.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				slog.Warn("A2A proxy request failed", "path", path, "error", err)
			}
			http.Error(w, "a2a: the workspace's agent host did not answer", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// ProxyClient steers and cancels dispatched agents through the server's
// proxy (#421), for a TUI in client/server mode. HTTP dials the server
// and BaseURL is the workspace's proxy prefix; a dispatch's route is
// BaseURL plus the path its card names, and the server authenticates
// every call, so the client sends no token.
type ProxyClient struct {
	HTTP    *http.Client
	BaseURL string
}

// IndexConn is the proxied connection to the workspace host's index.
func (c ProxyClient) IndexConn() AgentIndexConn {
	return AgentIndexConn{HTTP: c.HTTP, BaseURL: c.BaseURL}
}

// SteerDispatch delivers one steer through the proxy; see
// [ServerFactory.SteerDispatch].
func (c ProxyClient) SteerDispatch(ctx context.Context, p agent.DispatchSteerParams) (agent.DispatchSteerOutcome, error) {
	card, err := c.proxiedCard(p.Card, p.Endpoint)
	if err != nil {
		return agent.DispatchSteerOutcome{}, err
	}
	client, err := c.newClient(ctx, card)
	if err != nil {
		return agent.DispatchSteerOutcome{}, err
	}
	return steerDispatch(ctx, client, p)
}

// CancelDispatch sends one tasks/cancel through the proxy; see
// [ServerFactory.CancelDispatch].
func (c ProxyClient) CancelDispatch(ctx context.Context, p agent.DispatchCancelParams) error {
	card, err := c.proxiedCard(p.Card, p.Endpoint)
	if err != nil {
		return err
	}
	client, err := c.newClient(ctx, card)
	if err != nil {
		return err
	}
	return cancelDispatch(ctx, client, p)
}

// proxiedCard is the dispatch's card pointed at the proxy: one JSON-RPC
// interface, BaseURL plus the path of the card's socket interface. Only
// a route on the host's own label is proxied, so a card naming anything
// else cannot steer the proxy elsewhere.
func (c ProxyClient) proxiedCard(raw any, endpoint string) (*a2aspec.AgentCard, error) {
	card, ok := raw.(*a2aspec.AgentCard)
	if !ok || card == nil {
		return nil, fmt.Errorf("a2a: dispatch %s has no resolvable agent card", endpoint)
	}
	for _, iface := range card.SupportedInterfaces {
		if iface == nil || iface.ProtocolBinding != a2aspec.TransportProtocolJSONRPC {
			continue
		}
		u, err := url.Parse(iface.URL)
		if err != nil || u.Host != a2aURLHost || !strings.HasPrefix(u.Path, agentsPathPrefix) {
			continue
		}
		id := strings.TrimPrefix(u.Path, agentsPathPrefix)
		if id == "" || strings.Contains(id, "/") {
			continue
		}
		proxied := *card
		proxied.SupportedInterfaces = []*a2aspec.AgentInterface{{
			URL:             strings.TrimSuffix(c.BaseURL, "/") + AgentPath(url.PathEscape(id)),
			ProtocolBinding: a2aspec.TransportProtocolJSONRPC,
			ProtocolVersion: iface.ProtocolVersion,
		}}
		return &proxied, nil
	}
	return nil, fmt.Errorf("a2a: dispatch %s has no route on the agent host", endpoint)
}

// newClient builds the A2A client for a proxied card: the JSON-RPC
// transport over the server's HTTP client and the traceparent
// interceptor, with no auth interceptor — the server authenticates.
func (c ProxyClient) newClient(ctx context.Context, card *a2aspec.AgentCard) (*a2aclient.Client, error) {
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(c.HTTP),
		a2aclient.WithCallInterceptors(&traceparentInterceptor{}),
	)
	if err != nil {
		return nil, fmt.Errorf("a2a: client from card: %w", err)
	}
	return client, nil
}
