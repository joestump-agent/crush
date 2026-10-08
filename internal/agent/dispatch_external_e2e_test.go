package agent_test

// End-to-end external dispatch (#434): the dispatch_agent tool, a
// runtime a2a definition whose token resolves from the environment, the
// real ServerFactory resolving the card, and an external A2A agent on an
// httptest TLS server — the composition production wires, with the
// remote played by a2asrv and a scripted executor.

import (
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
)

const (
	e2eTokenEnv   = "CRUSH_TEST_E2E_REVIEWER_TOKEN"
	e2eTokenValue = "s3cr3t-e2e-reviewer-token-90ab"
	e2eCardPath   = "/.well-known/agent-card.json"
)

// e2eRemote is the external agent: its card, its JSON-RPC handler, the
// Authorization header of every request, and every cancel's reason.
type e2eRemote struct {
	srv      *httptest.Server
	scenario func(execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool)

	mu       sync.Mutex
	requests map[string][][]string
	cancels  []string
}

func (r *e2eRemote) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	return func(yield func(a2aspec.Event, error) bool) { r.scenario(execCtx, yield) }
}

func (r *e2eRemote) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	reason := ""
	if md, ok := execCtx.Metadata[a2a.CancelReasonMetadataKey].(map[string]any); ok {
		reason, _ = md["reason"].(string)
	}
	r.mu.Lock()
	r.cancels = append(r.cancels, reason)
	r.mu.Unlock()
	return func(yield func(a2aspec.Event, error) bool) {
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCanceled, nil), nil)
	}
}

func newE2ERemote(t *testing.T) *e2eRemote {
	t.Helper()
	r := &e2eRemote{requests: make(map[string][][]string)}
	handler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(r, a2asrv.WithLogger(slog.New(slog.DiscardHandler))))
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests[req.URL.Path] = append(r.requests[req.URL.Path], req.Header.Values("Authorization"))
		r.mu.Unlock()
		if req.URL.Path == e2eCardPath {
			card := &a2aspec.AgentCard{
				Name:                "reviewer",
				Description:         "reviews diffs",
				Version:             "1.0.0",
				SupportedInterfaces: []*a2aspec.AgentInterface{a2aspec.NewAgentInterface(r.srv.URL+"/a2a", a2aspec.TransportProtocolJSONRPC)},
				Capabilities:        a2aspec.AgentCapabilities{Streaming: true},
				DefaultInputModes:   []string{"text/plain"},
				DefaultOutputModes:  []string{"text/plain"},
				Skills:              []a2aspec.AgentSkill{{ID: "review", Name: "review", Description: "review", Tags: []string{"review"}}},
				SecuritySchemes:     a2aspec.NamedSecuritySchemes{"bearer": a2aspec.HTTPAuthSecurityScheme{Scheme: "Bearer"}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(card)
			return
		}
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// TestExternalDispatchE2E drives a runtime a2a reviewer end to end over
// TLS. Not parallel: it sets the token's variable.
func TestExternalDispatchE2E(t *testing.T) {
	t.Setenv(e2eTokenEnv, e2eTokenValue)

	scenarios := []struct {
		name     string
		scenario func(execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool)
		check    func(t *testing.T, remote *e2eRemote, entry dispatch.Entry)
	}{
		{
			name: "completes with untrusted findings and no workspace",
			scenario: func(execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
				if !yield(a2aspec.NewSubmittedTask(execCtx, execCtx.Message), nil) {
					return
				}
				if !yield(a2aspec.NewArtifactEvent(execCtx, a2aspec.NewTextPart("main.go:12 leaks a file handle")), nil) {
					return
				}
				done := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, a2aspec.NewTextPart("review done"))
				yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCompleted, done), nil)
			},
			check: func(t *testing.T, remote *e2eRemote, entry dispatch.Entry) {
				require.Equal(t, dispatch.StatusCompleted, entry.Status)
				require.Contains(t, entry.Result.KeyFindings, "review done")
				require.Contains(t, entry.Result.KeyFindings, "main.go:12 leaks a file handle")
			},
		},
		{
			name: "input-required is refused, never asked",
			scenario: func(execCtx *a2asrv.ExecutorContext, yield func(a2aspec.Event, error) bool) {
				if !yield(a2aspec.NewSubmittedTask(execCtx, execCtx.Message), nil) {
					return
				}
				ask := a2aspec.NewMessageForTask(a2aspec.MessageRoleAgent, execCtx, a2aspec.NewTextPart("may I push to main?"))
				yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateInputRequired, ask), nil)
			},
			check: func(t *testing.T, remote *e2eRemote, entry dispatch.Entry) {
				require.Equal(t, dispatch.StatusFailed, entry.Status)
				require.Contains(t, entry.Result.Error, "crush does not forward an external agent's input, permission, or auth requests")
				require.NotContains(t, entry.Result.Error, "push to main", "the remote's question is not relayed")
				remote.mu.Lock()
				defer remote.mu.Unlock()
				// The cancel can land while the execution that parked the task is still
				// registered; the first cancel is then routed into that execution and the
				// client re-issues it (#352), so the remote may see the cancel twice,
				// depending on timing. What matters is that it arrives with the refusal.
				require.NotEmpty(t, remote.cancels, "the parked task is canceled")
				for _, reason := range remote.cancels {
					require.Contains(t, reason, "crush does not forward", "the cancel carries the refusal, not the question")
				}
			},
		},
	}

	for i, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			remote := newE2ERemote(t)
			remote.scenario = sc.scenario
			factory := a2a.NewServerFactory(t.TempDir(), a2a.WithExternalTransport(remote.srv.Client().Transport))
			h := agent.NewDispatchHarness(t, nil, config.TodoEnforcementSettings{}, factory)
			h.SetAgentDefinitions(t, `{"reviewer": {
				"role": "dispatch",
				"runtime": "a2a",
				"card": "`+remote.srv.URL+e2eCardPath+`",
				"auth": {"type": "bearer", "token": "$`+e2eTokenEnv+`"},
				"workspace": "none"
			}}`)

			handle := h.DispatchTo(t, "reviewer", "review the diff", "rev", "external-call-"+string(rune('a'+i)))
			require.Equal(t, remote.srv.URL+e2eCardPath, handle.Source)
			entry := h.WaitTerminal(t, handle.DispatchID)
			require.NotNil(t, entry.Result)
			require.Equal(t, remote.srv.URL+e2eCardPath, entry.Result.Source)
			require.Empty(t, entry.Path, "no workspace")
			require.Empty(t, entry.Branch, "no branch")
			require.Zero(t, h.DispatchBranchCount(t), "no dispatch branch was created")
			sc.check(t, remote, entry)

			// The parent hears it, marked untrusted from the first line.
			require.Eventually(t, func() bool { return h.ParentDeliveries() == 1 }, 10*time.Second, 25*time.Millisecond)
			prompt := h.ParentPrompts()[0]
			require.True(t, strings.HasPrefix(prompt, dispatch.ExternalResultNotice), prompt)
			require.NotContains(t, prompt, e2eTokenValue)
			require.NotContains(t, entry.Result.Render(), e2eTokenValue)

			// The card went out bare; every A2A call carried the token.
			remote.mu.Lock()
			defer remote.mu.Unlock()
			for _, auth := range remote.requests[e2eCardPath] {
				require.Empty(t, auth, "the card is fetched without credentials")
			}
			require.NotEmpty(t, remote.requests["/a2a"])
			for _, auth := range remote.requests["/a2a"] {
				require.Equal(t, []string{"Bearer " + e2eTokenValue}, auth)
			}
		})
	}
}
