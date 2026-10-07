package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
)

// externalTestToken is the bearer credential the external agent tests
// configure. Every test asserts it never reaches a log line, an error,
// or an outcome.
const externalTestToken = "s3cr3t-external-token-7f3a9c"

// externalCardPath is where the test servers serve their card.
const externalCardPath = "/.well-known/agent-card.json"

// recordedRequest is one request an external test server saw.
type recordedRequest struct {
	path string
	auth []string
}

// externalScenario is what the fake executor does with one task.
type externalScenario func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool)

// externalExecutor is the external agent's executor: it runs the
// scenario, and records the Authorization header each execution saw and
// the reason every cancel carried.
type externalExecutor struct {
	scenario externalScenario

	mu            sync.Mutex
	execAuth      [][]string
	cancelReasons []string
	cancelAuth    [][]string
}

func (e *externalExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	auth, _ := execCtx.ServiceParams.Get(bearerAuthorizationHeader)
	e.mu.Lock()
	e.execAuth = append(e.execAuth, auth)
	e.mu.Unlock()
	return func(yield func(a2aspec.Event, error) bool) {
		e.scenario(ctx, execCtx, yield)
	}
}

func (e *externalExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	auth, _ := execCtx.ServiceParams.Get(bearerAuthorizationHeader)
	e.mu.Lock()
	e.cancelReasons = append(e.cancelReasons, cancelReasonFromMetadata(execCtx.Metadata))
	e.cancelAuth = append(e.cancelAuth, auth)
	e.mu.Unlock()
	return func(yield func(a2aspec.Event, error) bool) {
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCanceled, nil), nil)
	}
}

func (e *externalExecutor) canceled() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.cancelReasons...)
}

// externalServer is an httptest TLS server playing an external A2A
// agent: its card at externalCardPath, its JSON-RPC handler at /a2a, and
// a record of every request's path and Authorization header.
type externalServer struct {
	srv  *httptest.Server
	exec *externalExecutor
	// card builds the served card from the server's own base URL; the
	// tests swap it to tamper with the card.
	card func(base string) *a2aspec.AgentCard
	// cardHandler, when set, replaces the card endpoint outright.
	cardHandler http.HandlerFunc
	// callHandler, when set, replaces the JSON-RPC endpoint outright.
	callHandler http.HandlerFunc

	mu       sync.Mutex
	requests []recordedRequest
}

// newExternalServer starts one external agent. release is closed before
// the server closes, so a scenario parked on it never holds Close up.
func newExternalServer(t *testing.T, scenario externalScenario) (*externalServer, chan struct{}) {
	t.Helper()
	es := &externalServer{exec: &externalExecutor{scenario: scenario}}
	es.card = func(base string) *a2aspec.AgentCard { return externalTestCard(base+"/a2a", true) }
	handler := a2asrv.NewHandler(es.exec, a2asrv.WithLogger(slog.New(slog.DiscardHandler)))
	mux := http.NewServeMux()
	jsonrpc := a2asrv.NewJSONRPCHandler(handler)
	mux.HandleFunc("/a2a", func(w http.ResponseWriter, r *http.Request) {
		if es.callHandler != nil {
			es.callHandler(w, r)
			return
		}
		jsonrpc.ServeHTTP(w, r)
	})
	mux.HandleFunc(externalCardPath, func(w http.ResponseWriter, r *http.Request) {
		if es.cardHandler != nil {
			es.cardHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(es.card(es.srv.URL))
	})
	es.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		es.requests = append(es.requests, recordedRequest{path: r.URL.Path, auth: r.Header.Values(bearerAuthorizationHeader)})
		es.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(es.srv.Close)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return es, release
}

func (es *externalServer) cardURL() string { return es.srv.URL + externalCardPath }

func (es *externalServer) recorded() []recordedRequest {
	es.mu.Lock()
	defer es.mu.Unlock()
	return append([]recordedRequest(nil), es.requests...)
}

// externalTestCard is a well-formed external card serving JSON-RPC at
// serviceURL, declaring an HTTP bearer scheme when bearer is set.
func externalTestCard(serviceURL string, bearer bool) *a2aspec.AgentCard {
	card := &a2aspec.AgentCard{
		Name:                "reviewer",
		Description:         "reviews diffs",
		Version:             "1.0.0",
		SupportedInterfaces: []*a2aspec.AgentInterface{a2aspec.NewAgentInterface(serviceURL, a2aspec.TransportProtocolJSONRPC)},
		Capabilities:        a2aspec.AgentCapabilities{Streaming: true},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2aspec.AgentSkill{{ID: "review", Name: "review", Description: "review", Tags: []string{"review"}}},
	}
	if bearer {
		card.SecuritySchemes = a2aspec.NamedSecuritySchemes{
			"reviewer-bearer": a2aspec.HTTPAuthSecurityScheme{Scheme: "Bearer"},
		}
		card.SecurityRequirements = a2aspec.SecurityRequirementsOptions{{"reviewer-bearer": {}}}
	}
	return card
}

// externalFactory is a server factory whose external calls trust the
// httptest servers' certificate.
func externalFactory(t *testing.T, es *externalServer) *ServerFactory {
	t.Helper()
	return NewServerFactory(t.TempDir(), WithExternalTransport(es.srv.Client().Transport))
}

// resolveTestAgent resolves the server's card with the test token.
func resolveTestAgent(t *testing.T, es *externalServer) agent.ExternalAgent {
	t.Helper()
	ext, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{
		CardURL: es.cardURL(),
		Token:   agent.NewSecret(externalTestToken),
	})
	require.NoError(t, err)
	t.Cleanup(ext.Close)
	return ext
}

