package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
)

// The register -> resolve -> teardown loop (#70): StartServer registers
// the dispatch's route on the process host, the card it returns
// advertises the routed endpoint, and Resolve reads both back from a
// registry entry stamped with them.
func TestServerRegisterResolveTeardown(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID:  "dispatch-1",
		Runner:      runner,
		SessionID:   "dispatch-session",
		Name:        "tester",
		Description: "writes tests",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.Equal(t, "http://"+a2aURLHost+"/agents/dispatch-1", server.Endpoint)
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

	// Teardown: the route is unregistered, and Stop is idempotent.
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

// The served surface (#346): the well-known card path is gone
// (discovery is in-memory), and a JSON-RPC SendMessage on the dispatch's
// route runs the executor's full task lifecycle — submitted, working,
// completed with the agent's text.
func TestServerServesTaskLifecycle(t *testing.T) {
	runner := &fakeRunner{result: textResult("steered answer")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID:  "dispatch-1",
		Runner:      runner,
		SessionID:   "dispatch-session",
		Name:        "tester",
		Description: "writes tests",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)

	// The card is no longer served over the wire: the well-known path
	// never reaches the route table (middleware rejects a content-less
	// GET with 415 first; a JSON POST to it would 404).
	resp, err := httpGet(t, client, "http://"+a2aURLHost+a2asrv.WellKnownAgentCardPath)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)

	// A JSON-RPC message/send runs the dispatched agent and completes the
	// task with its text output.
	resp, err = postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "run the task"))
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
	factory := NewServerFactory(t.TempDir())

	_, err := factory.StartServer(t.Context(), ServerParams{DispatchID: "d", SessionID: "s"})
	require.ErrorContains(t, err, "requires a runner")

	_, err = factory.StartServer(t.Context(), ServerParams{DispatchID: "d", Runner: &fakeRunner{}})
	require.ErrorContains(t, err, "requires a session id")

	_, err = factory.StartServer(t.Context(), ServerParams{Runner: &fakeRunner{}, SessionID: "s"})
	require.ErrorContains(t, err, "requires a dispatch id")
}

// The factory satisfies the agent-side seam (#70): it registers the
// dispatch's route, returns the endpoint and the opaque card for the
// registry, and its stop unregisters the route while the host keeps
// serving.
func TestServerFactoryImplementsStarterSeam(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	endpoint, card, stop, err := factory.StartDispatchServer(context.Background(), agent.DispatchServerParams{
		DispatchID:  "dispatch-1",
		SessionID:   "dispatch-session",
		Runner:      sessionAgentAdapter{runner: runner},
		Name:        "tester",
		Description: "writes tests",
	})
	require.NoError(t, err)
	require.Equal(t, "http://"+a2aURLHost+"/agents/dispatch-1", endpoint)
	typed, ok := card.(*a2aspec.AgentCard)
	require.True(t, ok, "the registry card must be the served AgentCard")
	require.Equal(t, "tester", typed.Name)

	client := unixDialClient(factory)
	resp, err := postJSONRPC(t, client, endpoint, sendMessageBody(t, "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	stop()
	// The stop unregistered the route: the host answers, the dispatch
	// serves nothing.
	resp, err = postJSONRPC(t, client, endpoint, sendMessageBody(t, "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// A start failure propagates rather than returning a half-built server:
	// no session ID means nothing to serve.
	_, _, _, err = factory.StartDispatchServer(context.Background(), agent.DispatchServerParams{
		DispatchID: "dispatch-2",
		Runner:     sessionAgentAdapter{runner: &fakeRunner{}},
	})
	require.Error(t, err)
}

// Stop unregisters the route quickly and Close tears the whole host
// down: the socket file is removed and further dials fail, so teardown
// leaks neither the goroutine nor the socket (#346).
func TestServerStopReleasesSocket(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- server.Stop(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}

	client := unixDialClient(factory)
	resp, err := postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "x"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	require.NoError(t, factory.Close(context.Background()))
	resp, err = postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "x"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "the socket must be dead after Close")

	_, statErr := os.Stat(factory.socketPath())
	require.True(t, os.IsNotExist(statErr), "the socket file must be removed on Close")
}

// A request that is not a JSON POST never reaches the dispatch (#346):
// a text/plain body (the browser CSRF shape) and a missing content type
// are both rejected with 415, and the runner is not invoked.
func TestHostRejectsNonJSON(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	for _, ct := range []string{"text/plain", ""} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.Endpoint, bytes.NewReader(sendMessageBody(t, "run the task")))
		require.NoError(t, err)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode, "content type %q must be rejected", ct)
	}
	require.False(t, runner.ran, "the runner must not see a non-JSON request")
}

// A request carrying an Origin header is a browser attempt, never a
// dispatch client: 403 before anything else runs, and the runner is not
// invoked (#346).
func TestHostRejectsOrigin(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	for _, ct := range []string{"application/json", "text/plain"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.Endpoint, bytes.NewReader(sendMessageBody(t, "run the task")))
		require.NoError(t, err)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Origin", "https://evil.example")
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "origin with content type %q must be rejected", ct)
	}
	require.False(t, runner.ran, "the runner must not see a cross-origin request")
}

