package workspace

// The agent-task watcher (#421): one follower of an A2A host's agent index
// behind a workspace's agent surface. It keeps the descriptors the index
// streams, publishes each change to the TUI, and steers and cancels
// dispatched agents over A2A — with no dispatch registry on the path, so
// the same code serves a local host and, through the server's proxy, a
// remote one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// Reconnect backoff bounds for a dropped index stream.
const (
	agentWatchBackoffMin = 100 * time.Millisecond
	agentWatchBackoffMax = 5 * time.Second
)

// agentDialer reaches an agent index. ok is false while the index cannot
// be dialed yet; ready is closed once it may be.
type agentDialer func() (conn a2a.AgentIndexConn, ok bool, ready <-chan struct{})

// agentClient steers and cancels dispatched agents over A2A. The A2A
// host's own ServerFactory is one.
type agentClient interface {
	SteerDispatch(ctx context.Context, p agent.DispatchSteerParams) (agent.DispatchSteerOutcome, error)
	CancelDispatch(ctx context.Context, p agent.DispatchCancelParams) error
}

// agentTaskWatcher follows one agent index and serves the workspace's
// agent surface from what it has seen.
type agentTaskWatcher struct {
	dial   agentDialer
	client func() (agentClient, bool)

	mu    sync.Mutex
	tasks map[string]AgentTask // by dispatch id
	order []string
	send  func(tea.Msg)

	startOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

func newAgentTaskWatcher(dial agentDialer, client func() (agentClient, bool)) *agentTaskWatcher {
	return &agentTaskWatcher{
		dial:   dial,
		client: client,
		tasks:  make(map[string]AgentTask),
		done:   make(chan struct{}),
	}
}

// start begins following the index, once. Every later change goes to
// send, which may be nil: reads still work, and the TUI's Subscribe sets
// send when it arrives.
func (w *agentTaskWatcher) start(send func(tea.Msg)) {
	if send != nil {
		w.mu.Lock()
		w.send = send
		w.mu.Unlock()
	}
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		go func() {
			defer close(w.done)
			w.run(ctx)
		}()
	})
}

// stop ends the watcher and waits for it.
func (w *agentTaskWatcher) stop() {
	w.startOnce.Do(func() { close(w.done) })
	if w.cancel != nil {
		w.cancel()
	}
	<-w.done
}

// run follows the index until ctx ends: it waits for the index to be
// dialable, folds each stream into the cache, and reconnects with backoff
// when a stream drops. Every new stream opens with a full snapshot, so a
// drop loses nothing.
func (w *agentTaskWatcher) run(ctx context.Context) {
	backoff := agentWatchBackoffMin
	for {
		conn, ok, ready := w.dial()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-ready:
			}
			// ready can already be closed — a host that went down —
			// so a wait never becomes a spin.
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, agentWatchBackoffMax)
			continue
		}
		events, err := conn.WatchAgents(ctx)
		if err == nil {
			for ev := range events {
				w.apply(ev, true)
				backoff = agentWatchBackoffMin
			}
		} else if ctx.Err() == nil {
			slog.Debug("Agent index stream unavailable; retrying", "error", err)
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, agentWatchBackoffMax)
	}
}

// sleepCtx waits d, reporting false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// apply folds one index event into the cache and publishes every task it
// changed. A copy older than the one cached — a snapshot read while a
// newer change was already on the stream — is ignored, and a copy the
// cache already holds is not published again, so a reconnect's snapshot
// repeats no card update. A stream's snapshot is also authoritative:
// see below.
func (w *agentTaskWatcher) apply(ev a2a.AgentIndexEvent, fromStream bool) {
	descriptors := ev.Snapshot
	if ev.Upsert != nil {
		descriptors = []a2a.AgentDescriptor{*ev.Upsert}
	}
	w.mu.Lock()
	changed := make([]AgentTask, 0, len(descriptors))
	for _, d := range descriptors {
		cached, ok := w.tasks[d.ID]
		if ok && cached.descriptor.Revision >= d.Revision {
			continue
		}
		task := agentTaskFromDescriptor(d)
		if !ok {
			w.order = append(w.order, d.ID)
		}
		w.tasks[d.ID] = task
		changed = append(changed, task)
	}
	// A stream's snapshot is the whole index of the host that sent it,
	// and every later change follows it on the same stream. A live agent
	// it leaves out ran on a host that is gone — a restarted server's,
	// after a client/server reconnect — so it is marked unserved, which
	// shows as failed, rather than left running forever. Ended agents
	// stay, so their handles still resolve. The mark is an inference, not
	// a host's copy: its revision is cleared so any copy a host sends
	// replaces it.
	if fromStream && ev.Upsert == nil {
		listed := make(map[string]struct{}, len(ev.Snapshot))
		for _, d := range ev.Snapshot {
			listed[d.ID] = struct{}{}
		}
		for _, id := range w.order {
			task := w.tasks[id]
			if _, ok := listed[id]; ok || !task.Served || task.Terminal() {
				continue
			}
			d := task.descriptor
			d.Served = false
			d.Revision = 0
			task = agentTaskFromDescriptor(d)
			w.tasks[id] = task
			changed = append(changed, task)
		}
	}
	send := w.send
	w.mu.Unlock()
	if send == nil {
		return
	}
	for _, task := range changed {
		send(pubsub.Event[AgentTask]{Type: pubsub.UpdatedEvent, Payload: task})
	}
}

