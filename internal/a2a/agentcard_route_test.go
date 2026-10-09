package a2a

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
)

// coderDefinition is the agent definition the card-route tests publish.
var coderDefinition = agent.AgentDefinitionCard{
	ID:          "coder",
	Name:        "Coder",
	Description: "An agent that helps with executing coding tasks.",
}

// cardURL is a route's well-known card path on base (#580).
func cardURL(base, id string) string {
	return base + agentsPathPrefix + id + a2asrv.WellKnownAgentCardPath
}

// fetchCard issues a context-bound GET for url with the given headers and
// returns the answer's status, headers and body, the body drained and
// closed. host, when set, replaces the request's Host header.
func fetchCard(t *testing.T, client *http.Client, url, host string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(body)
}

// decodeCard parses a served card body.
func decodeCard(t *testing.T, body string) *a2aspec.AgentCard {
	t.Helper()
	var card a2aspec.AgentCard
	require.NoError(t, json.Unmarshal([]byte(body), &card))
	return &card
}

// withoutInterfaces renders a card as JSON with its interfaces stripped,
// so two copies of one card that differ only in what they tell the caller
// to dial compare equal.
func withoutInterfaces(t *testing.T, card *a2aspec.AgentCard) string {
	t.Helper()
	cp := *card
	cp.SupportedInterfaces = nil
	data, err := json.Marshal(&cp)
	require.NoError(t, err)
	return string(data)
}

// A definition's card is served on the socket at the route's well-known
// path (#580): a bare GET — no token, no Content-Type, the way every A2A
// client fetches a card — reads the very card the listing holds, and the
// token changes nothing. The card is metadata; the 0600 socket is the
// reach restriction, as it is for the definition route's JSON-RPC.
func TestHostServesDefinitionCardOverSocket(t *testing.T) {
	t.Parallel()

	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), coderDefinition))
	client := unixDialClient(factory)
	url := cardURL("http://"+a2aURLHost, coderDefinition.ID)

	status, header, body := fetchCard(t, client, url, "", nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "application/json", header.Get("Content-Type"))
	cards := factory.AgentCards()
	require.Len(t, cards, 1)
	want, err := json.Marshal(cards[0])
	require.NoError(t, err)
	require.JSONEq(t, string(want), body, "the definition's own card, as the listing holds it")

	card := decodeCard(t, body)
	require.Equal(t, coderDefinition.Name, card.Name)
	require.Equal(t, coderDefinition.Description, card.Description)
	require.Len(t, card.SupportedInterfaces, 1, "no TCP listener: the socket interface alone")
	require.Equal(t, "http://"+a2aURLHost+agentsPathPrefix+coderDefinition.ID, card.SupportedInterfaces[0].URL)
	require.Equal(t, a2aspec.TransportProtocolJSONRPC, card.SupportedInterfaces[0].ProtocolBinding)
	require.Contains(t, card.SecuritySchemes, bearerSchemeName)
	require.Empty(t, card.Skills)

	status, _, authed := fetchCard(t, client, url, "", map[string]string{
		bearerAuthorizationHeader: "Bearer " + factory.authToken(),
		"Content-Type":            "application/json",
	})
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, body, authed, "the token and a content type are accepted and change nothing")
}

// A dispatch's card is served the same way (#580) — the registry entry's
// own object — and goes away with the route when the dispatch stops.
func TestHostServesDispatchCardOverSocket(t *testing.T) {
	t.Parallel()

	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID:  "dispatch-1",
		Runner:      &fakeRunner{result: textResult("done")},
		SessionID:   "dispatch-session",
		ContextID:   "dispatch-session",
		Name:        "tester",
		Description: "writes tests",
	})
	require.NoError(t, err)
	client := unixDialClient(factory)
	url := cardURL("http://"+a2aURLHost, "dispatch-1")

	status, _, body := fetchCard(t, client, url, "", nil)
	require.Equal(t, http.StatusOK, status)
	want, err := json.Marshal(server.Card)
	require.NoError(t, err)
	require.JSONEq(t, string(want), body, "the dispatch's own card, the registry entry's object")
	require.Equal(t, "tester", decodeCard(t, body).Name)

	require.NoError(t, server.Stop(t.Context()))
	status, _, _ = fetchCard(t, client, url, "", nil)
	require.Equal(t, http.StatusNotFound, status, "a stopped dispatch's card is gone with its route")
}