// testKill is a RunKill for the transport tests: first reason wins.
type testKill struct {
	mu     sync.Mutex
	reason string
	done   chan struct{}
}

func newTestKill() *testKill { return &testKill{done: make(chan struct{})} }

func (k *testKill) Kill(reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.reason == "" && reason != "" {
		k.reason = reason
		close(k.done)
	}
}

func (k *testKill) Killed() <-chan struct{} { return k.done }

func (k *testKill) Reason() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reason
}

// working opens a task and reports it working.
func working(execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) bool {
	if !yield(a2aspec.NewSubmittedTask(execCtx, execCtx.Message), nil) {
		return false
	}
	return yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateWorking, nil), nil)
}

// parkUntil blocks a scenario until the execution or the test ends.
func parkUntil(ctx context.Context, release <-chan struct{}) {
	select {
	case <-ctx.Done():
	case <-release:
	}
}

// requireNoToken fails when the test token appears in s.
func requireNoToken(t *testing.T, s, what string) {
	t.Helper()
	require.NotContains(t, s, externalTestToken, "the bearer token leaked into %s", what)
}

// The happy path (#434): the card is fetched without credentials, every
// A2A request to the card's origin carries exactly the configured
// bearer token, and the findings come back as untrusted text — the
// status message plus the artifact text, with a token the remote echoed
// back scrubbed out.
func TestExternalAgentHappyPath(t *testing.T) {
	t.Parallel()
	es, _ := newExternalServer(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
		if !working(execCtx, yield) {
			return
		}
		auth, _ := execCtx.ServiceParams.Get(bearerAuthorizationHeader)
		art := a2aspec.NewArtifactEvent(execCtx, a2aspec.NewTextPart("finding: nil deref in main.go"))
		if !yield(art, nil) {
			return
		}
		echo := a2aspec.NewArtifactEvent(execCtx, a2aspec.NewTextPart("you sent "+strings.Join(auth, ",")+"\x1b[2J\u202e"))
		if !yield(echo, nil) {
			return
		}
		done := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, a2aspec.NewTextPart("review done"))
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCompleted, done), nil)
	})
	ext := resolveTestAgent(t, es)
	require.Equal(t, es.cardURL(), ext.Source())
	require.Equal(t, es.srv.URL+"/a2a", ext.Endpoint())

	var taskID string
	outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{
		Prompt: "review this",
		OnTask: func(id string) { taskID = id },
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.NotEmpty(t, taskID, "the remote task's ID is tracked like a served dispatch's")
	require.Contains(t, outcome.Text, "review done")
	require.Contains(t, outcome.Text, "finding: nil deref in main.go")
	require.Contains(t, outcome.Text, "you sent Bearer [REDACTED]", "a token the remote echoes back is scrubbed")
	require.NotContains(t, outcome.Text, "\x1b", "terminal escapes are stripped")
	require.NotContains(t, outcome.Text, "\u202e", "bidi overrides are stripped")
	require.Empty(t, outcome.Diff, "an external agent never produces a diff")
	require.Nil(t, outcome.Usage)
	requireNoToken(t, outcome.Text, "the outcome")

	var cardFetches, calls int
	for _, req := range es.recorded() {
		switch req.path {
		case externalCardPath:
			cardFetches++
			require.Empty(t, req.auth, "the card is fetched without credentials")
		default:
			calls++
			require.Equal(t, []string{"Bearer " + externalTestToken}, req.auth,
				"every A2A request to the card's origin carries exactly the configured token")
		}
	}
	require.Equal(t, 1, cardFetches)
	require.Positive(t, calls)
}

