package a2a

import (
	"testing"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
)

func TestBuildAgentCard(t *testing.T) {
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

	if card.Name != "Reviewer" {
		t.Errorf("Name = %q, want %q", card.Name, "Reviewer")
	}
	if card.Description != "Reviews a worktree diff" {
		t.Errorf("Description = %q, want %q", card.Description, "Reviews a worktree diff")
	}
	if card.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", card.Version, "1.2.3")
	}
	if !card.Capabilities.Streaming {
		t.Error("Capabilities.Streaming = false, want true")
	}

	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("SupportedInterfaces = %d, want 1", len(card.SupportedInterfaces))
	}
	iface := card.SupportedInterfaces[0]
	if iface.URL != "http://127.0.0.1:8080" {
		t.Errorf("interface URL = %q, want %q", iface.URL, "http://127.0.0.1:8080")
	}
	if iface.ProtocolBinding != a2aspec.TransportProtocolJSONRPC {
		t.Errorf("interface transport = %q, want %q", iface.ProtocolBinding, a2aspec.TransportProtocolJSONRPC)
	}
	if iface.ProtocolVersion != a2aspec.Version {
		t.Errorf("interface protocol version = %q, want %q", iface.ProtocolVersion, a2aspec.Version)
	}

	if want := []string{"text/plain"}; !equalStrings(card.DefaultInputModes, want) {
		t.Errorf("DefaultInputModes = %v, want %v", card.DefaultInputModes, want)
	}
	if want := []string{"text/plain"}; !equalStrings(card.DefaultOutputModes, want) {
		t.Errorf("DefaultOutputModes = %v, want %v", card.DefaultOutputModes, want)
	}

	if len(card.Skills) != 2 {
		t.Fatalf("Skills = %d, want 2", len(card.Skills))
	}
	first := card.Skills[0]
	if first.ID != "code-review" || first.Name != "code-review" {
		t.Errorf("skill[0] id/name = %q/%q, want code-review/code-review", first.ID, first.Name)
	}
	if first.Description != "Review a diff for bugs" {
		t.Errorf("skill[0] description = %q", first.Description)
	}
	if len(first.Tags) == 0 {
		t.Error("skill[0] Tags is empty; A2A requires a non-empty tags list")
	}
}

func TestBuildAgentCardDefaultTransport(t *testing.T) {
	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
	})
	if got := card.SupportedInterfaces[0].ProtocolBinding; got != a2aspec.TransportProtocolJSONRPC {
		t.Errorf("default transport = %q, want %q", got, a2aspec.TransportProtocolJSONRPC)
	}
}

func TestBuildAgentCardExplicitTransport(t *testing.T) {
	card := BuildAgentCard(CardParams{
		Agent:     config.Agent{Name: "worker"},
		Endpoint:  "http://127.0.0.1:9000",
		Transport: a2aspec.TransportProtocolGRPC,
	})
	if got := card.SupportedInterfaces[0].ProtocolBinding; got != a2aspec.TransportProtocolGRPC {
		t.Errorf("transport = %q, want %q", got, a2aspec.TransportProtocolGRPC)
	}
}

func TestBuildAgentCardNoSkills(t *testing.T) {
	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
	})
	// Required field: must be non-nil so it marshals as [] not null.
	if card.Skills == nil {
		t.Error("Skills is nil; want non-nil empty slice for a valid card")
	}
	if len(card.Skills) != 0 {
		t.Errorf("Skills = %d, want 0", len(card.Skills))
	}
}

func TestBuildAgentCardSkipsNilSkills(t *testing.T) {
	card := BuildAgentCard(CardParams{
		Agent:    config.Agent{Name: "worker"},
		Endpoint: "http://127.0.0.1:9000",
		Skills:   []*skills.Skill{nil, {Name: "real"}, nil},
	})
	if len(card.Skills) != 1 {
		t.Fatalf("Skills = %d, want 1 (nils skipped)", len(card.Skills))
	}
	if card.Skills[0].Name != "real" {
		t.Errorf("skill name = %q, want real", card.Skills[0].Name)
	}
}

func TestCardNameFallbacks(t *testing.T) {
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
			if got := cardName(tc.agent); got != tc.want {
				t.Errorf("cardName = %q, want %q", got, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
