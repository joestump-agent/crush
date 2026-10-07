package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
)

// The #392 contract: every agent definition appears in the host's card
// listing, one card per agent, published at its own /agents/<id> route —
// a live protocol surface that rejects runs until the definition's entry
// point serves a turn on it.
func TestPublishAgentDefinitionCardListing(t *testing.T) {
	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	cfg.SetupAgents()
	definitions := cfg.Config().Agents
	require.NotEmpty(t, definitions, "the built-in definitions must be present")

	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })

	for id, ag := range definitions {
		require.NoError(t, factory.PublishAgentDefinition(t.Context(), agent.AgentDefinitionCard{
			ID:          id,
			Name:        ag.Name,
			Description: ag.Description,
		}), "publish %s", id)
	}

	cards := factory.AgentCards()
	require.Len(t, cards, len(definitions), "one card per agent definition")

	byName := make(map[string]*a2aspec.AgentCard, len(cards))
	for _, card := range cards {
		byName[card.Name] = card
	}
	for id, ag := range definitions {
		card, ok := byName[ag.Name]
		require.True(t, ok, "definition %s (%q) must appear in the listing", id, ag.Name)
		require.Equal(t, ag.Description, card.Description)
		require.NotEmpty(t, card.SupportedInterfaces)
		require.Contains(t, card.SupportedInterfaces[0].URL, agentsPathPrefix+id,
			"the card serves at the definition's own route")
	}

	// Republishing is a no-op that keeps the first card.
	first := definitions[config.AgentCoder]
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), agent.AgentDefinitionCard{
		ID:          config.AgentCoder,
		Name:        first.Name,
		Description: first.Description,
	}))
	require.Len(t, factory.AgentCards(), len(definitions))
}

// A definition route is a live protocol surface (#392): a message on it
// with no run bound to its context is rejected without a runner — task
// sessions are not continuable — while the definition's card stays
// served.
func TestPublishAgentDefinitionRejectsUnboundRuns(t *testing.T) {
	factory := NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	require.NoError(t, factory.PublishAgentDefinition(t.Context(), agent.AgentDefinitionCard{
		ID:          "coder",
		Name:        "Coder",
		Description: "An agent that helps with executing coding tasks.",
	}))

	client := unixDialClient(factory)
	resp, err := postJSONRPC(t, client, "http://"+a2aURLHost+agentsPathPrefix+"coder",
		sendMessageBody(t, "no-such-run", "hello"))
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
	require.NotNil(t, rpcResp.Result.Task)
	require.Equal(t, a2aspec.TaskStateRejected, rpcResp.Result.Task.Status.State)
	require.NotNil(t, rpcResp.Result.Task.Status.Message, "the terminal status carries its message")
	require.Contains(t, partsText(rpcResp.Result.Task.Status.Message.Parts), "no running agent for context",
		"the rejection names the unresolvable context")
}