// A bare message reply ends the exchange as completed: plenty of agents
// answer without a task.
func TestExternalAgentMessageReply(t *testing.T) {
	t.Parallel()
	es, _ := newExternalServer(t, func(_ context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
		yield(a2aspec.NewMessage(a2aspec.MessageRoleAgent, a2aspec.NewTextPart("looks good to me")), nil)
	})
	ext := resolveTestAgent(t, es)
	outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{Prompt: "review"})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "looks good to me", outcome.Text)
}

// A card whose JSON-RPC service lives on another origin is refused
// before any request carries the token: the other origin sees nothing at
// all, and the card fetch itself carried no credentials.
func TestExternalAgentRefusesCrossOriginService(t *testing.T) {
	t.Parallel()
	elsewhere, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	es.card = func(string) *a2aspec.AgentCard { return externalTestCard(elsewhere.srv.URL+"/a2a", true) }

	_, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{
		CardURL: es.cardURL(),
		Token:   agent.NewSecret(externalTestToken),
	})
	require.ErrorContains(t, err, "on another origin")
	requireNoToken(t, err.Error(), "the error")

	require.Empty(t, elsewhere.recorded(), "the foreign origin must never be contacted")
	for _, req := range es.recorded() {
		require.Equal(t, externalCardPath, req.path, "nothing but the card is fetched")
		require.Empty(t, req.auth, "no request carried credentials")
	}
}

// A card that declares no HTTP bearer scheme is refused when the
// definition sets auth: the token is never sent anywhere.
func TestExternalAgentRequiresBearerScheme(t *testing.T) {
	t.Parallel()
	es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	es.card = func(base string) *a2aspec.AgentCard { return externalTestCard(base+"/a2a", false) }

	_, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{
		CardURL: es.cardURL(),
		Token:   agent.NewSecret(externalTestToken),
	})
	require.ErrorContains(t, err, "declares no HTTP bearer security scheme")
	requireNoToken(t, err.Error(), "the error")
	for _, req := range es.recorded() {
		require.Equal(t, externalCardPath, req.path)
		require.Empty(t, req.auth)
	}

	// Without auth the same card is fine, and no request carries a
	// credential at all.
	ext, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: es.cardURL()})
	require.NoError(t, err)
	ext.Close()
}

// A card URL that is not https (or loopback http) is refused before any
// request is made.
func TestExternalAgentRefusesNonHTTPSCard(t *testing.T) {
	t.Parallel()
	dialed := false
	f := NewServerFactory(t.TempDir(), WithExternalTransport(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		dialed = true
		return nil, errors.New("must not dial")
	})))
	for _, raw := range []string{
		"http://reviewer.example.net/.well-known/agent-card.json",
		"ftp://reviewer.example.net/card.json",
		"file:///etc/passwd",
		"https://user:" + externalTestToken + "@reviewer.example.net/card.json",
	} {
		_, err := f.ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: raw, Token: agent.NewSecret(externalTestToken)})
		require.Error(t, err, raw)
		require.Contains(t, err.Error(), "agent card URL", raw)
		requireNoToken(t, err.Error(), "the error")
	}
	require.False(t, dialed, "a refused card URL must not reach the network")
}

