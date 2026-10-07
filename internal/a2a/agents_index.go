package a2a

// The host's agent index (#421): one descriptor per listed dispatch, kept
// current from the dispatch's own task lifecycle and served next to the
// per-dispatch routes. It is how a UI meets dispatched agents over the
// wire instead of reading the process's dispatch registry: a snapshot,
// then one event per change on a stream, never a poll.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/charmbracelet/crush/internal/agent"
)

// AgentsIndexPath is the path the host serves its agent index at. The
// per-dispatch routes live under [agentsPathPrefix] beside it.
const AgentsIndexPath = "/agents"

// indexEventBuffer is how many index events a stream subscriber may fall
// behind by. A subscriber that falls further behind loses its stream and
// reconnects; the new stream starts from a fresh snapshot, so nothing is
// lost but the stale events in between.
const indexEventBuffer = 256

// indexKeepAlive is how often an idle index stream carries an SSE
// comment, so a proxy between it and its watcher never times it out.
const indexKeepAlive = 30 * time.Second

// indexWriteTimeout bounds one write to an index stream: a watcher that
// stops reading loses its stream instead of holding the host's shutdown.
const indexWriteTimeout = 10 * time.Second

// maxFinishedDescriptors bounds how many ended dispatches the index keeps
// (#421). The oldest ended one goes first; a finished dispatch's durable
// record lives in its session (#410), so the index only needs recent
// ones for @handle resolution and card state.
const maxFinishedDescriptors = 500

// AgentDescriptor is one listed dispatch on the host's agent index
// (#421): what a UI needs to render the dispatch's card, resolve its
// @handle, and steer or cancel it over A2A — without the dispatch
// registry. State follows the dispatch's own task only; the tasks that
// steers open on the same context never move it.
type AgentDescriptor struct {
	// ID is the dispatch's registry id; the host routes
	// /agents/<ID> to it.
	ID string `json:"id"`
	// Endpoint is the dispatch's card URL, and Card the served card the
	// A2A client dials it through.
	Endpoint string             `json:"endpoint"`
	Card     *a2aspec.AgentCard `json:"card,omitempty"`
	// Handle and Role are the dispatch's @handle and one-line role: the
	// card's name and description.
	Handle string `json:"handle"`
	Role   string `json:"role,omitempty"`
	// ContextID is the dispatch's A2A context, its task session (#350):
	// a steer addresses it. ParentSessionID is the session the dispatch
	// was created from (#399).
	ContextID       string `json:"context_id"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// TaskID is the dispatch's own task, empty until the dispatch's
	// first message creates it.
	TaskID string `json:"task_id,omitempty"`
	// State is the task's state as a dispatch status token
	// (DispatchStatusWorking and its siblings); empty before the task
	// exists. StatusText is the latest status message's text.
	State      string `json:"state,omitempty"`
	StatusText string `json:"status_text,omitempty"`
	// Progress is the latest todos/v1 value the task carried, and Usage
	// the latest usage/v1 value: live while the agent works, final once
	// the task ends.
	Progress *agent.TodoProgress `json:"progress,omitempty"`
	Usage    *agent.Usage        `json:"usage,omitempty"`
	// Served is true while the dispatch's route is up. A finished
	// dispatch stays on the index with Served false, so its @handle keeps
	// resolving and its card keeps its last state.
	Served bool `json:"served"`
	// StartedAt is when the route came up, UpdatedAt the descriptor's
	// last change, and FinishedAt when the task reached a terminal state.
	StartedAt  time.Time  `json:"started_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Revision increases with every change to any descriptor on the
	// index, so a watcher keeps the newer of two copies of one dispatch
	// and publishes only what changed. It starts from the host's start
	// time, so a restarted host's revisions are newer still.
	Revision uint64 `json:"revision"`
}

// meaningfulEqual reports whether two copies of a descriptor differ only
// in their bookkeeping: the update time and the revision.
func meaningfulEqual(a, b AgentDescriptor) bool {
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	a.Revision, b.Revision = 0, 0
	return reflect.DeepEqual(a, b)
}

// Terminal reports whether the descriptor's task has ended.
func (d AgentDescriptor) Terminal() bool {
	switch d.State {
	case DispatchStatusCompleted, DispatchStatusFailed, DispatchStatusCanceled:
		return true
	default:
		return false
	}
}

// AgentIndexEvent is one event on the index stream: a full Snapshot when
// the stream opens, then one Upsert per descriptor change.
type AgentIndexEvent struct {
	Snapshot []AgentDescriptor `json:"snapshot,omitempty"`
	Upsert   *AgentDescriptor  `json:"upsert,omitempty"`
}

