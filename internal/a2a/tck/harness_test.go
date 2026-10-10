package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/a2a"
)

// startHarness wires the full TCK shape in-process: the production
// ServerFactory and host with the scripted runner, fronted by the
// loopback proxy. Everything it returns must be cleaned up.
func startHarness(t *testing.T) (baseURL string, factory *a2a.ServerFactory) {
	t.Helper()

	factory = a2a.NewServerFactory(t.TempDir())
	server, err := factory.StartServer(context.Background(), a2a.ServerParams{
		DispatchID:  "tck-test",
		SessionID:   sessionID,
		ContextID:   sessionID,
		Runner:      &ScriptedRunner{Text: "TCK scripted answer"},
		Todos:       &ScriptedTodos{},
		Diff:        ScriptedDiff,
		Name:        "Crush TCK Agent",
		Description: "harness under test",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = factory.Close(context.Background())
	})
	require.NoError(t, publishDefinition(context.Background(), factory))

	proxy, err := NewProxy(context.Background(), factory, server, sessionID, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Close() })

	return proxy.BaseURL(), factory
}

// TestHarnessServesCardAndSendMessage is the harness's own coverage in
// make test (#363): the well-known card resolves through the proxy with
// its endpoint rewritten, and a JSON-RPC SendMessage round trip
// completes with the scripted answer — which also proves the proxy's
// injected bearer token authenticates, because the host rejects calls
// without it.
func TestHarnessServesCardAndSendMessage(t *testing.T) {
	baseURL, _ := startHarness(t)
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/.well-known/agent-card.json", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var card struct {
		Name                string `json:"name"`
		SupportedInterfaces []struct {
			URL string `json:"url"`
		} `json:"supportedInterfaces"`
		SecurityRequirements []map[string]json.RawMessage `json:"securityRequirements"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&card))
	require.Equal(t, "Crush TCK Agent", card.Name)
	require.NotEmpty(t, card.SupportedInterfaces)
	require.True(t, len(card.SecurityRequirements) > 0, "the card must declare its security requirement")
	// The spec's Security Requirement wants each scheme's scope list as
	// a StringList object, and the TCK's schema validation rejects the
	// bare array the SDK marshals (CARD-STRUCT-001): the served card
	// must carry the normalized {"list": [...]} shape.
	var schemes map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(card.SecurityRequirements[0]["schemes"], &schemes))
	for name, scopes := range schemes {
		var list []string
		require.NoError(t, json.Unmarshal(scopes["list"], &list),
			"scheme %q scopes must be a StringList object (a list field), got %s", name, scopes["list"])
	}
	for _, iface := range card.SupportedInterfaces {
		require.True(t, bytes.HasPrefix([]byte(iface.URL), []byte(baseURL)),
			"interface URL %q must route through the proxy %s", iface.URL, baseURL)
	}

	// The A2A context is the task session (#350): the harness sends the
	// session it started the server with, the same way the dispatch
	// client does.
	msg := a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("run the scripted scenario"))
	msg.ContextID = sessionID
	params, err := json.Marshal(&a2aspec.SendMessageRequest{
		Message: msg,
	})
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  json.RawMessage(params),
	})
	require.NoError(t, err)

	endpoint := card.SupportedInterfaces[0].URL
	req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	rpcResp, err := client.Do(req)
	require.NoError(t, err)
	defer rpcResp.Body.Close()
	require.Equal(t, http.StatusOK, rpcResp.StatusCode)

	var out struct {
		Result struct {
			Task *struct {
				Artifacts []struct {
					Parts []map[string]any `json:"parts"`
				} `json:"artifacts"`
				Status struct {
					State   string `json:"state"`
					Message *struct {
						Role  string           `json:"role"`
						Parts []map[string]any `json:"parts"`
					} `json:"message"`
				} `json:"status"`
				History []struct {
					Role  string           `json:"role"`
					Parts []map[string]any `json:"parts"`
				} `json:"history"`
			} `json:"task"`
		} `json:"result"`
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(rpcResp.Body).Decode(&out))
	require.Nil(t, out.Error, "JSON-RPC error on the scripted round trip: %v", out.Error)
	require.NotNil(t, out.Result.Task)
	require.Equal(t, "TASK_STATE_COMPLETED", out.Result.Task.Status.State)

	var diff, text string
	for _, artifact := range out.Result.Task.Artifacts {
		for _, part := range artifact.Parts {
			if s, ok := part["text"].(string); ok {
				diff += s
			}
		}
	}
	if out.Result.Task.Status.Message != nil {
		for _, part := range out.Result.Task.Status.Message.Parts {
			if s, ok := part["text"].(string); ok {
				text += s
			}
		}
	}
	require.Contains(t, diff, "TCK harness scripted change",
		fmt.Sprintf("the completion artifact must carry the scripted diff, got %q", diff))
	require.Contains(t, text, "TCK scripted answer",
		fmt.Sprintf("the agent's final message must carry the scripted answer, got %q", text))
}

// sendRawMessage posts a JSON-RPC SendMessage carrying message, shaped
// as the TCK sends it, to the proxy's dispatch endpoint and returns the
// task's state and context.
func sendRawMessage(t *testing.T, baseURL string, message map[string]any) (state, contextID string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "SendMessage",
		"params":  map[string]any{"message": message},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/agents/tck-test", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Result struct {
			Task *struct {
				ContextID string `json:"contextId"`
				Status    struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"task"`
		} `json:"result"`
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Nil(t, out.Error, "JSON-RPC error on SendMessage: %v", out.Error)
	require.NotNil(t, out.Result.Task)
	return out.Result.Task.Status.State, out.Result.Task.ContextID
}