// An unreachable card fails resolution with a clear error.
func TestExternalAgentUnreachableCard(t *testing.T) {
	t.Parallel()
	es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	f := externalFactory(t, es)
	cardURL := es.cardURL()
	es.srv.Close()

	_, err := f.ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: cardURL, Token: agent.NewSecret(externalTestToken)})
	require.ErrorContains(t, err, "resolve agent card "+cardURL)
	requireNoToken(t, err.Error(), "the error")
}

// A card endpoint that answers with an error status or junk fails
// resolution with the resolver's reason.
func TestExternalAgentResolverErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name:    "not found",
			handler: func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
			want:    "404",
		},
		{
			name: "not a card",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("<html>login</html>"))
			},
			want: "card parsing failed",
		},
		{
			name: "oversized card",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(bytes.Repeat([]byte(" "), maxExternalCardBytes+1))
			},
			want: "request body too large",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
			es.cardHandler = tc.handler
			_, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: es.cardURL()})
			require.ErrorContains(t, err, "resolve agent card")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// A card endpoint that redirects to another origin is not followed: the
// other origin is never contacted. A redirect within the origin is.
func TestExternalAgentRedirects(t *testing.T) {
	t.Parallel()
	elsewhere, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	es.cardHandler = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.cardURL(), http.StatusFound)
	}

	_, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{
		CardURL: es.cardURL(),
		Token:   agent.NewSecret(externalTestToken),
	})
	require.ErrorContains(t, err, "cross-origin redirect")
	require.Empty(t, elsewhere.recorded(), "a cross-origin redirect must not be followed")

	// Same-origin: the card moved within the agent's own host.
	same, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	same.cardHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("moved") == "" {
			http.Redirect(w, r, externalCardPath+"?moved=1", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(externalTestCard(same.srv.URL+"/a2a", true))
	}
	ext, err := externalFactory(t, same).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{
		CardURL: same.cardURL(),
		Token:   agent.NewSecret(externalTestToken),
	})
	require.NoError(t, err)
	ext.Close()
}

// A card that offers no JSON-RPC interface is refused as unsupported.
func TestExternalAgentRequiresJSONRPC(t *testing.T) {
	t.Parallel()
	es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	es.card = func(base string) *a2aspec.AgentCard {
		card := externalTestCard(base+"/a2a", true)
		card.SupportedInterfaces = []*a2aspec.AgentInterface{a2aspec.NewAgentInterface(base+"/grpc", a2aspec.TransportProtocolGRPC)}
		return card
	}
	_, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: es.cardURL()})
	require.ErrorContains(t, err, "offers no supported transport")
	require.ErrorContains(t, err, "GRPC")
}

// A stream that stays silent past the idle timeout is cut: the run's
// kill switch records the idle reason, the remote task is canceled with
// it over a credentialed tasks/cancel, and the outcome is canceled.
func TestExternalAgentIdleTimeoutCancels(t *testing.T) {
	t.Parallel()
	var release chan struct{}
	es, release := newExternalServer(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
		if !working(execCtx, yield) {
			return
		}
		parkUntil(ctx, release)
	})
	ext := resolveTestAgent(t, es)
	kill := newTestKill()

	outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{
		Prompt:      "review",
		IdleTimeout: 100 * time.Millisecond,
		Kill:        kill,
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCanceled, outcome.Status)
	require.Equal(t, dispatch.ReasonIdleTimeout, outcome.Text)
	require.Equal(t, dispatch.ReasonIdleTimeout, kill.Reason(), "the idle timeout trips the run's kill switch")
	require.Equal(t, []string{dispatch.ReasonIdleTimeout}, es.exec.canceled(), "the remote task is canceled with the reason")

	es.exec.mu.Lock()
	defer es.exec.mu.Unlock()
	require.Equal(t, [][]string{{"Bearer " + externalTestToken}}, es.exec.cancelAuth, "the cancel is credentialed too")
}

