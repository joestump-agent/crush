package a2a

import (
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
)

// A dispatched Crush agent is prompted with, and replies in, plain text; its
// work product (a git diff) is also text. These are the card's default MIME
// modes, overridable per-skill by the spec but unused as such in Phase 1.
var (
	defaultInputModes  = []string{"text/plain"}
	defaultOutputModes = []string{"text/plain"}
)

// skillTag is the single tag applied to every derived A2A skill.
// AgentSkill.Tags is a required (non-omitempty) field, and Crush skills have
// no native tag concept, so a stable marker keeps the card spec-valid
// without inventing per-skill taxonomy.
const skillTag = "crush-skill"

// CardParams are the inputs needed to describe a dispatched worktree agent as
// an A2A [a2aspec.AgentCard]. It is populated from the same dispatch config the
// DispatchAgent tool (#64) takes: the agent definition, the skills loaded for
// it, and the endpoint it serves A2A on.
type CardParams struct {
	// Agent is the dispatched agent's configuration — name, description,
	// model, tools. Required.
	Agent config.Agent

	// Skills are the Crush skills loaded for this agent. Each becomes one
	// A2A skill entry on the card. Optional.
	Skills []*skills.Skill

	// Endpoint is the base URL the agent serves A2A on (the process's
	// unix-socket host in Phase 1, #346). Required for a resolvable card.
	Endpoint string

	// TLSEndpoint is the agent's URL on the host's optional TCP listener
	// (#358), https://<host:port>/agents/<id>. When set, the card lists
	// it as a second interface after Endpoint, so a client that picks
	// the first interface it supports keeps dialing the socket. Empty
	// (no TCP listener) lists Endpoint alone.
	TLSEndpoint string

	// MutualTLS declares the TCP listener's client-certificate scheme
	// (#358) as an alternative to the bearer token. Only meaningful with
	// TLSEndpoint.
	MutualTLS bool

	// Transport is the protocol binding served at Endpoint. Defaults to
	// JSON-RPC over HTTP — the Phase 1 transport — when empty.
	Transport a2aspec.TransportProtocol

	// Version is the agent/build version advertised on the card.
	Version string
}

// BuildAgentCard derives a spec-valid A2A AgentCard from a dispatched agent's
// config. The card advertises the agent's identity, its endpoint and
// transport, streaming capability (dispatched runs stream progress via SSE),
// one A2A skill per loaded Crush skill, and the bearer security scheme served
// calls are authenticated with (#357) — declared as required, so a caller
// without the host's token is told what the card demands.
func BuildAgentCard(p CardParams) *a2aspec.AgentCard {
	transport := p.Transport
	if transport == "" {
		transport = a2aspec.TransportProtocolJSONRPC
	}

	// NewAgentInterface stamps the SDK's protocol version. The socket
	// interface stays first: the SDK client tries interfaces in card
	// order, and the in-process dispatch client dials the socket.
	interfaces := []*a2aspec.AgentInterface{a2aspec.NewAgentInterface(p.Endpoint, transport)}
	if p.TLSEndpoint != "" {
		interfaces = append(interfaces, a2aspec.NewAgentInterface(p.TLSEndpoint, transport))
	}

	return &a2aspec.AgentCard{
		Name:                cardName(p.Agent),
		Description:         p.Agent.Description,
		Version:             p.Version,
		SupportedInterfaces: interfaces,
		Capabilities: a2aspec.AgentCapabilities{
			// Dispatched agents stream todo/progress updates over SSE.
			Streaming: true,
			// Declared A2A extensions (#359): the metadata the
			// TaskStatusUpdateEvents carry, with their schemas.
			Extensions: cardExtensions(),
		},
		DefaultInputModes:    defaultInputModes,
		DefaultOutputModes:   defaultOutputModes,
		Skills:               agentSkills(p.Skills),
		SecuritySchemes:      securitySchemes(p),
		SecurityRequirements: securityRequirements(p),
	}
}

// securitySchemes declares how served calls authenticate (#357): the
// host's per-process bearer token, plus — when the TCP listener requires
// client certificates (#358) — mutual TLS.
func securitySchemes(p CardParams) a2aspec.NamedSecuritySchemes {
	schemes := a2aspec.NamedSecuritySchemes{
		bearerSchemeName: a2aspec.HTTPAuthSecurityScheme{
			Scheme:      "bearer",
			Description: "Per-process bearer token minted by the serving Crush process.",
		},
	}
	if p.TLSEndpoint != "" && p.MutualTLS {
		schemes[mtlsSchemeName] = a2aspec.MutualTLSSecurityScheme{
			Description: "Client certificate verified against the TCP listener's client CA.",
		}
	}
	return schemes
}

// securityRequirements lists the alternatives a call may satisfy:
// the bearer token always, and a verified client certificate on the
// TCP listener when it requires one (#358). Scopes are empty — the
// credential either matches or the call is rejected.
func securityRequirements(p CardParams) a2aspec.SecurityRequirementsOptions {
	reqs := a2aspec.SecurityRequirementsOptions{
		{bearerSchemeName: {}},
	}
	if p.TLSEndpoint != "" && p.MutualTLS {
		reqs = append(reqs, a2aspec.SecurityRequirements{mtlsSchemeName: {}})
	}
	return reqs
}

// cardName resolves a human-readable card name from the agent config, falling
// back through the agent ID to a generic label so the required Name field is
// never empty.
func cardName(a config.Agent) string {
	switch {
	case a.Name != "":
		return a.Name
	case a.ID != "":
		return a.ID
	default:
		return "crush-agent"
	}
}

// agentSkills maps each loaded Crush skill onto an A2A skill entry. A skill's
// name doubles as its A2A id — skill names are unique after discovery dedupe.
// It always returns a non-nil slice so the card's required "skills" field
// marshals as [] rather than null when the agent has no skills.
func agentSkills(sk []*skills.Skill) []a2aspec.AgentSkill {
	out := make([]a2aspec.AgentSkill, 0, len(sk))
	for _, s := range sk {
		if s == nil {
			continue
		}
		out = append(out, a2aspec.AgentSkill{
			ID:          s.Name,
			Name:        s.Name,
			Description: s.Description,
			Tags:        []string{skillTag},
		})
	}
	return out
}
