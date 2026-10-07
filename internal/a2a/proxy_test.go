package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
)

// proxyFront stands in for the server API in front of a host (#421): the
// same two routes the server registers, handed to ServeProxied. The
// client it returns reaches the host only through them.
func proxyFront(t *testing.T, factory *ServerFactory) ProxyClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/a2a/agents", func(w http.ResponseWriter, r *http.Request) {
		factory.ServeProxied(w, r, AgentsIndexPath)
	})
	mux.HandleFunc("POST /ws/a2a/agents/{agent}", func(w http.ResponseWriter, r *http.Request) {
		factory.ServeProxied(w, r, AgentPath(r.PathValue("agent")))
	})
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	return ProxyClient{HTTP: front.Client(), BaseURL: front.URL + "/ws/a2a"}
}

// A client that has only the server reaches the host's index and its
// dispatches through the proxy (#421): the snapshot, a stream whose
// events arrive as they happen, a steer, and a cancel — with no token on
// its side.
func TestProxyServesTheIndexAndTheDispatch(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	runner := newPacedRunner("done")
	runner.enqueueAccepted = true
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1", Runner: runner, SessionID: "dispatch-session", ContextID: "dispatch-session",
		Name: "tester", Listed: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	proxy := proxyFront(t, factory)

	listed, err := proxy.IndexConn().ListAgents(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "dispatch-1", listed[0].ID)

	events, err := proxy.IndexConn().WatchAgents(t.Context())
	require.NoError(t, err)
	first := <-events
	require.Len(t, first.Snapshot, 1, "the proxied stream opens with the snapshot")

	gotTask := make(chan string, 1)
	done := make(chan agent.DispatchTransportOutcome, 1)
	go func() {
		outcome, _ := factory.StreamDispatch(context.Background(), agent.DispatchTransportParams{
			Endpoint: server.Endpoint, Card: server.Card, Prompt: "fix the bug", ContextID: "dispatch-session",
			OnTask: func(id string) { gotTask <- id },
		})
		done <- outcome
	}()
	<-runner.started
	taskID := <-gotTask
	working := nextUpsert(t, events, func(d AgentDescriptor) bool { return d.State == DispatchStatusWorking })
	require.Equal(t, taskID, working.TaskID, "a change streams through the proxy as it happens")

	steer, err := proxy.SteerDispatch(t.Context(), agent.DispatchSteerParams{
		Endpoint: working.Endpoint, Card: working.Card, ContextID: working.ContextID,
		Text: "also the docs", ReferenceTaskIDs: []string{working.TaskID},
	})
	require.NoError(t, err)
	require.Equal(t, SteerStatusWorking, steer.Status, "the steer reached the running agent")
	runner.consume(true)

	require.NoError(t, proxy.CancelDispatch(t.Context(), agent.DispatchCancelParams{
		Endpoint: working.Endpoint, Card: working.Card, TaskID: working.TaskID, Reason: "canceled by user",
	}))
	outcome := <-done
	require.Equal(t, DispatchStatusCanceled, outcome.Status, "the cancel ended the dispatch")
	close(runner.release)
}

// The proxy authenticates with the host's own token whatever the caller
// sent, and the host's other checks still judge the request as sent: an
// Origin is refused (#421).
func TestProxyAuthenticatesForTheCaller(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1", Runner: &fakeRunner{result: textResult("x")}, SessionID: "s", ContextID: "s", Listed: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	proxy := proxyFront(t, factory)

	get := func(header, value string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxy.BaseURL+AgentsIndexPath, nil)
		require.NoError(t, err)
		req.Header.Set(header, value)
		resp, err := proxy.HTTP.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, get(bearerAuthorizationHeader, "Bearer not-the-token"), "the caller's credential is replaced")
	require.Equal(t, http.StatusForbidden, get("Origin", "https://evil.example"), "the host still refuses an Origin")
}

// Until the host is up there is nothing to proxy to: 503 (#421).
func TestProxyRefusesBeforeTheHostStarts(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	proxy := proxyFront(t, factory)
	_, err := proxy.IndexConn().ListAgents(t.Context())
	require.ErrorContains(t, err, "503")
}

// A proxied card keeps only a route on the host's own label, so a card
// naming anywhere else cannot point the client off the proxy (#421).
func TestProxiedCardStaysOnTheHost(t *testing.T) {
	proxy := ProxyClient{BaseURL: "http://api.crush.localhost/v1/workspaces/w1/a2a"}
	card := func(url string) *a2aspec.AgentCard {
		return &a2aspec.AgentCard{SupportedInterfaces: []*a2aspec.AgentInterface{{
			URL: url, ProtocolBinding: a2aspec.TransportProtocolJSONRPC, ProtocolVersion: a2aspec.Version,
		}}}
	}

	proxied, err := proxy.proxiedCard(card("http://crush-a2a/agents/dispatch-1"), "e")
	require.NoError(t, err)
	require.Len(t, proxied.SupportedInterfaces, 1)
	require.Equal(t, proxy.BaseURL+"/agents/dispatch-1", proxied.SupportedInterfaces[0].URL)

	for _, url := range []string{
		"https://evil.example/agents/dispatch-1",
		"http://crush-a2a/other/dispatch-1",
		"http://crush-a2a/agents/",
		"http://crush-a2a/agents/a/b",
	} {
		_, err := proxy.proxiedCard(card(url), "e")
		require.Error(t, err, url)
		require.True(t, strings.Contains(err.Error(), "no route"), url)
	}
	_, err = proxy.proxiedCard(nil, "e")
	require.ErrorContains(t, err, "no resolvable agent card")
}
