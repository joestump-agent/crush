package dispatch

import (
	"context"
	"slices"
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
	// Todos is the full structured todo list — content, status, and
	// active form — so consumers can render a checklist without parsing
	// prose (#174 carries it in TaskStatusUpdateEvent metadata).
	Todos []session.Todo
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
	// sessionListeners holds the per-session snapshot subscriptions
	// (#174): each receives every snapshot reduced for its session,
	// until its context ends.
	sessionListeners map[string]map[chan TodoSnapshot]struct{}
}

// NewTodoCollector returns a collector reducing progress for the
// workspaces in ws, reading session state from sessions, and delivering
// snapshots to sinks. Call [TodoCollector.Start] to run it.
func NewTodoCollector(ws *Workspace, sessions pubsub.Subscriber[session.Session], sinks ...TodoSink) *TodoCollector {
	return &TodoCollector{
		ws:               ws,
		sessions:         sessions,
		sinks:            sinks,
		latest:           make(map[string]TodoSnapshot),
		sessionListeners: make(map[string]map[chan TodoSnapshot]struct{}),
	}
}

// Start subscribes to both streams and spawns the reduce loop. It
// returns once the subscriptions are live, so every event published
// after it returns is observed; events published before Start (e.g. the
// registry transitions of a dispatch already running) are not replayed
// — the registry re-emits an entry on its next transition, and session
// saves are frequent while an agent works, so the collector converges
// without a replay log.
func (c *TodoCollector) Start(ctx context.Context) {
	sessionCh := c.sessions.Subscribe(ctx)
	entryCh := c.ws.Subscribe(ctx)
	go c.loop(ctx, sessionCh, entryCh)
}

// loop reduces both streams until ctx is cancelled or a channel
// closes.
func (c *TodoCollector) loop(ctx context.Context, sessionCh <-chan pubsub.Event[session.Session], entryCh <-chan pubsub.Event[Entry]) {
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

// SubscribeSessionTodos returns a snapshot stream for one dispatched
// session (#174): every snapshot the collector reduces for sessionID,
// until ctx ends. It is the collector's second consumption surface
// after the sink fan-out — the A2A executor subscribes for the lifetime
// of a run — and it is deliberately per-session, so an in-flight
// consumer never sees another dispatch's progress. Delivery is buffered
// and lossy under back-pressure; the stream carries Working-state
// progress only, so a dropped snapshot is corrected by the next one.
// The channel closes when ctx is cancelled.
func (c *TodoCollector) SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan TodoSnapshot {
	ch := make(chan TodoSnapshot, 16)
	c.mu.Lock()
	if c.sessionListeners[sessionID] == nil {
		c.sessionListeners[sessionID] = make(map[chan TodoSnapshot]struct{})
	}
	c.sessionListeners[sessionID][ch] = struct{}{}
	c.mu.Unlock()

	go func() {
		<-ctx.Done()
		c.mu.Lock()
		delete(c.sessionListeners[sessionID], ch)
		if len(c.sessionListeners[sessionID]) == 0 {
			delete(c.sessionListeners, sessionID)
		}
		c.mu.Unlock()
		close(ch)
	}()

	return ch
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

// emit stores the snapshot, delivers it to every sink, and forwards it
// to every per-session listener for the snapshot's session.
func (c *TodoCollector) emit(snap TodoSnapshot) {
	c.mu.Lock()
	c.latest[snap.Entry.ID] = snap
	sinks := c.sinks
	listeners := c.listenersFor(snap.Entry.SessionID)
	c.mu.Unlock()
	for _, sink := range sinks {
		sink.DispatchTodos(snap)
	}
	for _, ch := range listeners {
		select {
		case ch <- snap:
		default:
			// Listener is slow — skip this snapshot; the next one
			// carries the newer state anyway.
		}
	}
}

// listenersFor returns the listener channels for sessionID. The caller
// must hold c.mu.
func (c *TodoCollector) listenersFor(sessionID string) []chan TodoSnapshot {
	subs := c.sessionListeners[sessionID]
	if len(subs) == 0 {
		return nil
	}
	listeners := make([]chan TodoSnapshot, 0, len(subs))
	for ch := range subs {
		listeners = append(listeners, ch)
	}
	return listeners
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
	snap.Todos = slices.Clone(sess.Todos)
	return snap
}