// agentIndex is the factory's index state: the descriptors by dispatch
// id in listing order, and the open streams.
type agentIndex struct {
	mu     sync.Mutex
	byID   map[string]*AgentDescriptor
	order  []string
	subs   map[chan AgentIndexEvent]struct{}
	rev    uint64
	closed bool
	now    func() time.Time
}

func newAgentIndex() *agentIndex {
	return &agentIndex{
		byID: make(map[string]*AgentDescriptor),
		subs: make(map[chan AgentIndexEvent]struct{}),
		rev:  uint64(time.Now().UnixNano()),
		now:  time.Now,
	}
}

// add lists a dispatch whose route just came up.
func (x *agentIndex) add(d AgentDescriptor) {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	d.Served = true
	d.StartedAt = now
	if _, ok := x.byID[d.ID]; !ok {
		x.order = append(x.order, d.ID)
	}
	x.byID[d.ID] = &d
	x.changedLocked(&d)
}

// setTask records the task that starts the dispatch's own turn (#421):
// the executor names it, so the descriptor follows it and never a task a
// steer opened first.
func (x *agentIndex) setTask(id, taskID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	d, ok := x.byID[id]
	if !ok || d.TaskID != "" {
		return
	}
	d.TaskID = taskID
	x.changedLocked(d)
}

// unserve marks a dispatch whose route went down. The descriptor stays,
// up to maxFinishedDescriptors ended ones.
func (x *agentIndex) unserve(id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	d, ok := x.byID[id]
	if !ok || !d.Served {
		return
	}
	d.Served = false
	x.changedLocked(d)
	x.evictLocked()
}

// evictLocked drops the oldest ended dispatches beyond
// maxFinishedDescriptors.
func (x *agentIndex) evictLocked() {
	ended := 0
	for _, id := range x.order {
		if !x.byID[id].Served {
			ended++
		}
	}
	for i := 0; ended > maxFinishedDescriptors && i < len(x.order); {
		id := x.order[i]
		if x.byID[id].Served {
			i++
			continue
		}
		delete(x.byID, id)
		x.order = slices.Delete(x.order, i, i+1)
		ended--
	}
}

// track folds a write to a listed route's task store into its descriptor.
// Only the dispatch's own task moves it — the one the executor named
// through setTask, on the dispatch's context. Writes to any other task,
// a steer's above all, are ignored. ev is the event behind an update,
// whose metadata carries the todos/v1 and usage/v1 values. A write that
// changes nothing a reader sees publishes nothing.
func (x *agentIndex) track(id string, task *a2aspec.Task, ev a2aspec.Event) {
	if task == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	d, ok := x.byID[id]
	if !ok || d.TaskID == "" || d.TaskID != string(task.ID) || string(task.ContextID) != d.ContextID {
		return
	}
	before := *d
	d.State = dispatchTaskStateToken(task.Status.State)
	if task.Status.Message != nil {
		d.StatusText = messageText(task.Status.Message)
	}
	if sue, ok := ev.(*a2aspec.TaskStatusUpdateEvent); ok {
		foldIndexMetadata(d, sue.Meta())
	}
	if d.Terminal() && d.FinishedAt == nil {
		finished := x.now()
		d.FinishedAt = &finished
	}
	if meaningfulEqual(before, *d) {
		return
	}
	x.changedLocked(d)
}

// changedLocked stamps a changed descriptor's time and revision and
// publishes it.
func (x *agentIndex) changedLocked(d *AgentDescriptor) {
	x.rev++
	d.Revision = x.rev
	d.UpdatedAt = x.now()
	x.publishLocked(*d)
}

// foldIndexMetadata decodes the todos/v1 and usage/v1 values on a status
// update into the descriptor. A value that does not decode is dropped.
func foldIndexMetadata(d *AgentDescriptor, meta map[string]any) {
	if raw, ok := meta[TodoExt.URI]; ok {
		if decoded, err := DecodeValue(TodoExt, raw); err == nil {
			if progress, ok := decoded.(*agent.TodoProgress); ok {
				d.Progress = progress
			}
		}
	}
	if raw, ok := meta[UsageExt.URI]; ok {
		if decoded, err := DecodeValue(UsageExt, raw); err == nil {
			if usage, ok := decoded.(*agent.Usage); ok {
				d.Usage = usage
			}
		}
	}
}

// snapshot returns the descriptors in listing order.
func (x *agentIndex) snapshot() []AgentDescriptor {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.snapshotLocked()
}

func (x *agentIndex) snapshotLocked() []AgentDescriptor {
	out := make([]AgentDescriptor, 0, len(x.order))
	for _, id := range x.order {
		out = append(out, *x.byID[id])
	}
	return out
}