// A kill from outside — the hard timeout, a user cancel — cuts the
// stream and cancels the remote task with the kill's reason.
func TestExternalAgentKillCancels(t *testing.T) {
	t.Parallel()
	var release chan struct{}
	es, release := newExternalServer(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
		if !working(execCtx, yield) {
			return
		}
		parkUntil(ctx, release)
	})
	ext := resolveTestAgent(t, es)
	kill := newTestKill()

	outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{
		Prompt: "review",
		// The kill lands once the remote task is known, the way the
		// coordinator's watchdog meets a running dispatch.
		OnTask: func(string) { kill.Kill(dispatch.ReasonHardTimeout) },
		Kill:   kill,
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCanceled, outcome.Status)
	require.Equal(t, dispatch.ReasonHardTimeout, outcome.Text)
	require.Equal(t, []string{dispatch.ReasonHardTimeout}, es.exec.canceled())
}

// A kill recorded before the stream even starts ends it at once.
func TestExternalAgentKilledBeforeStart(t *testing.T) {
	t.Parallel()
	var release chan struct{}
	es, release := newExternalServer(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
		if !working(execCtx, yield) {
			return
		}
		parkUntil(ctx, release)
	})
	ext := resolveTestAgent(t, es)
	kill := newTestKill()
	kill.Kill(dispatch.ReasonShutdown)

	outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{Prompt: "review", Kill: kill})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCanceled, outcome.Status)
	require.Equal(t, dispatch.ReasonShutdown, outcome.Text)
}

// A remote that asks for input — or for auth — is never answered: the
// task is canceled and the outcome fails with a clear reason.
func TestExternalAgentRefusesInputRequired(t *testing.T) {
	t.Parallel()
	for _, state := range []a2aspec.TaskState{a2aspec.TaskStateInputRequired, a2aspec.TaskStateAuthRequired} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			es, _ := newExternalServer(t, func(_ context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
				if !working(execCtx, yield) {
					return
				}
				ask := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, a2aspec.NewTextPart("approve rm -rf /?"))
				yield(a2aspec.NewStatusUpdateEvent(execCtx, state, ask), nil)
			})
			ext := resolveTestAgent(t, es)

			outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{Prompt: "review"})
			require.NoError(t, err)
			require.Equal(t, DispatchStatusFailed, outcome.Status)
			require.Contains(t, outcome.Text, "crush does not forward an external agent's input, permission, or auth requests")
			require.NotContains(t, outcome.Text, "rm -rf", "the remote's question is not relayed")
			require.Len(t, es.exec.canceled(), 1, "the paused task is canceled")
			require.Contains(t, es.exec.canceled()[0], "crush does not forward")
		})
	}
}

// A remote failure comes back as a failed outcome whose reason is
// sanitized and capped.
func TestExternalAgentFailureIsCapped(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("x", 3*maxExternalTextBytes)
	es, _ := newExternalServer(t, func(_ context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
		if !working(execCtx, yield) {
			return
		}
		msg := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, a2aspec.NewTextPart(huge))
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed, msg), nil)
	})
	ext := resolveTestAgent(t, es)
	outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{Prompt: "review"})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusFailed, outcome.Status)
	require.LessOrEqual(t, len(outcome.Text), maxExternalTextBytes+len(externalTruncatedMarker))
	require.True(t, strings.HasSuffix(outcome.Text, externalTruncatedMarker))
}

