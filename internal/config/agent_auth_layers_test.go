package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An agent's auth is bound to the layer that set its card (#434): a
// project config that repoints a card does not inherit the global token,
// so it cannot send that token to the project's origin.
func TestAgentAuthBindsToCardLayer(t *testing.T) {
	t.Parallel()
	global := []byte(`{"agents": {
		"reviewer": {"role": "dispatch", "runtime": "a2a", "card": "https://global.example/card.json", "auth": {"type": "bearer", "token": "$GLOBAL_TOKEN"}},
		"auditor": {"role": "dispatch", "runtime": "a2a", "card": "https://audit.example/card.json", "auth": {"type": "bearer", "token": "$AUDIT_TOKEN"}}
	}}`)

	t.Run("card without auth drops the inherited auth", func(t *testing.T) {
		t.Parallel()
		project := []byte(`{"agents": {"reviewer": {"card": "https://project.example/card.json"}}}`)
		cfg, err := loadFromBytes([][]byte{global, project})
		require.NoError(t, err)
		reviewer := cfg.AgentDefinitions["reviewer"]
		require.Equal(t, "https://project.example/card.json", *reviewer.Card)
		require.Nil(t, reviewer.Auth, "the global token must not follow the card to another origin")

		auditor := cfg.AgentDefinitions["auditor"]
		require.NotNil(t, auditor.Auth, "an agent the project left alone keeps its auth")
		require.Equal(t, "$AUDIT_TOKEN", *auditor.Auth.Token)
	})

	t.Run("card with auth keeps the layer's own auth", func(t *testing.T) {
		t.Parallel()
		project := []byte(`{"agents": {"reviewer": {"card": "https://project.example/card.json", "auth": {"token": "$PROJECT_TOKEN"}}}}`)
		cfg, err := loadFromBytes([][]byte{global, project})
		require.NoError(t, err)
		auth := cfg.AgentDefinitions["reviewer"].Auth
		require.NotNil(t, auth)
		require.Equal(t, "$PROJECT_TOKEN", *auth.Token)
		require.Nil(t, auth.Type, "nothing of the global auth survives, not even its type")
	})

	t.Run("a layer that leaves the card alone keeps the auth", func(t *testing.T) {
		t.Parallel()
		project := []byte(`{"agents": {"reviewer": {"name": "Reviewer"}}}`)
		cfg, err := loadFromBytes([][]byte{global, project})
		require.NoError(t, err)
		auth := cfg.AgentDefinitions["reviewer"].Auth
		require.NotNil(t, auth)
		require.Equal(t, "$GLOBAL_TOKEN", *auth.Token)
	})

	t.Run("layers are not modified", func(t *testing.T) {
		t.Parallel()
		before := string(global)
		_, err := loadFromBytes([][]byte{global, []byte(`{"agents": {"reviewer": {"card": "https://project.example/card.json"}}}`)})
		require.NoError(t, err)
		require.Equal(t, before, string(global))
	})
}