// subscribe opens a stream whose first event is the snapshot. The
// returned func closes it. Once the index has closed, the stream is
// already closed: a request that raced the host's shutdown ends at once.
func (x *agentIndex) subscribe() (<-chan AgentIndexEvent, func()) {
	x.mu.Lock()
	defer x.mu.Unlock()
	ch := make(chan AgentIndexEvent, indexEventBuffer)
	if x.closed {
		close(ch)
		return ch, func() {}
	}
	ch <- AgentIndexEvent{Snapshot: x.snapshotLocked()}
	x.subs[ch] = struct{}{}
	return ch, func() {
		x.mu.Lock()
		defer x.mu.Unlock()
		if _, ok := x.subs[ch]; ok {
			delete(x.subs, ch)
			close(ch)
		}
	}
}

// publishLocked sends a descriptor change to every stream. A stream that
// has fallen indexEventBuffer events behind is closed rather than
// blocking the task it tracks; its watcher reconnects to a fresh
// snapshot.
func (x *agentIndex) publishLocked(d AgentDescriptor) {
	for ch := range x.subs {
		select {
		case ch <- AgentIndexEvent{Upsert: &d}:
		default:
			delete(x.subs, ch)
			close(ch)
		}
	}
}

// closeAll ends every open stream, and every later one, when the host
// closes.
func (x *agentIndex) closeAll() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.closed = true
	for ch := range x.subs {
		delete(x.subs, ch)
		close(ch)
	}
}

// trackingStore decorates a listed route's task store (#421): every task
// the served executor creates or updates also lands on the route's index
// descriptor. The SDK still talks to the store it would have used.
type trackingStore struct {
	taskstore.Store
	index *agentIndex
	id    string
}

// Create implements [taskstore.Store].
func (s *trackingStore) Create(ctx context.Context, task *a2aspec.Task) (taskstore.TaskVersion, error) {
	version, err := s.Store.Create(ctx, task)
	if err == nil {
		s.index.track(s.id, task, nil)
	}
	return version, err
}

// Update implements [taskstore.Store].
func (s *trackingStore) Update(ctx context.Context, update *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	version, err := s.Store.Update(ctx, update)
	if err == nil && update != nil {
		s.index.track(s.id, update.Task, update.Event)
	}
	return version, err
}

// serveAgentsIndex answers GET /agents: the index as a JSON array, or —
// when the request accepts text/event-stream — a stream that opens with
// the snapshot and then carries one event per descriptor change. The
// index is the host's own surface, so it takes the host's checks: no
// Origin, the host's Host, and the host's bearer token from the host's
// own user.
func (f *ServerFactory) serveAgentsIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "a2a: the agent index answers GET", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "a2a: cross-origin requests are not accepted", http.StatusForbidden)
		return
	}
	if r.Host != a2aURLHost {
		http.Error(w, "a2a: unexpected Host", http.StatusBadRequest)
		return
	}
	decision := authDecision{
		token:         f.authToken(),
		wantUID:       strconv.Itoa(os.Getuid()),
		authorization: r.Header.Values(bearerAuthorizationHeader),
	}
	if peerUID, ok := peerUIDFromContext(r.Context()); ok {
		decision.peerKnown = true
		decision.peerUID = peerUID
	}
	if decision.authorize() != nil {
		http.Error(w, "a2a: unauthenticated", http.StatusUnauthorized)
		return
	}

	if !AcceptsEventStream(r) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.index.snapshot())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "a2a: streaming is not supported", http.StatusInternalServerError)
		return
	}
	events, unsubscribe := f.index.subscribe()
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Every write gets a deadline: a watcher that stops reading loses
	// its stream instead of parking this handler, and the host's
	// shutdown with it.
	rc := http.NewResponseController(w)
	write := func(format string, args ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(indexWriteTimeout))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	keepAlive := time.NewTicker(indexKeepAlive)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if !write(": keep-alive\n\n") {
				return
			}
		case ev, open := <-events:
			if !open {
				// The host closed, or this stream fell too far behind;
				// the watcher reconnects to a fresh snapshot.
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				slog.Warn("A2A agent index event failed to encode", "err", err)
				continue
			}
			if !write("data: %s\n\n", data) {
				return
			}
		}
	}
}

// AcceptsEventStream reports whether any of r's Accept headers asks for
// an SSE stream.
func AcceptsEventStream(r *http.Request) bool {
	return slices.ContainsFunc(r.Header.Values("Accept"), func(v string) bool {
		return strings.Contains(v, "text/event-stream")
	})
}