// The card path answers GET alone (#580): every other method is 405 with
// Allow, token or not.
func TestHostCardRouteAnswersOnlyGET(t *testing.T) {
	t.Parallel()

	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), coderDefinition))
	client := unixDialClient(factory)
	url := cardURL("http://"+a2aURLHost, coderDefinition.ID)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead, http.MethodOptions} {
		var body io.Reader
		if method == http.MethodPost || method == http.MethodPut {
			body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{}}`)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, url, body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(bearerAuthorizationHeader, "Bearer "+factory.authToken())
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, method)
		require.Equal(t, http.MethodGet, resp.Header.Get("Allow"), method)
	}
}

// Only /agents/<id>/.well-known/agent-card.json is a card (#580): an
// unknown id, an empty id, any other sub-path of a route, and the host's
// own well-known path stay 404, and the content-type gate still holds on
// everything that is not a card.
func TestHostCardRouteRefusesOtherShapes(t *testing.T) {
	t.Parallel()

	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), coderDefinition))
	client := unixDialClient(factory)
	base := "http://" + a2aURLHost

	// With a JSON content type, so the gate is not what answers.
	for _, path := range []string{
		"/agents/.well-known/agent-card.json",
		"/agents/no-such-route/.well-known/agent-card.json",
		"/agents/coder/x/.well-known/agent-card.json",
		"/agents/coder/.well-known/agent-card.json/",
		"/agents/coder/.well-known/",
		"/agents/coder/.well-known/other.json",
		"/agents/coder/card",
		"/agents/coder/",
		"/.well-known/agent-card.json",
	} {
		status, _, _ := fetchCard(t, client, base+path, "", map[string]string{"Content-Type": "application/json"})
		require.Equal(t, http.StatusNotFound, status, path)
	}
	// An unknown id in the card shape is 404 without a content type too.
	status, _, _ := fetchCard(t, client, base+"/agents/no-such-route/.well-known/agent-card.json", "", nil)
	require.Equal(t, http.StatusNotFound, status)

	// The gate holds everywhere but the card: a content-less GET on the
	// route itself, and on the host's own well-known path, is 415.
	for _, path := range []string{"/agents/coder", "/.well-known/agent-card.json"} {
		status, _, _ := fetchCard(t, client, base+path, "", nil)
		require.Equal(t, http.StatusUnsupportedMediaType, status, path)
	}
}

// The card GET takes the rest of the middleware (#580): an Origin is
// 403, a wrong Host 400, and an unsupported A2A-Version the spec's
// -32009 envelope — the same answers the JSON-RPC route gives.
func TestHostCardRouteKeepsMiddleware(t *testing.T) {
	t.Parallel()

	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), coderDefinition))
	client := unixDialClient(factory)
	url := cardURL("http://"+a2aURLHost, coderDefinition.ID)

	status, _, _ := fetchCard(t, client, url, "", map[string]string{"Origin": "https://evil.example"})
	require.Equal(t, http.StatusForbidden, status)

	status, _, _ = fetchCard(t, client, cardURL("http://rebind.evil.example", coderDefinition.ID), "", nil)
	require.Equal(t, http.StatusBadRequest, status)

	_, _, body := fetchCard(t, client, url, "", map[string]string{a2aspec.SvcParamVersion: "99.0"})
	var rpcResp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rpcResp))
	require.Equal(t, -32009, rpcResp.Error.Code)
}

// Over TCP the gate still comes first (#358, #580): a bare card GET is
// 401 before the route is looked up. With the bearer token the card is
// served, and a TCP caller's copy names one interface — the route at the
// origin the caller dialed — never the socket label it cannot reach.
func TestTCPListenerServesCardWithBearer(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	factory, server := startTCPDispatch(t, pki.listenerOptions(), &fakeRunner{result: textResult("done")})
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), coderDefinition))
	client := pki.httpsClient(t, nil)
	addr := factory.tcpAddr()
	base := "https://" + addr
	authed := map[string]string{bearerAuthorizationHeader: "Bearer " + factory.authToken()}

	for _, id := range []string{coderDefinition.ID, "no-such-route"} {
		status, header, _ := fetchCard(t, client, cardURL(base, id), "", nil)
		require.Equal(t, http.StatusUnauthorized, status, "the gate answers an uncredentialed GET for %s", id)
		require.NotEmpty(t, header.Get("WWW-Authenticate"))
	}

	status, _, body := fetchCard(t, client, cardURL(base, coderDefinition.ID), "", authed)
	require.Equal(t, http.StatusOK, status)
	card := decodeCard(t, body)
	require.Equal(t, coderDefinition.Name, card.Name)
	require.Len(t, card.SupportedInterfaces, 1, "the socket interface is not offered to a TCP caller")
	require.Equal(t, base+agentsPathPrefix+coderDefinition.ID, card.SupportedInterfaces[0].URL)
	require.Equal(t, a2aspec.TransportProtocolJSONRPC, card.SupportedInterfaces[0].ProtocolBinding)
	require.Equal(t, a2aspec.Version, card.SupportedInterfaces[0].ProtocolVersion)
	require.JSONEq(t, withoutInterfaces(t, factory.AgentCards()[0]), withoutInterfaces(t, card),
		"everything but the interfaces is the definition's card")

	status, _, body = fetchCard(t, client, cardURL(base, "dispatch-1"), "", authed)
	require.Equal(t, http.StatusOK, status)
	card = decodeCard(t, body)
	require.Equal(t, "tester", card.Name, "a dispatch route serves the dispatch's card")
	require.Len(t, card.SupportedInterfaces, 1)
	require.Equal(t, server.Card.SupportedInterfaces[1].URL, card.SupportedInterfaces[0].URL,
		"dialed by the advertised address, the card names the advertised address")
	require.JSONEq(t, withoutInterfaces(t, server.Card), withoutInterfaces(t, card))

	// Dialed by another name the certificate carries, the card names the
	// origin the caller used, so its reader can dial what it reads.
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	status, _, body = fetchCard(t, client, cardURL(base, coderDefinition.ID), "localhost:"+port, authed)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "https://localhost:"+port+agentsPathPrefix+coderDefinition.ID, decodeCard(t, body).SupportedInterfaces[0].URL)

	// With the token the routes are told apart, and the method is checked.
	status, _, _ = fetchCard(t, client, cardURL(base, "no-such-route"), "", authed)
	require.Equal(t, http.StatusNotFound, status)
	status, header, _ := postOverTCP(t, client, cardURL(base, coderDefinition.ID), factory.authToken(), []byte(`{}`))
	require.Equal(t, http.StatusMethodNotAllowed, status)
	require.Equal(t, http.MethodGet, header.Get("Allow"))
}

// With client_ca, a verified client certificate alone reads a card over
// TCP (#580) — no token, no content type, the bare GET an A2A client
// sends — and a client without one never reaches a route.
func TestTCPListenerServesCardWithMutualTLS(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	opts := pki.listenerOptions()
	opts.ClientCA = pki.caFile
	factory, _ := startTCPDispatch(t, opts, &fakeRunner{result: textResult("done")})
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), coderDefinition))
	base := "https://" + factory.tcpAddr()
	url := cardURL(base, coderDefinition.ID)

	requireRefusedHandshake(t, pki.httpsClient(t, nil), url, "a client without a certificate must be rejected")

	cert := clientCert(t, pki.ca, pki.caKey, "teammate")
	status, _, body := fetchCard(t, pki.httpsClient(t, &cert), url, "", nil)
	require.Equal(t, http.StatusOK, status)
	card := decodeCard(t, body)
	require.Equal(t, coderDefinition.Name, card.Name)
	require.Contains(t, card.SecuritySchemes, mtlsSchemeName, "the card declares the scheme that let its reader in")
	require.Len(t, card.SupportedInterfaces, 1)
	require.Equal(t, base+agentsPathPrefix+coderDefinition.ID, card.SupportedInterfaces[0].URL)
}

// The discovery step the team workflow needs (#580, #334): a Crush
// pointed at another Crush's definition card over a mutual-TLS listener
// resolves it with its own external-agent resolver — the card's one
// interface is on the card URL's origin, so the origin pin holds — and
// the resolved endpoint is the definition's route on that listener. On a
// bearer-only listener the resolver's uncredentialed fetch meets the
// gate, which is the documented limit of a token that never leaves its
// process.
func TestExternalResolverReadsTCPDefinitionCard(t *testing.T) {
	t.Parallel()

	pki := newTestPKI(t)
	opts := pki.listenerOptions()
	opts.ClientCA = pki.caFile
	host, _ := startTCPDispatch(t, opts, &fakeRunner{result: textResult("done")})
	require.NoError(t, host.PublishAgentDefinition(t.Context(), coderDefinition))
	base := "https://" + host.tcpAddr()

	cert := clientCert(t, pki.ca, pki.caKey, "teammate")
	caller := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = caller.Close(context.Background()) })
	caller.externalTransport = pki.httpsClient(t, &cert).Transport

	external, err := caller.ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: cardURL(base, coderDefinition.ID)})
	require.NoError(t, err)
	t.Cleanup(external.Close)
	require.Equal(t, cardURL(base, coderDefinition.ID), external.Source())
	require.Equal(t, base+agentsPathPrefix+coderDefinition.ID, external.Endpoint())

	bearerOnly, _ := startTCPDispatch(t, pki.listenerOptions(), &fakeRunner{result: textResult("done")})
	require.NoError(t, bearerOnly.PublishAgentDefinition(t.Context(), coderDefinition))
	caller.externalTransport = pki.httpsClient(t, nil).Transport
	_, err = caller.ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: cardURL("https://"+bearerOnly.tcpAddr(), coderDefinition.ID)})
	require.ErrorContains(t, err, "HTTP status 401", "a bearer-only listener cannot serve a remote card fetch")
}

// splitRoutePath is the router's reading of a path (#580).
func TestSplitRoutePath(t *testing.T) {
	t.Parallel()

	for path, want := range map[string]struct {
		id   string
		card bool
	}{
		"/agents/coder": {"coder", false},
		"/agents/coder/.well-known/agent-card.json":      {"coder", true},
		"/agents/dispatch-1/.well-known/agent-card.json": {"dispatch-1", true},
		"/agents/":                            {"", false},
		"/agents":                             {"", false},
		"/agents/.well-known/agent-card.json": {"", false},
		"/agents/coder/":                      {"", false},
		"/agents/coder/x":                     {"", false},
		"/agents/coder/x/.well-known/agent-card.json": {"", false},
		"/agents/coder/.well-known/agent-card.json/":  {"", false},
		"/.well-known/agent-card.json":                {"", false},
		"/":                                           {"", false},
	} {
		id, card := splitRoutePath(path)
		require.Equal(t, want.id, id, path)
		require.Equal(t, want.card, card, path)
	}
}
