package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
		ContextID:   "dispatch-session",
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
		ContextID:   "dispatch-session",
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
	resp, err = postJSONRPCAuthed(t, factory, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
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

	_, err = factory.StartServer(t.Context(), ServerParams{DispatchID: "d", Runner: &fakeRunner{}, SessionID: "s"})
	require.ErrorContains(t, err, "requires a context id")

	_, err = factory.StartServer(t.Context(), ServerParams{Runner: &fakeRunner{}, SessionID: "s", ContextID: "c"})
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
	resp, err := postJSONRPCAuthed(t, factory, client, endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	stop()
	// The stop unregistered the route: the host answers, the dispatch
	// serves nothing.
	resp, err = postJSONRPC(t, client, endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
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
		ContextID:  "dispatch-session",
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
	resp, err := postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "x"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	require.NoError(t, factory.Close(context.Background()))
	resp, err = postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "x"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "the socket must be dead after Close")

	_, statErr := os.Stat(factory.socketPath())
	require.True(t, os.IsNotExist(statErr), "the socket file must be removed on Close")
}

// Stop ends the route's runs before it returns: a run still in flight is
// canceled and joined, so the dispatch's teardown after Stop never closes
// the toolchain or releases the workspace under a running tool call.
func TestServerStopJoinsInFlightRun(t *testing.T) {
	runner := newUnwindingRunner()
	close(runner.unwind)
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		resp, err := postJSONRPCAuthed(t, factory, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-runner.started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, server.Stop(ctx))
	select {
	case <-runner.ctxDone:
	default:
		t.Fatal("Stop returned without ending the in-flight run")
	}
	<-sent
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
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	for _, ct := range []string{"text/plain", ""} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.Endpoint, bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
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
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	for _, ct := range []string{"application/json", "text/plain"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.Endpoint, bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
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
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://rebind.evil.example/agents/dispatch-1", bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
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
		ContextID:  "dispatch-session",
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
// prompts never cross (#346). Each dispatch owns its own A2A context
// (#350) — its task session ID — and each task runs on its own session.
func TestHostRoutesTwoDispatches(t *testing.T) {
	one := &fakeRunner{result: textResult("from one")}
	two := &fakeRunner{result: textResult("from two")}
	factory := NewServerFactory(t.TempDir())
	serverOne, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "one",
		Runner:     one,
		SessionID:  "session-one",
		ContextID:  "session-one",
	})
	require.NoError(t, err)
	serverTwo, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "two",
		Runner:     two,
		SessionID:  "session-two",
		ContextID:  "session-two",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	drive := func(server *Server, runner *fakeRunner, contextID, prompt string) error {
		resp, err := postJSONRPCAuthed(t, factory, client, server.Endpoint, sendMessageBody(t, contextID, prompt))
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
	go func() { errs <- drive(serverOne, one, "session-one", "prompt one") }()
	go func() { errs <- drive(serverTwo, two, "session-two", "prompt two") }()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.Equal(t, "prompt one", one.gotCall.Prompt)
	require.Equal(t, "prompt two", two.gotCall.Prompt)
	// Each task ran on its own session (#350): the contexts never cross.
	require.Equal(t, "session-one", one.gotCall.SessionID)
	require.Equal(t, "session-two", two.gotCall.SessionID)
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
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.True(t, strings.HasPrefix(factory.socketPath(), filepath.Join(os.TempDir(), "crush-a2a-")),
		"the socket must fall back to the per-user temp dir, got %s", factory.socketPath())

	client := unixDialClient(factory)
	resp, err := postJSONRPCAuthed(t, factory, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// Two hosts in one process whose data dirs both overflow the socket
// path limit share the fallback directory but never a socket (#346):
// before the fallback name carried the data dir, the second bind
// removed the first host's live socket and took over its traffic,
// which the bearer check (#357) then rejected with the wrong token.
// Each host keeps its own socket, and each serves its own dispatch
// with its own token.
func TestHostFallbackSocketIsPerDataDir(t *testing.T) {
	type host struct {
		factory *ServerFactory
		server  *Server
		runner  *fakeRunner
	}
	start := func() host {
		deep := filepath.Join(t.TempDir(), strings.Repeat("sub/", 60))
		primary := filepath.Join(deep, a2aSocketDirName, fmt.Sprintf("%d.sock", os.Getpid()))
		require.Greater(t, len(primary), maxUnixSocketPathLen, "the test data dir must overflow the socket path limit")

		runner := &fakeRunner{result: textResult("done")}
		factory := NewServerFactory(deep)
		server, err := factory.StartServer(t.Context(), ServerParams{
			DispatchID: "dispatch-1",
			Runner:     runner,
			SessionID:  "dispatch-session",
			ContextID:  "dispatch-session",
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = factory.Close(context.Background()) })
		return host{factory: factory, server: server, runner: runner}
	}
	first, second := start(), start()

	fallback := filepath.Join(os.TempDir(), "crush-a2a-")
	for _, h := range []host{first, second} {
		require.True(t, strings.HasPrefix(h.factory.socketPath(), fallback),
			"the socket must fall back to the per-user temp dir, got %s", h.factory.socketPath())
		require.LessOrEqual(t, len(h.factory.socketPath()), maxUnixSocketPathLen,
			"the fallback exists to fit the socket path limit, got %s", h.factory.socketPath())
	}
	require.NotEqual(t, first.factory.socketPath(), second.factory.socketPath(),
		"hosts rooted at different data dirs must not share a fallback socket")
	_, err := os.Stat(first.factory.socketPath())
	require.NoError(t, err, "the second host's bind must leave the first host's socket in place")

	for i, h := range []host{first, second} {
		resp, err := postJSONRPCAuthed(t, h.factory, unixDialClient(h.factory), h.server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
		require.NoError(t, err)
		var rpcResp struct {
			Result struct {
				Task *a2aspec.Task `json:"task"`
			} `json:"result"`
			Error any `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
		_ = resp.Body.Close()
		require.Nil(t, rpcResp.Error, "host %d must authenticate its own token on its own socket", i)
		require.NotNil(t, rpcResp.Result.Task)
		require.Equal(t, a2aspec.TaskStateCompleted, rpcResp.Result.Task.Status.State)
		require.True(t, h.runner.ran, "host %d's own runner must serve its socket", i)
	}
}

// The fallback directory names a POSIX uid as is, and a long uid — a
// Windows SID, which alone nearly fills the AF_UNIX path limit under
// the user's temp dir — by a short hash that still tells users apart.
func TestFallbackUserTag(t *testing.T) {
	t.Parallel()

	require.Equal(t, "501", fallbackUserTag("501"))
	require.Equal(t, "unknown", fallbackUserTag("unknown"))

	sid := "S-1-5-21-1643835476-1616584234-1346609752-500"
	tag := fallbackUserTag(sid)
	require.Len(t, tag, maxFallbackUserTagLen)
	require.Equal(t, tag, fallbackUserTag(sid), "the tag must be stable for one user")
	require.NotEqual(t, tag, fallbackUserTag("S-1-5-21-1643835476-1616584234-1346609752-501"),
		"different users must get different directories")
}

// A stale socket file at the host's path is replaced on start: the path
// is pid-scoped, so anything there belongs to a dead process and plain
// removal before the bind is safe (#346).
func TestHostReplacesStaleSocket(t *testing.T) {
	dataDir := t.TempDir()
	// The stale file goes exactly where the host will bind, computed by
	// the same helper (which falls back to the per-user temp dir when the
	// data dir path would overflow the socket length limit, as it can on
	// the macOS runners).
	stale, err := a2aSocketPath(dataDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))

	factory := NewServerFactory(dataDir)
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.Equal(t, stale, factory.socketPath())

	client := unixDialClient(factory)
	resp, err := postJSONRPCAuthed(t, factory, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// A call without the host's bearer token never reaches the runner
// (#357): no header, a malformed header, an empty credential and a
// wrong token all come back as the JSON-RPC UNAUTHENTICATED error.
func TestHostRejectsUnauthenticated(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	for name, authorization := range map[string]string{
		"no token":     "",
		"wrong token":  "Bearer not-the-token",
		"wrong scheme": "Basic " + factory.authToken(),
		"empty bearer": "Bearer ",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.Endpoint, bytes.NewReader(sendMessageBody(t, "dispatch-session", "run the task")))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set(bearerAuthorizationHeader, authorization)
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		var rpcResp struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
		_ = resp.Body.Close()
		require.NotNil(t, rpcResp.Error, "%s must be rejected with a JSON-RPC error", name)
		require.Equal(t, -31401, rpcResp.Error.Code, "%s must map to UNAUTHENTICATED", name)
		require.Contains(t, rpcResp.Error.Message, "unauthenticated", "%s must name the reason", name)
	}
	require.False(t, runner.ran, "the runner must not see an unauthenticated call")
}

// The right token runs the dispatch, and tasks/list — authenticated the
// same way — answers with the dispatch's task (#357): the interceptor
// marks the call's user, and that is what the task store authorizes
// against.
func TestHostAuthenticatesBearerAndServesTasks(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	resp, err := postJSONRPCAuthed(t, factory, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
	require.NoError(t, err)
	var rpcResp struct {
		Result struct {
			Task *a2aspec.Task `json:"task"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
	_ = resp.Body.Close()
	require.Nil(t, rpcResp.Error, "JSON-RPC error on authenticated SendMessage")
	require.NotNil(t, rpcResp.Result.Task)
	require.Equal(t, a2aspec.TaskStateCompleted, rpcResp.Result.Task.Status.State)
	require.True(t, runner.ran)

	listBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "ListTasks",
		"params":  map[string]any{},
	})
	require.NoError(t, err)
	resp, err = postJSONRPCAuthed(t, factory, client, server.Endpoint, listBody)
	require.NoError(t, err)
	var listResp struct {
		Result struct {
			Tasks []*a2aspec.Task `json:"tasks"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listResp))
	_ = resp.Body.Close()
	require.Nil(t, listResp.Error, "JSON-RPC error on authenticated ListTasks")
	require.Len(t, listResp.Result.Tasks, 1)
	require.Equal(t, a2aspec.TaskStateCompleted, listResp.Result.Tasks[0].Status.State)
}

// The host token never reaches the log (#357): a rejected call and an
// accepted one both run under a captured slog handler — the SDK logs
// through slog too — and the token appears nowhere in what was written.
func TestHostTokenNeverLogged(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)

	client := unixDialClient(factory)
	resp, err := postJSONRPC(t, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "x"))
	require.NoError(t, err)
	var rpcResp struct {
		Error any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
	_ = resp.Body.Close()
	require.NotNil(t, rpcResp.Error)

	resp, err = postJSONRPCAuthed(t, factory, client, server.Endpoint, sendMessageBody(t, "dispatch-session", "run the task"))
	require.NoError(t, err)
	var okResp struct {
		Error any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&okResp))
	_ = resp.Body.Close()
	require.Nil(t, okResp.Error)
	require.True(t, runner.ran)

	require.NotContains(t, buf.String(), factory.authToken(),
		"the host token must never be written to the log")
}

// A request carrying an A2A-Version the host does not serve is answered
// with the spec's VersionNotSupportedError envelope (-32009) before any
// dispatch work runs (TCK VER-SERVER-002), the request id echoed back;
// the current version passes through and serves normally.
func TestHostRejectsUnsupportedVersion(t *testing.T) {
	runner := &fakeRunner{result: textResult("done")}
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     runner,
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	client := unixDialClient(factory)
	post := func(version string) string {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.Endpoint, bytes.NewReader(sendMessageBody(t, "dispatch-session", "version probe")))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(bearerAuthorizationHeader, "Bearer "+factory.authToken())
		req.Header.Set(a2aspec.SvcParamVersion, version)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	body := post("99.0")
	require.False(t, runner.ran, "the runner must not see an unsupported version")
	var rpcResp struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int              `json:"code"`
			Message string           `json:"message"`
			Data    []map[string]any `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rpcResp))
	require.Equal(t, -32009, rpcResp.Error.Code)
	require.NotEmpty(t, rpcResp.Error.Message)
	require.Equal(t, json.RawMessage("1"), rpcResp.ID, "the envelope echoes the request id")
	require.NotEmpty(t, rpcResp.Error.Data, "the error carries typed details")
	require.Equal(t, "VERSION_NOT_SUPPORTED", rpcResp.Error.Data[0]["reason"])

	post(string(a2aspec.Version))
	require.True(t, runner.ran, "the current version serves normally")
}

// GetExtendedAgentCard answers UnsupportedOperationError (-32004) when
// the served card does not declare the extendedAgentCard capability
// (TCK CORE-CAP-003): the handler's capability checks see the card.
func TestHostExtendedCardUnsupportedOperation(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	server, err := factory.StartServer(t.Context(), ServerParams{
		DispatchID: "dispatch-1",
		Runner:     &fakeRunner{result: textResult("done")},
		SessionID:  "dispatch-session",
		ContextID:  "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.False(t, server.Card.Capabilities.ExtendedAgentCard)

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "GetExtendedAgentCard",
		"params":  map[string]any{},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.Endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(bearerAuthorizationHeader, "Bearer "+factory.authToken())
	resp, err := unixDialClient(factory).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var rpcResp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpcResp))
	require.Equal(t, -32004, rpcResp.Error.Code, "expected UnsupportedOperationError")
}

// The peer-uid half of the auth decision (#357): a pure table test —
// the rejected-uid case cannot be produced on a live socket from inside
// the host's own process, so the decision takes the peer as injected.
func TestAuthorizePeerUID(t *testing.T) {
	base := authDecision{
		token:         "right",
		wantUID:       "1000",
		peerKnown:     true,
		peerUID:       "1000",
		authorization: []string{"Bearer right"},
	}
	require.NoError(t, base.authorize())
	require.Equal(t, "crush:1000", base.userName())

	otherUID := base
	otherUID.peerUID = "1001"
	require.ErrorIs(t, otherUID.authorize(), a2aspec.ErrUnauthenticated,
		"a peer running as another uid must be rejected even with the right token")

	noPeerCredentials := base
	noPeerCredentials.peerKnown = false
	require.NoError(t, noPeerCredentials.authorize())
	require.Equal(t, "crush", noPeerCredentials.userName(),
		"without peer credentials the identity carries no uid")

	badToken := base
	badToken.authorization = []string{"Bearer wrong"}
	require.ErrorIs(t, badToken.authorize(), a2aspec.ErrUnauthenticated)

	missingHeader := base
	missingHeader.authorization = nil
	require.ErrorIs(t, missingHeader.authorize(), a2aspec.ErrUnauthenticated)

	duplicateHeader := base
	duplicateHeader.authorization = []string{"Bearer right", "Bearer right"}
	require.ErrorIs(t, duplicateHeader.authorize(), a2aspec.ErrUnauthenticated,
		"an ambiguous Authorization header must not authenticate")

	wrongScheme := base
	wrongScheme.authorization = []string{"Basic right"}
	require.ErrorIs(t, wrongScheme.authorize(), a2aspec.ErrUnauthenticated)

	// The token comparison is constant time on the credential, but the
	// length difference is observable and a token shorter than the
	// prefix can never match.
	shortBearer := base
	shortBearer.authorization = []string{"Bearer "}
	require.ErrorIs(t, shortBearer.authorize(), a2aspec.ErrUnauthenticated)
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

// postJSONRPCAuthed issues a context-bound JSON POST carrying the host
// bearer token (#357) — what every request to a live route must
// present, and what the production dispatch client attaches itself.
func postJSONRPCAuthed(t *testing.T, factory *ServerFactory, client *http.Client, url string, body []byte) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(bearerAuthorizationHeader, "Bearer "+factory.authToken())
	return client.Do(req)
}

// sendMessageBody marshals a JSON-RPC message/send envelope for prompt,
// addressed to contextID (#350): the client supplies the A2A context, the
// same way the dispatch client does — the served executor resolves it to
// the dispatch's binding or rejects the task.
func sendMessageBody(t *testing.T, contextID, prompt string) []byte {
	t.Helper()
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart(prompt))
	msg.ContextID = contextID
	params, err := json.Marshal(&a2aspec.SendMessageRequest{Message: msg})
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

// servedUserName is the identity the host authenticates a same-process
// call as (#357): the socket peer's uid where the platform reports peer
// credentials, bare otherwise (Windows).
func servedUserName() string {
	decision := authDecision{
		wantUID:   strconv.Itoa(os.Getuid()),
		peerKnown: runtime.GOOS != "windows",
	}
	decision.peerUID = decision.wantUID
	return decision.userName()
}
