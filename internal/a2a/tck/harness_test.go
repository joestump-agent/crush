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

	proxy, err := NewProxy(context.Background(), factory, server, 0)
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

	params, err := json.Marshal(&a2aspec.SendMessageRequest{
		Message: a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart("run the scripted scenario")),
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
