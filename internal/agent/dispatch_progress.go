package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	a2ui "github.com/tmc/a2ui"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

// dispatchSnapshot is the reduced progress of one active dispatch: the
// todos its agent has recorded so far, plus the coordinates that let a
// sink render or route it.
type dispatchSnapshot struct {
	DispatchID string
	Workspace  string
	Status     string
	Todos      []session.Todo
}

// progressSink receives the full, ordered set of active dispatch
// snapshots on every change. Sinks must return promptly (they run on the
// collector's emit path) and must not mutate the slice. The dashboard
// sink composes an A2UI surface (#65); #174 attaches a second sink that
// re-emits the same snapshots as A2A TaskStatusUpdateEvents — the
// subscription and reduction below are written once and sunk twice.
type progressSink interface {
	emit(snapshots []dispatchSnapshot)
}

// dispatchProgress collects todo progress from dispatched sessions and
// fans reduced snapshots to sinks. There is one collector per
// coordinator; each dispatch registers with track and deregisters with
// untrack. It is safe for concurrent use.
type dispatchProgress struct {
	sinks []progressSink

	mu      sync.Mutex
	active  map[string]*dispatchSnapshot
	order   []string
	cancels map[string]context.CancelFunc
}

func newDispatchProgress(sinks ...progressSink) *dispatchProgress {
	return &dispatchProgress{
		sinks:   sinks,
		active:  make(map[string]*dispatchSnapshot),
		cancels: make(map[string]context.CancelFunc),
	}
}

// track begins collecting todos for a dispatch. It subscribes to the
// dispatch's (ephemeral) session store and, on every update to that
// session, refreshes the snapshot's todos and re-emits to all sinks. The
// subscription runs until untrack is called or ctx is canceled. Passing
// a nil collector is a no-op so dispatch works when progress is disabled.
func (p *dispatchProgress) track(ctx context.Context, dispatchID, workspace, sessionID string, sessions session.Service) {
	if p == nil || sessions == nil {
		return
	}

	subCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.active[dispatchID] = &dispatchSnapshot{
		DispatchID: dispatchID,
		Workspace:  workspace,
		Status:     dispatchStatusRunning,
	}
	p.order = append(p.order, dispatchID)
	p.cancels[dispatchID] = cancel
	p.mu.Unlock()

	events := sessions.Subscribe(subCtx)
	p.emit()

	go func() {
		for ev := range events {
			if ev.Payload.ID != sessionID {
				continue
			}
			p.mu.Lock()
			if snap, ok := p.active[dispatchID]; ok {
				snap.Todos = ev.Payload.Todos
			}
			p.mu.Unlock()
			p.emit()
		}
	}()
}

// untrack stops collecting for a dispatch and drops it from the active
// set, re-emitting so the surface reflects its completion (collapsing to
// empty when it was the last one).
func (p *dispatchProgress) untrack(dispatchID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if cancel, ok := p.cancels[dispatchID]; ok {
		cancel()
		delete(p.cancels, dispatchID)
	}
	delete(p.active, dispatchID)
	for i, id := range p.order {
		if id == dispatchID {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	p.emit()
}

// emit snapshots the active set under the lock and fans it to every sink
// outside the lock, so a slow sink never blocks a tracked session's event
// loop.
func (p *dispatchProgress) emit() {
	p.mu.Lock()
	snaps := make([]dispatchSnapshot, 0, len(p.order))
	for _, id := range p.order {
		if snap, ok := p.active[id]; ok {
			snaps = append(snaps, *snap)
		}
	}
	sinks := p.sinks
	p.mu.Unlock()

	for _, sink := range sinks {
		sink.emit(snaps)
	}
}

// dispatchStatusRunning is the snapshot status while a dispatch's agent
// is working. (Terminal states drop out of the active set on untrack.)
const dispatchStatusRunning = "running"

const (
	// dispatchSurfaceID is the A2UI surface the composite dashboard card
	// is published under, stable across updates so the client composites
	// in place.
	dispatchSurfaceID = "dispatch-progress"
	dispatchRootID    = "dispatch-progress-root"
)

// dashboardSink is the #65 sink: it composes the active dispatches into a
// single A2UI surface and publishes it to the Sidekick dashboard broker,
// the same pinned slot the sidekick_update tool feeds (#56/#57).
type dashboardSink struct {
	dashboard pubsub.Publisher[tools.SidekickSurface]
}

func (s dashboardSink) emit(snapshots []dispatchSnapshot) {
	if s.dashboard == nil {
		return
	}
	s.dashboard.Publish(pubsub.UpdatedEvent, composeDispatchSurface(snapshots))
}

// composeDispatchSurface renders the active dispatches into one A2UI
// surface: a column of one text line per dispatch (workspace, status,
// todo progress). With nothing active it emits a DeleteSurface so the
// dashboard slot collapses.
func composeDispatchSurface(snapshots []dispatchSnapshot) tools.SidekickSurface {
	msg := a2ui.ServerMessage{Version: a2ui.Version}

	if len(snapshots) == 0 {
		msg.DeleteSurface = &a2ui.DeleteSurface{SurfaceID: dispatchSurfaceID}
	} else {
		childIDs := make([]string, 0, len(snapshots))
		components := make([]a2ui.Component, 0, len(snapshots)+1)
		for i, snap := range snapshots {
			id := fmt.Sprintf("dispatch-%d", i)
			childIDs = append(childIDs, id)
			components = append(components, a2ui.Component{
				ID:   id,
				Text: &a2ui.TextComponent{Text: a2ui.StringLiteral(formatDispatchLine(snap))},
			})
		}
		// The root column must precede its children in the component list.
		components = append([]a2ui.Component{{
			ID:     dispatchRootID,
			Column: &a2ui.ColumnComponent{Children: a2ui.ChildList{IDs: childIDs}},
		}}, components...)

		msg.UpdateComponents = &a2ui.UpdateComponents{
			SurfaceID:  dispatchSurfaceID,
			Components: components,
		}
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		// The message is built from fixed shapes and plain strings, so a
		// marshal error is not reachable in practice; degrade to an empty
		// surface rather than panic on the collector's hot path.
		return tools.SidekickSurface{}
	}
	return tools.SidekickSurface{Content: "<a2ui-json>" + string(payload) + "</a2ui-json>"}
}

// formatDispatchLine summarizes one dispatch: its workspace, status, todo
// completion count, and the currently in-progress todo when there is one.
func formatDispatchLine(snap dispatchSnapshot) string {
	done := 0
	active := ""
	for _, todo := range snap.Todos {
		if todo.Status == session.TodoStatusCompleted {
			done++
		}
		if todo.Status == session.TodoStatusInProgress && active == "" {
			active = todo.Content
		}
	}

	line := fmt.Sprintf("%s · %s · %d/%d", snap.Workspace, snap.Status, done, len(snap.Todos))
	if active != "" {
		line += " · " + active
	}
	return line
}
