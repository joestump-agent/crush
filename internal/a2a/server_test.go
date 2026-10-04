package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
)

// The register -> resolve -> teardown loop (#70): StartServer binds a
// loopback endpoint, the card it returns advertises that endpoint, and
// Resolve reads both back from a registry entry stamped with them.
func TestServerRegisterResolveTeardown(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:      runner,
		SessionID:   "dispatch-session",
		Name:        "tester",
		Description: "writes tests",
	})
	require.NoError(t, err)
	require.NotEmpty(t, server.Endpoint)
	require.NotNil(t, server.Card)
	require.Equal(t, "tester", server.Card.Name)

	// Stamp on a registry entry — the in-memory discovery surface — and
	// resolve back without a network hop.
	entry := dispatch.Entry{Endpoint: server.Endpoint, AgentCard: server.Card}
	card, endpoint, ok := Resolve(entry)
	require.True(t, ok)
	require.Equal(t, server.Endpoint, endpoint)
	require.Equal(t, "tester", card.Name)
	require.Equal(t, "writes tests", card.Description)

	// Teardown: the server stops, and Stop is idempotent.
	require.NoError(t, server.Stop(context.Background()))
	require.NoError(t, server.Stop(context.Background()))

	// A cleared entry resolves to nothing — a torn-down dispatch must not
	// hand out a dead endpoint.
	entry.Endpoint, entry.AgentCard = "", nil
	_, _, ok = Resolve(entry)
	require.False(t, ok)

	// An opaque card (something else stamped the entry) is not resolvable.
	entry.Endpoint, entry.AgentCard = server.Endpoint, "not a card"
	_, _, ok = Resolve(entry)
	require.False(t, ok)
}

// The served surface: the well-known agent-card path serves the card, and
// a JSON-RPC SendMessage runs the executor's full task lifecycle —
// submitted, working, completed with the agent's text (#70 wires the
// Executor behind a2asrv for the first time in production).
func TestServerServesCardAndTaskLifecycle(t *testing.T) {
	runner := &fakeRunner{result: textResult("steered answer")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:      runner,
		SessionID:   "dispatch-session",
		Name:        "tester",
		Description: "writes tests",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	// The agent card is served at the well-known path.
	card, err := fetchCard(t, server.Endpoint)
	require.NoError(t, err)
	require.Equal(t, "tester", card.Name)
	require.NotEmpty(t, card.SupportedInterfaces)

	// A JSON-RPC message/send runs the dispatched agent and completes the
	// task with its text output.
	params, err := json.Marshal(&a2aspec.SendMessageRequest{
		Message: a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("run the task")),
	})
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  json.RawMessage(params),
	})
	require.NoError(t, err)

	resp, err := postJSONRPC(t, server.Endpoint+"/", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpcResp struct {
		Result struct {
			Task *a2aspec.Task `json:"task"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
	require.Nil(t, rpcResp.Error, "JSON-RPC error on SendMessage")
	require.NotNil(t, rpcResp.Result.Task, "message/send must return a task")
	require.Equal(t, a2aspec.TaskStateCompleted, rpcResp.Result.Task.Status.State)

	// The executor ran the dispatched agent on the dispatch's session.
	require.True(t, runner.ran)
	require.Equal(t, "dispatch-session", runner.gotCall.SessionID)
	require.Equal(t, "run the task", runner.gotCall.Prompt)
}

// StartServer validates its inputs.
func TestStartServerValidation(t *testing.T) {
	_, err := StartServer(t.Context(), ServerParams{SessionID: "s"})
	require.ErrorContains(t, err, "requires a runner")

	_, err = StartServer(t.Context(), ServerParams{Runner: &fakeRunner{}})
	require.ErrorContains(t, err, "requires a session id")
}

// The factory satisfies the agent-side seam (#70): it starts the server,
// returns the endpoint and the opaque card for the registry, and its stop
// tears the server down.
func TestServerFactoryImplementsStarterSeam(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory()

	endpoint, card, stop, err := factory.StartDispatchServer(context.Background(), agent.DispatchServerParams{
		SessionID:   "dispatch-session",
		Runner:      sessionAgentAdapter{runner: runner},
		Name:        "tester",
		Description: "writes tests",
	})
	require.NoError(t, err)
	require.NotEmpty(t, endpoint)
	typed, ok := card.(*a2aspec.AgentCard)
	require.True(t, ok, "the registry card must be the served AgentCard")
	require.Equal(t, "tester", typed.Name)

	resp, err := httpGet(t, endpoint+a2asrv.WellKnownAgentCardPath)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	stop()
	// The stop shut the server down: further requests fail.
	require.False(t, urlReachable(t, endpoint+a2asrv.WellKnownAgentCardPath))

	// A start failure propagates rather than returning a half-built server:
	// no session ID means nothing to serve.
	_, _, _, err = factory.StartDispatchServer(context.Background(), agent.DispatchServerParams{
		Runner: sessionAgentAdapter{runner: &fakeRunner{}},
	})
	require.Error(t, err)
}

// sessionAgentAdapter widens the executor's fake runner to the fuller
// SessionAgent shape the starter seam takes. The embedded nil interface
// satisfies the surface; only Run is exercised, exactly like the agent
// package's own dispatch fakes.
type sessionAgentAdapter struct {
	agent.SessionAgent
	runner *fakeRunner
}

// Run delegates to the fake runner.
func (a sessionAgentAdapter) Run(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	return a.runner.Run(ctx, call)
}

// Stop waits for the Serve goroutine to exit within a bound, proving
// teardown leaks neither the goroutine nor its port.
func TestServerStopReleasesPort(t *testing.T) {
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{result: textResult("done")},
		SessionID: "dispatch-session",
	})
	require.NoError(t, err)
	endpoint := server.Endpoint

	done := make(chan error, 1)
	go func() { done <- server.Stop(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}

	require.False(t, urlReachable(t, endpoint+a2asrv.WellKnownAgentCardPath), "the port must be released after Stop")
}

// httpGet issues a context-bound GET, satisfying the noctx rule the raw
// http.Get trips.
func httpGet(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// fetchCard GETs and decodes the well-known agent card.
func fetchCard(t *testing.T, endpoint string) (*a2aspec.AgentCard, error) {
	t.Helper()
	resp, err := httpGet(t, endpoint+a2asrv.WellKnownAgentCardPath)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errUnexpectedStatus
	}
	var card a2aspec.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, err
	}
	return &card, nil
}

// urlReachable reports whether a GET on url succeeds at all, closing
// whatever response comes back.
func urlReachable(t *testing.T, url string) bool {
	t.Helper()
	resp, err := httpGet(t, url)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// errUnexpectedStatus reports a non-200 card fetch.
var errUnexpectedStatus = errors.New("unexpected status")

// postJSONRPC issues a context-bound JSON POST.
func postJSONRPC(t *testing.T, url string, body []byte) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}