// A wrong Host is a DNS-rebinding attempt: 400, no dispatch work (#346).
func TestHostRejectsWrongHost(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://rebind.evil.example/agents/dispatch-1", bytes.NewReader(sendMessageBody(t, "run the task")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.False(t, runner.ran)
}

// The socket is bound 0600 in a 0700 directory, so only the same user
// reaches the unauthenticated surface (#346).
func TestHostSocketPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket permissions are a POSIX assertion")
	}
	factory := NewServerFactory(t.TempDir())
	_, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	info, err := os.Stat(factory.socketPath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the socket must be 0600")

	dir, err := os.Stat(filepath.Dir(factory.socketPath()))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dir.Mode().Perm(), "the socket directory must be 0700")
}

// Two concurrent dispatches each reach their own executor through the
// shared host: the route table maps /agents/<id> separately, and the
// prompts never cross (#346).
func TestHostRoutesTwoDispatches(t *testing.T) {
	one := &fakeRunner{result: textResult("from one")}
	two := &fakeRunner{result: textResult("from two")}
	factory := NewServerFactory(t.TempDir())
	serverOne, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "one",
		Runner:     one,
		SessionID:  "session-one",
	})
	require.NoError(t, err)
	serverTwo, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "two",
		Runner:     two,
		SessionID:  "session-two",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	drive := func(server *Server, runner *fakeRunner, prompt string) error {
		resp, err := postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, prompt))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d from %s", resp.StatusCode, server.Endpoint)
		}
		var rpcResp struct {
			Result struct {
				Task *a2aspec.Task `json:"task"`
			} `json:"result"`
			Error any `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
			return err
		}
		if rpcResp.Error != nil {
			return fmt.Errorf("rpc error from %s", server.Endpoint)
		}
		if rpcResp.Result.Task == nil || rpcResp.Result.Task.Status.State != a2aspec.TaskStateCompleted {
			return fmt.Errorf("dispatch %s did not complete", server.Endpoint)
		}
		if runner.gotCall.Prompt != prompt {
			return fmt.Errorf("dispatch %s got prompt %q, want %q", server.Endpoint, runner.gotCall.Prompt, prompt)
		}
		return nil
	}

	errs := make(chan error, 2)
	go func() { errs <- drive(serverOne, one, "prompt one") }()
	go func() { errs <- drive(serverTwo, two, "prompt two") }()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.Equal(t, "prompt one", one.gotCall.Prompt)
	require.Equal(t, "prompt two", two.gotCall.Prompt)
}

// A deep data directory would overflow the 104-byte sun_path limit: the
// host falls back to a per-user directory under [os.TempDir] and serves
// from there (#346).
func TestHostLongDataDirFallsBack(t *testing.T) {
	deep := filepath.Join(t.TempDir(), strings.Repeat("sub/", 60))
	primary := filepath.Join(deep, a2aSocketDirName, fmt.Sprintf("%d.sock", os.Getpid()))
	require.Greater(t, len(primary), maxUnixSocketPathLen, "the test data dir must overflow the socket path limit")

	factory := NewServerFactory(deep)
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.True(t, strings.HasPrefix(factory.socketPath(), filepath.Join(os.TempDir(), "crush-a2a-")),
		"the socket must fall back to the per-user temp dir, got %s", factory.socketPath())

	client := unixDialClient(factory)
	resp, err := postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// A stale socket file at the host's path is replaced on start: the path
// is pid-scoped, so anything there belongs to a dead process and plain
// removal before the bind is safe (#346).
func TestHostReplacesStaleSocket(t *testing.T) {
	dataDir := t.TempDir()
	stale := filepath.Join(dataDir, a2aSocketDirName, fmt.Sprintf("%d.sock", os.Getpid()))
	require.NoError(t, os.MkdirAll(filepath.Dir(stale), 0o700))
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))

	factory := NewServerFactory(dataDir)
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.Equal(t, stale, factory.socketPath())

	client := unixDialClient(factory)
	resp, err := postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
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

// unixDialClient returns an HTTP client whose transport dials the
// factory's unix socket, ignoring the resolved address: the same wire
// the dispatch client uses, with the card's Host label on every request.
func unixDialClient(factory *ServerFactory) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", factory.socketPath())
			},
		},
	}
}

// httpGet issues a context-bound GET, satisfying the noctx rule the raw
// http.Get trips.
func httpGet(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// postJSONRPC issues a context-bound JSON POST.
func postJSONRPC(t *testing.T, client *http.Client, url string, body []byte) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

// sendMessageBody marshals a JSON-RPC message/send envelope for prompt.
func sendMessageBody(t *testing.T, prompt string) []byte {
	t.Helper()
	params, err := json.Marshal(&a2aspec.SendMessageRequest{
		Message: a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart(prompt)),
	})
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  json.RawMessage(params),
	})
	require.NoError(t, err)
	return body
}
