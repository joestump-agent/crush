package agent

import (
	"context"
	"log/slog"
	"slices"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/skills"
)

// DispatchServerStarter starts an in-process A2A server for one
// dispatched agent (#70). It is the seam that keeps the dependency
// direction one-way — internal/a2a imports internal/agent for the
// Executor's runner, so the agent package only ever sees this
// interface. The production implementation is a2a.ServerFactory; tests
// substitute fakes through it, and a nil starter (the default until the
// app wires the factory) simply serves nothing.
type DispatchServerStarter interface {
	// StartDispatchServer stands up the loopback A2A server for one
	// dispatch and returns its endpoint, the AgentCard to stamp on the
	// registry entry (opaque here), and the stop function the dispatch
	// run defers.
	StartDispatchServer(ctx context.Context, params DispatchServerParams) (endpoint string, card any, stop func(), err error)
}

// DispatchServerParams is one dispatch's slice of the A2A server (#70):
// what to serve, on which session, and where its card's identity comes
// from. Todos is spelled structurally so the agent package stays
// import-clean of internal/a2a; the production value is the dispatch
// registry's todo collector.
type DispatchServerParams struct {
	// SessionID is the ephemeral task session backing the dispatch.
	SessionID string
	// Runner is the dispatched agent itself.
	Runner SessionAgent
	// Diff collects the completion artifact (the workspace diff).
	Diff func(ctx context.Context) (string, error)
	// Todos streams per-session progress snapshots for
	// TaskStatusUpdateEvents (#174).
	Todos interface {
		SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot
	}
	// Name is the card's agent name — the dispatch's assigned handle.
	Name string
	// Description is the card's one-line description — the dispatch's
	// role.
	Description string
	// Skills are the skills the dispatch was given.
	Skills []*skills.Skill
}

// SetDispatchServerStarter wires the A2A server factory (#70). Call once
// at app construction, before the first dispatch; nil disables serving
// (tests, or a build without the factory wired).
func (c *coordinator) SetDispatchServerStarter(starter DispatchServerStarter) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	c.dispatchServer = starter
}

// dispatchServerStarter returns the wired A2A server starter, if any.
func (c *coordinator) dispatchServerStarter() DispatchServerStarter {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	return c.dispatchServer
}

// startDispatchServer stands up the dispatch's A2A server (#70) and
// stamps its endpoint and card on the registry entry — the in-memory
// discovery surface. A start failure is logged and swallowed: the
// dispatch itself does not depend on being served, and Phase 1 has no
// A2A client in the loop yet (#71 adds it); failing the dispatch over a
// loopback server would trade working dispatches for protocol purity.
func (c *coordinator) startDispatchServer(ctx context.Context, workspace *dispatch.Workspace, entryID, sessionID, handle, role string, runner SessionAgent, loaded []*skills.Skill) (stop func()) {
	starter := c.dispatchServerStarter()
	if starter == nil {
		return nil
	}

	endpoint, card, stop, err := starter.StartDispatchServer(ctx, DispatchServerParams{
		SessionID:   sessionID,
		Runner:      runner,
		Diff:        func(ctx context.Context) (string, error) { return workspace.Diff(ctx, entryID) },
		Todos:       c.dispatchCollector,
		Name:        handle,
		Description: role,
		Skills:      loaded,
	})
	if err != nil {
		slog.Warn("Dispatch A2A server failed to start", "dispatch_id", entryID, "error", err)
		return nil
	}
	workspace.SetEndpoint(entryID, endpoint, card)
	slog.Debug("Dispatch A2A server started", "dispatch_id", entryID, "endpoint", endpoint)
	return stop
}

// resolvedSkills loads the skills a dispatch runs with from its scoped
// store: every discovered skill when nothing was requested, else exactly
// the requested ones. The card advertises what the agent actually has.
func resolvedSkills(store *config.ConfigStore, requested []string) []*skills.Skill {
	if store == nil {
		return nil
	}
	all, _ := discoverSkills(store)
	if len(requested) == 0 {
		return all
	}
	var out []*skills.Skill
	for _, s := range all {
		if slices.Contains(requested, s.Name) {
			out = append(out, s)
		}
	}
	return out
}

// stopDispatchServer tears a dispatch's A2A server down and clears the
// registry entry's endpoint and card: a finished dispatch serves
// nothing, and discovery must not hand out a dead endpoint (#70's
// teardown half).
func (c *coordinator) stopDispatchServer(workspace *dispatch.Workspace, entryID string, stop func()) {
	if stop != nil {
		stop()
	}
	workspace.SetEndpoint(entryID, "", nil)
}
