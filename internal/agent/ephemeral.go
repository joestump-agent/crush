package agent

import (
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// EphemeralAgent bundles a [SessionAgent] with its own private
// in-memory session and message stores. It is the foundation for
// Sidekick-style fire-and-forget runs:
//
//   - Sessions and messages live only in process memory — nothing is
//     written to SQLite, so ephemeral sessions never appear in the
//     session list and are destroyed when Crush exits.
//   - Title generation, auto-summarize, and notifications are disabled.
//   - Busy sessions reject new calls with [ErrSessionBusy] instead of
//     queueing them.
//   - Busy tracking ([SessionAgent.IsSessionBusy]) is fully independent
//     of any other agent because the agent instance owns its own active
//     request registry.
//
// The exposed Sessions and Messages services publish events on their
// own private brokers, so a UI can subscribe to streaming updates for
// ephemeral runs without those events ever reaching the app-wide
// session or message brokers.
type EphemeralAgent struct {
	SessionAgent

	// Sessions is the private in-memory session store backing this
	// agent. Create ephemeral sessions here.
	Sessions session.Service
	// Messages is the private in-memory message store backing this
	// agent. Subscribe here to observe streaming message updates.
	Messages message.Service
}

// NewEphemeralAgent builds an [EphemeralAgent] from opts. The Sessions,
// Messages, Ephemeral, DisableAutoSummarize, and Notify fields of opts
// are overridden to enforce the ephemeral contract; everything else
// (models, prompt, tools, config) is used as provided.
func NewEphemeralAgent(opts SessionAgentOptions) *EphemeralAgent {
	sessions := session.NewInMemoryService()
	messages := message.NewInMemoryService()
	opts.Sessions = sessions
	opts.Messages = messages
	opts.Ephemeral = true
	return &EphemeralAgent{
		SessionAgent: NewSessionAgent(opts),
		Sessions:     sessions,
		Messages:     messages,
	}
}
