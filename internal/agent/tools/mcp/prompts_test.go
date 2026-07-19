package mcp

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// livePromptSession spins up an in-memory MCP server that exposes a single
// prompt (which templates its "name" argument) and returns a connected
// *ClientSession, mirroring liveSession in lifecycle_test.go.
func livePromptSession(t *testing.T) *ClientSession {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "srv"}, nil)
	server.AddPrompt(
		&mcp.Prompt{
			Name:        "greet",
			Description: "Greet someone by name",
			Arguments: []*mcp.PromptArgument{
				{Name: "name", Description: "Who to greet", Required: true},
			},
		},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			who := req.Params.Arguments["name"]
			return &mcp.GetPromptResult{
				Messages: []*mcp.PromptMessage{
					{Role: "user", Content: &mcp.TextContent{Text: "Hello, " + who + "!"}},
				},
			}, nil
		},
	)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	client := mcp.NewClient(&mcp.Implementation{Name: "crush-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	return &ClientSession{ClientSession: clientSession, cancel: cancel}
}

func TestListPrompts_ReturnsServerPrompts(t *testing.T) {
	const name = "test-list-prompts"
	t.Cleanup(func() {
		sessions.Del(name)
		allPrompts.Del(name)
		states.Del(name)
	})

	sess := livePromptSession(t)
	t.Cleanup(func() { _ = sess.Close() })
	sessions.Set(name, sess)

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	prompts, err := ListPrompts(context.Background(), cfg, name)
	require.NoError(t, err)
	require.Len(t, prompts, 1)
	require.Equal(t, "greet", prompts[0].Name)
	require.Equal(t, "Greet someone by name", prompts[0].Description)
	require.Len(t, prompts[0].Arguments, 1)
	require.Equal(t, "name", prompts[0].Arguments[0].Name)
	require.True(t, prompts[0].Arguments[0].Required)

	// ListPrompts should have refreshed the cache and the prompt count.
	cached, ok := allPrompts.Get(name)
	require.True(t, ok)
	require.Len(t, cached, 1)
	info, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, 1, info.Counts.Prompts)
}

func TestGetPromptMessages_RendersArguments(t *testing.T) {
	const name = "test-get-prompt"
	t.Cleanup(func() {
		sessions.Del(name)
		allPrompts.Del(name)
		states.Del(name)
	})

	sess := livePromptSession(t)
	t.Cleanup(func() { _ = sess.Close() })
	sessions.Set(name, sess)

	cfg := config.NewTestStore(&config.Config{MCP: config.MCPs{name: {Type: config.MCPStdio}}})

	messages, err := GetPromptMessages(context.Background(), cfg, name, "greet", map[string]string{"name": "Ada"})
	require.NoError(t, err)
	require.Equal(t, []string{"Hello, Ada!"}, messages)
}