// The TCK starts tasks with no contextId (#363), and a served dispatch
// runs only messages on its own bound context (#350): the proxy stamps
// the dispatch's context on such a message, so the scripted run
// completes instead of every TCK task being rejected. A message that
// brings a context of its own is forwarded untouched and meets the
// host's own rejection.
func TestHarnessStampsDispatchContextOnNewTasks(t *testing.T) {
	baseURL, _ := startHarness(t)

	state, contextID := sendRawMessage(t, baseURL, map[string]any{
		"role":      "ROLE_USER",
		"parts":     []map[string]any{{"text": "run the scripted scenario"}},
		"messageId": "tck-no-context",
	})
	require.Equal(t, "TASK_STATE_COMPLETED", state)
	require.Equal(t, sessionID, contextID)

	state, contextID = sendRawMessage(t, baseURL, map[string]any{
		"role":      "ROLE_USER",
		"parts":     []map[string]any{{"text": "run the scripted scenario"}},
		"messageId": "tck-foreign-context",
		"contextId": "tck-foreign-context",
	})
	require.Equal(t, "TASK_STATE_REJECTED", state)
	require.Equal(t, "tck-foreign-context", contextID)
}

// withDispatchContext stamps the context only on a message that starts
// a task without one; every other body passes through byte for byte.
func TestWithDispatchContext(t *testing.T) {
	t.Parallel()

	stampedContext := func(t *testing.T, body []byte) any {
		t.Helper()
		var out struct {
			Params struct {
				Message map[string]any `json:"message"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		return out.Params.Message["contextId"]
	}

	for name, body := range map[string]string{
		"no context":    `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","parts":[{"text":"hi"}]}}}`,
		"empty context": `{"jsonrpc":"2.0","id":1,"method":"SendStreamingMessage","params":{"message":{"messageId":"m","contextId":"","parts":[{"text":"hi"}]}}}`,
		"null context":  `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","contextId":null,"parts":[{"text":"hi"}]}}}`,
	} {
		t.Run("stamps "+name, func(t *testing.T) {
			t.Parallel()
			got := withDispatchContext([]byte(body), sessionID)
			require.Equal(t, sessionID, stampedContext(t, got))

			var before, after map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &before))
			require.NoError(t, json.Unmarshal(got, &after))
			before["params"].(map[string]any)["message"].(map[string]any)["contextId"] = sessionID
			require.Equal(t, before, after, "only the contextId may change")
		})
	}

	for name, body := range map[string]string{
		"own context":     `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","contextId":"theirs","parts":[{"text":"hi"}]}}}`,
		"names a task":    `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","taskId":"t-1","parts":[{"text":"hi"}]}}}`,
		"no message":      `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"t-1"}}`,
		"null message":    `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":null}}`,
		"no params":       `{"jsonrpc":"2.0","id":1,"method":"SendMessage"}`,
		"batch":           `[{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m"}}}]`,
		"not json":        `not json`,
		"non-string task": `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m","taskId":7}}}`,
	} {
		t.Run("leaves "+name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, body, string(withDispatchContext([]byte(body), sessionID)))
		})
	}
}

// TestHarnessProxyForwardsWithoutOriginHeader pins the browser-hardening
// pass-through: the proxy adds no Origin of its own, so the host's
// cross-origin rejection never trips for TCK callers.
func TestHarnessProxyForwardsWithoutOriginHeader(t *testing.T) {
	baseURL, _ := startHarness(t)
	client := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/agents/tck-test",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":2,"method":"","params":{}}`)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// An unauthenticated-style garbage method is a JSON-RPC error from
	// the handler, never the host's 403/415/400 pre-checks: reaching
	// the handler at all means Origin, Content-Type and Host passed.
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// A route's own card path passes through the proxy as spelled (#580), so
// a hand probe of the harness reads what the host serves there — the
// dispatch's card, and the definition's — rather than the proxy's
// rewritten root card.
func TestHarnessPassesRouteCardsThrough(t *testing.T) {
	baseURL, factory := startHarness(t)
	client := &http.Client{Timeout: 10 * time.Second}

	for id, wantName := range map[string]string{"tck-test": "Crush TCK Agent", definitionID: "Coder"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+a2a.AgentPath(id)+wellKnownCardPath, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		var card struct {
			Name                string `json:"name"`
			SupportedInterfaces []struct {
				URL string `json:"url"`
			} `json:"supportedInterfaces"`
		}
		err = json.NewDecoder(resp.Body).Decode(&card)
		_ = resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, id)
		require.Equal(t, wantName, card.Name, id)
		require.Len(t, card.SupportedInterfaces, 1, id)
		require.Equal(t, "http://crush-a2a"+a2a.AgentPath(id), card.SupportedInterfaces[0].URL,
			"%s: the host's own card, as served on the socket", id)
	}
	require.Len(t, factory.AgentCards(), 1, "the harness publishes one definition")
}