// The pinned transport refuses any origin but its own, and the
// interceptor never stamps the token off it.
func TestExternalPinning(t *testing.T) {
	t.Parallel()
	pinned := originOf(mustURL(t, "https://reviewer.example.net/.well-known/agent-card.json"))
	require.Equal(t, pinned, originOf(mustURL(t, "HTTPS://Reviewer.Example.Net:443/a2a")), "default ports and case normalize")
	require.NotEqual(t, pinned, originOf(mustURL(t, "http://reviewer.example.net/a2a")), "scheme is part of the origin")
	require.NotEqual(t, pinned, originOf(mustURL(t, "https://reviewer.example.net:8443/a2a")), "port is part of the origin")
	require.NotEqual(t, pinned, originOf(mustURL(t, "https://evil.example.net/a2a")))

	called := false
	transport := &pinnedTransport{origin: pinned, base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unreachable")
	})}
	body := &closeTracker{Reader: strings.NewReader("{}")}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://evil.example.net/a2a", body)
	require.NoError(t, err)
	req.Header.Set(bearerAuthorizationHeader, "Bearer "+externalTestToken)
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorContains(t, err, "pinned to https://reviewer.example.net:443")
	requireNoToken(t, err.Error(), "the error")
	require.False(t, called, "an off-origin request must not be sent")
	require.True(t, body.closed, "a refused request's body is closed")

	interceptor := &externalAuthInterceptor{origin: pinned, token: agent.NewSecret(externalTestToken)}
	offOrigin := &a2aclient.Request{BaseURL: "https://evil.example.net/a2a", ServiceParams: a2aclient.ServiceParams{}}
	_, _, err = interceptor.Before(t.Context(), offOrigin)
	require.ErrorContains(t, err, "refusing a call off the external agent's origin")
	require.Empty(t, offOrigin.ServiceParams.Get(bearerAuthorizationHeader), "no token off the pinned origin")

	onOrigin := &a2aclient.Request{BaseURL: "https://reviewer.example.net/a2a", ServiceParams: a2aclient.ServiceParams{"Authorization": {"Bearer stale"}}}
	_, _, err = interceptor.Before(t.Context(), onOrigin)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer " + externalTestToken}, onOrigin.ServiceParams.Get(bearerAuthorizationHeader))
	require.NotContains(t, onOrigin.ServiceParams, "Authorization", "one credential, replaced rather than appended")
}

// The token never reaches a log line (#434): every scenario that logs —
// a resolution refusal, a canceled pause, a stream cut by a kill — runs
// with the default logger captured at debug, and the capture must not
// hold the token. Not parallel: it swaps the process-wide logger.
func TestExternalAgentNeverLogsToken(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var release chan struct{}
	es, release := newExternalServer(t, func(ctx context.Context, execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
		if !working(execCtx, yield) {
			return
		}
		switch execCtx.Message.Parts[0].Text() {
		case "ask":
			ask := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, a2aspec.NewTextPart("token please"))
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateInputRequired, ask), nil)
		case "park":
			parkUntil(ctx, release)
		default:
			auth, _ := execCtx.ServiceParams.Get(bearerAuthorizationHeader)
			fail := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, a2aspec.NewTextPart("bad credential "+strings.Join(auth, "")))
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed, fail), nil)
		}
	})
	ext := resolveTestAgent(t, es)

	for _, prompt := range []string{"ask", "park", "fail"} {
		kill := newTestKill()
		outcome, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{Prompt: prompt, IdleTimeout: 100 * time.Millisecond, Kill: kill})
		require.NoError(t, err, prompt)
		requireNoToken(t, outcome.Text, "the outcome")
		requireNoToken(t, fmt.Sprintf("%+v", outcome), "the formatted outcome")
	}

	es.card = func(base string) *a2aspec.AgentCard { return externalTestCard(base+"/a2a", false) }
	_, err := externalFactory(t, es).ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{
		CardURL: es.cardURL(),
		Token:   agent.NewSecret(externalTestToken),
	})
	require.Error(t, err)
	slog.Warn("Resolution refused", "error", err, "params", agent.ExternalAgentParams{CardURL: es.cardURL(), Token: agent.NewSecret(externalTestToken)})

	logs := buf.String()
	require.NotEmpty(t, logs, "the scenarios must have logged something for the check to mean anything")
	requireNoToken(t, logs, "the logs")
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// closeTracker records whether a request body was closed.
type closeTracker struct {
	*strings.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// sseHandler answers every call with an event stream whose body is
// written by write.
func sseHandler(write func(w io.Writer)) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write(w)
	}
}