// list returns the tasks dispatched from sessionID (#399), oldest first.
func (w *agentTaskWatcher) list(sessionID string) []AgentTask {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []AgentTask
	for _, id := range w.order {
		if task := w.tasks[id]; task.ParentSessionID == sessionID {
			out = append(out, task)
		}
	}
	return out
}

// bySession returns the task whose child session is sessionID.
func (w *agentTaskWatcher) bySession(sessionID string) (AgentTask, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range w.order {
		if task := w.tasks[id]; task.SessionID == sessionID {
			return task, true
		}
	}
	return AgentTask{}, false
}

// byHandle returns the latest task dispatched from sessionID under handle
// (#313, #399). Without a session nothing is in scope.
func (w *agentTaskWatcher) byHandle(sessionID, handle string) (AgentTask, bool) {
	if sessionID == "" {
		return AgentTask{}, false
	}
	handle = dispatch.HandleSlug(handle)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range slices.Backward(w.order) {
		task := w.tasks[id]
		if task.ParentSessionID == sessionID && dispatch.HandleSlug(task.Handle) == handle {
			return task, true
		}
	}
	return AgentTask{}, false
}

// byRef resolves a cancel reference (#373) the way the coordinator does:
// a dispatch id, then a child session id, then an @handle (the latest
// dispatch carrying it).
func (w *agentTaskWatcher) byRef(ref string) (AgentTask, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if task, ok := w.tasks[ref]; ok {
		return task, true
	}
	for _, id := range w.order {
		if task := w.tasks[id]; task.SessionID == ref {
			return task, true
		}
	}
	slug := dispatch.HandleSlug(ref)
	for _, id := range slices.Backward(w.order) {
		if task := w.tasks[id]; dispatch.HandleSlug(task.Handle) == slug {
			return task, true
		}
	}
	return AgentTask{}, false
}

// refresh reads the index once into the cache: the fallback for a lookup
// that misses before the stream has delivered its snapshot.
func (w *agentTaskWatcher) refresh(ctx context.Context) {
	conn, ok, _ := w.dial()
	if !ok {
		return
	}
	descriptors, err := conn.ListAgents(ctx)
	if err != nil {
		slog.Debug("Agent index read failed", "error", err)
		return
	}
	// A read can race the stream, so it only adds what it saw.
	w.apply(a2a.AgentIndexEvent{Snapshot: descriptors}, false)
}

// steer sends text to the running agent dispatched from sessionID under
// handle (#313), over A2A (#351). It refuses — with the coordinator's
// wording — an unknown handle, another session's agent (#399), and a
// finished agent.
func (w *agentTaskWatcher) steer(ctx context.Context, sessionID, handle, text string, attachments []message.Attachment) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("message text is required")
	}
	handle = dispatch.HandleSlug(handle)
	task, ok := w.byHandle(sessionID, handle)
	if !ok {
		w.refresh(ctx)
		task, ok = w.byHandle(sessionID, handle)
	}
	if !ok {
		return fmt.Errorf("no agent with handle @%s in this session; dispatch one first", handle)
	}
	if task.Terminal() {
		return fmt.Errorf("agent @%s finished (%s); task sessions are never continuable — dispatch a new agent instead", handle, task.Status)
	}
	d := task.descriptor
	if !task.Served {
		return fmt.Errorf("agent %s is no longer running; dispatch a new agent instead", task.SessionID)
	}
	// Until the executor has named the dispatch's own task, the first
	// message on its context would start the dispatch's turn rather than
	// steer it (#351): refuse, as the coordinator does before its server
	// is up.
	if d.TaskID == "" {
		return fmt.Errorf("no running agent for session %s; dispatch one first", task.SessionID)
	}
	client, ok := w.client()
	if !ok {
		return errors.New("dispatched agents are not reachable yet")
	}
	var references []string
	if d.TaskID != "" {
		references = []string{d.TaskID}
	}
	outcome, err := client.SteerDispatch(ctx, agent.DispatchSteerParams{
		Endpoint:         d.Endpoint,
		Card:             d.Card,
		ContextID:        d.ContextID,
		Text:             text,
		Attachments:      attachments,
		ReferenceTaskIDs: references,
	})
	if err != nil {
		return err
	}
	return agent.SteerOutcomeError(task.SessionID, outcome)
}

// cancel stops one dispatched agent (#373) with the user's cancel reason,
// through tasks/cancel (#348).
func (w *agentTaskWatcher) cancelTask(ctx context.Context, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("a dispatch reference is required")
	}
	task, ok := w.byRef(ref)
	if !ok {
		w.refresh(ctx)
		task, ok = w.byRef(ref)
	}
	if !ok {
		return fmt.Errorf("no dispatch %q is known; dispatch one first", ref)
	}
	if task.Terminal() || !task.Served {
		return fmt.Errorf("dispatch %s already finished (%s); task sessions are never continuable — dispatch a new agent instead", task.DispatchID, task.Status)
	}
	d := task.descriptor
	if d.TaskID == "" {
		return fmt.Errorf("dispatch %s has not started yet", task.DispatchID)
	}
	client, ok := w.client()
	if !ok {
		return errors.New("dispatched agents are not reachable yet")
	}
	return client.CancelDispatch(ctx, agent.DispatchCancelParams{
		Endpoint: d.Endpoint,
		Card:     d.Card,
		TaskID:   d.TaskID,
		Reason:   dispatch.ReasonCanceled,
	})
}
