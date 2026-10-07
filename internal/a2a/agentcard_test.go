package a2a

import (
	"testing"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
)

func TestBuildAgentCard(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent: config.Agent{
			ID:          "reviewer",
			Name:        "Reviewer",
			Description: "Reviews a worktree diff",
			Model:       config.SelectedModelTypeSmall,
		},
		Skills: []*skills.Skill{
			{Name: "code-review", Description: "Review a diff for bugs"},
			{Name: "run-tests", Description: "Run the test suite"},
		},
		Endpoint: "http://127.0.0.1:8080",
		Version:  "1.2.3",
	})

	require.Equal(t, "Reviewer", card.Name)
	require.Equal(t, "Reviews a worktree diff", card.Description)
	require.Equal(t, "1.2.3", card.Version)
	require.True(t, card.Capabilities.Streaming)

	require.Len(t, card.SupportedInterfaces, 1)
	iface := card.SupportedInterfaces[0]
	require.Equal(t, "http://127.0.0.1:8080", iface.URL)
	require.Equal(t, a2aspec.TransportProtocolJSONRPC, iface.ProtocolBinding)
	require.Equal(t, a2aspec.Version, iface.ProtocolVersion)

	require.Equal(t, []string{"text/plain"}, card.DefaultInputModes)
	require.Equal(t, []string{"text/plain"}, card.DefaultOutputModes)

	require.Len(t, card.Skills, 2)
	first := card.Skills[0]
	require.Equal(t, "code-review", first.ID)
	require.Equal(t, "code-review", first.Name)
	require.Equal(t, "Review a diff for bugs", first.Description)
	require.NotEmpty(t, first.Tags, "A2A requires a non-empty tags list")
}

// The card declares the registry's extensions (#359): one entry per
// registered extension, optional, with the JSON Schema discoverable in the
// params so third-party clients can negotiate and validate the metadata.
func TestBuildAgentCardDeclaresExtensions(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
	})

	require.Len(t, card.Capabilities.Extensions, len(Registered()))
	byURI := make(map[string]a2aspec.AgentExtension, len(card.Capabilities.Extensions))
	for _, ext := range card.Capabilities.Extensions {
		byURI[ext.URI] = ext
	}
	todoExt, ok := byURI[TodoExtensionURI]
	require.True(t, ok, "the todos/v1 extension is declared on the card")
	require.False(t, todoExt.Required)
	require.Equal(t, TodoExt.Description, todoExt.Description)
	require.Contains(t, todoExt.Params, "schema")
	schema, ok := todoExt.Params["schema"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "#/$defs/TodoProgress", schema["$ref"], "the schema describes the TodoProgress object")
	require.Contains(t, schema, "$defs")
}

func TestBuildAgentCardDefaultTransport(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
	})
	require.Equal(t, a2aspec.TransportProtocolJSONRPC, card.SupportedInterfaces[0].ProtocolBinding)
}

func TestBuildAgentCardExplicitTransport(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent:     config.Agent{Name: "worker"},
		Endpoint:  "http://127.0.0.1:9000",
		Transport: a2aspec.TransportProtocolGRPC,
	})
	require.Equal(t, a2aspec.TransportProtocolGRPC, card.SupportedInterfaces[0].ProtocolBinding)
}

func TestBuildAgentCardNoSkills(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
	})
	// Required field: must be non-nil so it marshals as [] not null.
	require.NotNil(t, card.Skills)
	require.Empty(t, card.Skills)
}

func TestBuildAgentCardSkipsNilSkills(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
		Skills:   []*skills.Skill{nil, {Name: "real"}, nil},
	})
	require.Len(t, card.Skills, 1)
	require.Equal(t, "real", card.Skills[0].Name)
}

func TestCardNameFallbacks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		agent config.Agent
		want  string
	}{
		{"name wins", config.Agent{ID: "id", Name: "Name"}, "Name"},
		{"id fallback", config.Agent{ID: "id"}, "id"},
		{"generic fallback", config.Agent{}, "crush-agent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, cardName(tc.agent))
		})
	}
}

// The card declares the bearer security scheme served calls are
// authenticated with (#357), and requires it — so a caller without the
// host's token is told what the card demands before it dials.
func TestBuildAgentCardDeclaresBearerSecurity(t *testing.T) {
	t.Parallel()

	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{ID: "reviewer", Name: "Reviewer"},
		Endpoint: "http://crush-a2a/agents/reviewer",
		Version:  "1.2.3",
	})

	require.Contains(t, card.SecuritySchemes, bearerSchemeName)
	scheme, ok := card.SecuritySchemes[bearerSchemeName].(a2aspec.HTTPAuthSecurityScheme)
	require.True(t, ok, "the declared scheme must be the HTTP auth kind")
	require.Equal(t, "bearer", scheme.Scheme)

	require.Len(t, card.SecurityRequirements, 1)
	require.Contains(t, card.SecurityRequirements[0], bearerSchemeName)
	require.Empty(t, card.SecurityRequirements[0][bearerSchemeName],
		"the bearer requirement carries no scopes")
}