// A remote that keeps talking is cut off at the call body cap: the
// stream fails rather than buffering without end.
func TestExternalAgentCapsCallBody(t *testing.T) {
	t.Parallel()
	es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	keepalive := []byte(strings.Repeat(": keepalive\n\n", 4096))
	es.callHandler = sseHandler(func(w io.Writer) {
		for written := 0; written <= maxExternalBodyBytes; written += len(keepalive) {
			if _, err := w.Write(keepalive); err != nil {
				return
			}
		}
	})
	ext := resolveTestAgent(t, es)
	_, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{Prompt: "review"})
	require.ErrorContains(t, err, "request body too large")
}

// One server-sent event with no boundary is cut off at the event cap,
// whatever the body cap leaves.
func TestExternalAgentCapsSSEEvent(t *testing.T) {
	t.Parallel()
	es, _ := newExternalServer(t, func(context.Context, *a2asrv.ExecutorContext, func(a2aspec.Event, error) bool) {})
	es.callHandler = sseHandler(func(w io.Writer) {
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxExternalEventBytes+1))
		_, _ = w.Write([]byte("\n\n"))
	})
	ext := resolveTestAgent(t, es)
	_, err := ext.Stream(t.Context(), agent.ExternalDispatchParams{Prompt: "review"})
	require.ErrorContains(t, err, "server-sent event over")
}

// The event cap counts from the last boundary in every line-ending
// style, so a long stream of small events never trips it.
func TestSSEEventLimitBoundaries(t *testing.T) {
	t.Parallel()
	for name, stream := range map[string]string{
		"lf":   strings.Repeat("data: {}\n\n", 50),
		"crlf": strings.Repeat("data: {}\r\n\r\n", 50),
		"cr":   strings.Repeat("data: {}\r\r", 50),
	} {
		limited := &sseEventLimit{body: io.NopCloser(strings.NewReader(stream)), limit: 16, lineEmpty: true}
		got, err := io.ReadAll(limited)
		require.NoError(t, err, name)
		require.Equal(t, stream, string(got), name)
	}
	long := &sseEventLimit{body: io.NopCloser(strings.NewReader("data: 0123456789\ndata: 0123456789\n\n")), limit: 16, lineEmpty: true}
	_, err := io.ReadAll(long)
	require.ErrorIs(t, err, errSSEEventTooLarge, "data lines of one event add up")
}

// The artifacts an external agent's output is collected from are
// capped: a remote cannot grow the fold with endless artifacts.
func TestExternalFoldCapsArtifacts(t *testing.T) {
	t.Parallel()
	var fold externalFold
	for i := range 3 * maxExternalArtifacts {
		fold.artifact(&a2aspec.Artifact{ID: a2aspec.ArtifactID(fmt.Sprintf("art-%d", i)), Parts: []*a2aspec.Part{a2aspec.NewTextPart("x")}}, false)
	}
	require.Len(t, fold.order, maxExternalArtifacts)
	require.Len(t, fold.texts, maxExternalArtifacts)
}

// A plain-http card is allowed only on loopback, and a name is checked
// where it resolves (#434): a localhost that resolves off the machine is
// refused before anything is sent, and one that resolves to loopback is
// reached.
func TestExternalAgentPlainHTTPMustResolveToLoopback(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(externalTestCard("http://"+r.Host+"/a2a", false))
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	cardURL := "http://localhost:" + port + externalCardPath

	resolvesTo := func(ip string) *ServerFactory {
		f := NewServerFactory(t.TempDir())
		f.externalLookup = func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return f
	}

	_, err = resolvesTo("203.0.113.9").ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: cardURL})
	require.ErrorContains(t, err, "not a loopback address")
	require.Zero(t, requests.Load(), "nothing is sent to a localhost that is not local")

	ext, err := resolvesTo("127.0.0.1").ResolveExternalAgent(t.Context(), agent.ExternalAgentParams{CardURL: cardURL})
	require.NoError(t, err)
	ext.Close()
	require.Equal(t, int32(1), requests.Load())
}
