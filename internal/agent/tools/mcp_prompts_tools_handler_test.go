package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// The MCP prompt tools call into the in-process mcp registry, which needs a
// live server session to return prompts. These tests instead pin the handler
// logic that sits in front of that call — argument validation, the session
// requirement, and permission gating — none of which needs a live server.
// (The end-to-end render happy path is covered in mcp/prompts_test.go.)

func newPromptPerms(allow bool) *recordingPermissionService {
	return &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  allow,
	}
}

func promptSessionCtx() context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, "test-session")
}

func runPromptTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, name string, params any) (fantasy.ToolResponse, error) {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "test-call", Name: name, Input: string(input)})
}

func TestListMCPPromptsTool_RequiresMCPName(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(true)
	tool := NewListMCPPromptsTool(cfg, perms)

	// Whitespace-only name must be rejected before any session/permission work.
	resp, err := runPromptTool(t, tool, promptSessionCtx(), ListMCPPromptsToolName, ListMCPPromptsParams{MCPName: "  "})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "mcp_name parameter is required")
	require.Zero(t, perms.requestCount, "validation must short-circuit before requesting permission")
}

func TestListMCPPromptsTool_RequiresSession(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(true)
	tool := NewListMCPPromptsTool(cfg, perms)

	// No session ID in context: the handler cannot request permission and
	// must surface a hard error rather than silently proceeding.
	_, err := runPromptTool(t, tool, context.Background(), ListMCPPromptsToolName, ListMCPPromptsParams{MCPName: "srv"})
	require.Error(t, err)
	require.Zero(t, perms.requestCount)
}

func TestListMCPPromptsTool_PermissionDenied(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(false)
	tool := NewListMCPPromptsTool(cfg, perms)

	resp, err := runPromptTool(t, tool, promptSessionCtx(), ListMCPPromptsToolName, ListMCPPromptsParams{MCPName: "srv"})
	require.NoError(t, err)
	require.Equal(t, 1, perms.requestCount, "a well-formed request must reach the permission gate")
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "denied permission")
}

func TestListMCPPromptsTool_GrantedReachesMCP(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(true)
	tool := NewListMCPPromptsTool(cfg, perms)

	// Permission granted: the handler proceeds into the mcp registry. With no
	// live session for this server the registry returns a "not available"
	// error, which the tool must surface as an error response (not a Go error)
	// — proving the granted branch runs end to end.
	resp, err := runPromptTool(t, tool, promptSessionCtx(), ListMCPPromptsToolName, ListMCPPromptsParams{MCPName: "ghost-list"})
	require.NoError(t, err)
	require.Equal(t, 1, perms.requestCount)
	require.True(t, resp.IsError)
}

func TestCallMCPPromptTool_RequiresMCPName(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(true)
	tool := NewCallMCPPromptTool(cfg, perms)

	resp, err := runPromptTool(t, tool, promptSessionCtx(), CallMCPPromptToolName, CallMCPPromptParams{PromptName: "greet"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "mcp_name parameter is required")
	require.Zero(t, perms.requestCount)
}

func TestCallMCPPromptTool_RequiresPromptName(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(true)
	tool := NewCallMCPPromptTool(cfg, perms)

	// mcp_name present but prompt_name blank must be rejected before the gate.
	resp, err := runPromptTool(t, tool, promptSessionCtx(), CallMCPPromptToolName, CallMCPPromptParams{MCPName: "srv", PromptName: "  "})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "prompt_name parameter is required")
	require.Zero(t, perms.requestCount)
}

func TestCallMCPPromptTool_PermissionDenied(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(false)
	tool := NewCallMCPPromptTool(cfg, perms)

	resp, err := runPromptTool(t, tool, promptSessionCtx(), CallMCPPromptToolName, CallMCPPromptParams{MCPName: "srv", PromptName: "greet"})
	require.NoError(t, err)
	require.Equal(t, 1, perms.requestCount)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "denied permission")
}

func TestCallMCPPromptTool_GrantedReachesMCP(t *testing.T) {
	cfg := config.NewTestStore(&config.Config{})
	perms := newPromptPerms(true)
	tool := NewCallMCPPromptTool(cfg, perms)

	resp, err := runPromptTool(t, tool, promptSessionCtx(), CallMCPPromptToolName, CallMCPPromptParams{MCPName: "ghost-call", PromptName: "greet"})
	require.NoError(t, err)
	require.Equal(t, 1, perms.requestCount)
	require.True(t, resp.IsError)
}
