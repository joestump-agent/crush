package dispatch

import (
	"context"
	"sync"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
)

// TodoSnapshot is the reduced progress state of one dispatch (#65): the
// registry entry (lifecycle state, session, handle, timestamps, and —
// once terminal — the DispatchResult) plus the dispatched session's
// todo list and usage as of the last save. The agent block renders its
// live status card from it, and #174's A2A bridge re-emits the same
// snapshots as TaskStatusUpdateEvents.
type TodoSnapshot struct {
	// Entry is the registry entry the snapshot was reduced from, by
	// value. Status is the dispatch state, SessionID locates the agent
	// block (it parses back to the dispatch_agent tool call that
	// created the session), StartedAt/FinishedAt bound the elapsed
	// time, and Result carries the terminal payload once the run
	// finished.
	Entry Entry
	// CurrentTodo is the dispatched agent's in-progress todo — its
	// active form when set, its content otherwise. Empty when the agent
	// has no todo in progress.
	CurrentTodo string
	// TodoCompleted and TodoTotal count the dispatched agent's todos.
	TodoCompleted int
	TodoTotal     int
	// PromptTokens, CompletionTokens, and Cost mirror the dispatched
	// session's usage at reduction time.
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
}

// TodoSink receives per-dispatch [TodoSnapshot]s from a [TodoCollector].
// Sinks are called on the collector's goroutine and must not block: the
// first sink publishes onto a broker for the UI (#65), and #174's A2A
// TaskStatusUpdateEvent bridge attaches as a second sink over the same
// reduction.
type TodoSink interface {
	// DispatchTodos delivers one reduced snapshot. Terminal snapshots
	// (completed/failed/killed) are the dispatch's durable record and
	// must not be silently coalesced away.
	DispatchTodos(TodoSnapshot)
}

// TodoCollector reduces dispatch progress for every sink: it subscribes
// once to the session event stream (a [pubsub.Broker[Session]] publishes
// an UpdatedEvent on every save, and [session.Todos] plus usage ride on
// it) and to the registry's own entry stream, filters both to dispatched
// sessions, and fans each reduction out to every registered sink. The
// subscription and reduction are written once and sunk twice — the
// agent block is the first sink, #174's A2A event bridge the second.
type TodoCollector struct {
	ws       *Workspace
	sessions pubsub.Subscriber[session.Session]
	sinks    []TodoSink

	mu sync.Mutex
	// latest holds the most recent snapshot per dispatch ID, so a
	// registry transition can re-emit a snapshot without waiting for
	// the next session save, and Snapshot can answer a pull.
	latest map[string]TodoSnapshot
}

// NewTodoCollector returns a collector reducing progress for the
// workspaces in ws, reading session state from sessions, and delivering
// snapshots to sinks. Call [TodoCollector.Run] to start it.
func NewTodoCollector(ws *Workspace, sessions pubsub.Subscriber[session.Session], sinks ...TodoSink) *TodoCollector {
	return &TodoCollector{
		ws:       ws,
		sessions: sessions,
		sinks:    sinks,
		latest:   make(map[string]TodoSnapshot),
	}
}

// Run subscribes to both streams and reduces until ctx is cancelled.
// Events published before Run starts are not replayed; the registry
// entry for a dispatch is always re-emitted on its next transition, and
// session saves are frequent while an agent works, so a late-starting
// collector converges without a replay log.
func (c *TodoCollector) Run(ctx context.Context) {
	sessionCh := c.sessions.Subscribe(ctx)
	entryCh := c.ws.Subscribe(ctx)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sessionCh:
			if !ok {
				return
			}
			c.processSessionEvent(ev)
		case ev, ok := <-entryCh:
			if !ok {
				return
			}
			c.processEntryEvent(ev)
		}
	}
}

// Snapshot returns the current snapshot for the dispatched agent
// running on sessionID, if the registry knows it. It composes the
// latest registry entry with the last reduced session state, so a
// session reloaded mid-dispatch picks up live state immediately
// (#65's seed path).
func (c *TodoCollector) Snapshot(sessionID string) (TodoSnapshot, bool) {
	entry, ok := c.ws.BySession(sessionID)
	if !ok {
		return TodoSnapshot{}, false
	}
	return c.mergeEntry(entry), true
}

// processSessionEvent reduces one session event when it belongs to a
// dispatched session.
func (c *TodoCollector) processSessionEvent(ev pubsub.Event[session.Session]) {
	entry, ok := c.ws.BySession(ev.Payload.ID)
	if !ok {
		return
	}
	c.emit(Reduce(entry, ev.Payload))
}

// processEntryEvent re-emits the latest session state under a fresh
// registry entry, so lifecycle transitions reach the sinks even when no
// session save accompanies them.
func (c *TodoCollector) processEntryEvent(ev pubsub.Event[Entry]) {
	c.emit(c.mergeEntry(ev.Payload))
}

// mergeEntry replaces the registry entry of the latest snapshot for id
// (or seeds an empty one), keeping the session-derived fields.
func (c *TodoCollector) mergeEntry(entry Entry) TodoSnapshot {
	c.mu.Lock()
	prev, ok := c.latest[entry.ID]
	c.mu.Unlock()
	if !ok {
		return TodoSnapshot{Entry: entry}
	}
	prev.Entry = entry
	return prev
}

// emit stores the snapshot and delivers it to every sink.
func (c *TodoCollector) emit(snap TodoSnapshot) {
	c.mu.Lock()
	c.latest[snap.Entry.ID] = snap
	sinks := c.sinks
	c.mu.Unlock()
	for _, sink := range sinks {
		sink.DispatchTodos(snap)
	}
}

// Reduce builds the snapshot for one dispatched session: the registry
// entry by value, the in-progress todo as the current activity, todo
// counts, and the session's usage. It is pure, so the collector's tests
// and any later sink can pin the reduction independently.
func Reduce(entry Entry, sess session.Session) TodoSnapshot {
	snap := TodoSnapshot{
		Entry:            entry,
		PromptTokens:     sess.PromptTokens,
		CompletionTokens: sess.CompletionTokens,
		Cost:             sess.Cost,
	}
	for _, todo := range sess.Todos {
		snap.TodoTotal++
		if todo.Status == session.TodoStatusCompleted {
			snap.TodoCompleted++
		}
		if todo.Status == session.TodoStatusInProgress && snap.CurrentTodo == "" {
			snap.CurrentTodo = todo.Content
			if todo.ActiveForm != "" {
				snap.CurrentTodo = todo.ActiveForm
			}
		}
	}
	return snap
}
