// Package dispatch provides the agent registry behind worktree
// dispatch (Worktree Dispatch epic, #61) and the workspace lifecycle
// the dispatched agents run in, split in two:
//
// The registry ([AgentRegistry]) is the in-memory store of dispatch
// entries and their event stream. It owns lifecycle state, sessions,
// handles, endpoints, and results, and publishes every mutation as an
// entry event. It knows nothing about filesystems or SCM.
//
// The workspace lifecycle lives behind the [WorkspaceProvider]
// interface: the git worktree provider (#63) provisions, diffs, and
// cleans up the directories dispatched agents run in, and
// [NoneProvider] serves agents that need no workspace at all (#433
// will select per agent). Callers only ever see directories, branches,
// and diffs, so a jj, sapling, or plain-copy backend can implement the
// interface without touching the registry or its consumers.
//
// The two compose: a caller provisions with a provider, builds an
// [Entry] from the placement, and registers it in the registry. Phase 1
// is in-process: the registry lives in memory on the parent
// coordinator, which owns its lifetime. When dispatch crosses a process
// or network boundary (#72/#73), the same registry entries keep
// pointing at workspaces served remotely.
//
// The registry is deliberately the only one: A2A discovery (#70) reads
// its entries rather than keeping an endpoint map of its own, and the
// @handle addressing layer (#313) uses it as the handle registry. The
// AgentCard field is opaque to this package so it can stay
// import-cycle-free of internal/a2a.
package dispatch

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
)

// IsTerminal reports whether status ends the dispatch: a terminal
// dispatch's agent is gone, its handle answers with a refusal when
// addressed, and it is excluded from the live-agents surfaces.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusKilled:
		return true
	default:
		return false
	}
}

// MaxHandleLength caps a handle slug in bytes (#399), so a long role
// can never balloon the @ completions or a prompt line. A suffix is
// included in the cap: the base truncates so "-2" still fits.
const MaxHandleLength = 32

// ReservedHandles are the names a dispatch may never claim outright:
// the built-in agent ids, plus "all". A request for one is suffixed
// like a collision (#399): task, task-2.
var ReservedHandles = map[string]bool{
	"coder":  true,
	"plan":   true,
	"task":   true,
	"worker": true,
	"all":    true,
}

// Status is the lifecycle state of a dispatched workspace.
type Status string

const (
	// StatusProvisioned means the workspace exists but no agent has
	// started running in it yet.
	StatusProvisioned Status = "provisioned"
	// StatusRunning means a dispatched agent is working in the
	// workspace.
	StatusRunning Status = "running"
	// StatusCompleted means the dispatched agent finished normally.
	StatusCompleted Status = "completed"
	// StatusFailed means the dispatched agent's run failed.
	StatusFailed Status = "failed"
	// StatusKilled means the run was canceled deterministically
	// (wander kill, #316).
	StatusKilled Status = "killed"
)

// Entry is one dispatched agent in the registry. Entries are handed
// out by value; mutations go through the registry's setter methods so
// the registry never hands out a shared pointer. The workspace fields
// (Path, Branch, Base, BaseSHA) are stamped from the placement a
// [WorkspaceProvider] returned and stay empty for an agent served by
// [NoneProvider].
type Entry struct {
	// ID is the dispatch identifier, a UUID. The branch and directory
	// name are BranchPrefix + ID.
	ID string
	// Path is the absolute workspace directory.
	Path string
	// Branch is the workspace branch, BranchPrefix + ID.
	Branch string
	// Base is the base the workspace was cut from: a branch, commit, or
	// other revision as given at provision time.
	Base string
	// BaseSHA is the commit Base resolved to when the workspace was
	// provisioned. Diff always diffs against it.
	BaseSHA string
	// SessionID is the ephemeral session backing the dispatched agent
	// (#48/#50); empty until the dispatch starts running.
	SessionID string
	// ParentSessionID is the session the dispatch was created from
	// (#399): the scope handle and session-ID addressing are validated
	// against, so only the dispatching session can address the agent.
	// Empty until the dispatch tool records it.
	ParentSessionID string
	// Handle is the workspace's @handle for agent addressing (#313);
	// empty until one is assigned by [AgentRegistry.AssignHandle].
	Handle string
	// Role is the one-line role label the dispatch was given (#313) —
	// the "what it is" shown next to the handle in the @ completions,
	// and the fallback a handle is derived from when the model did not
	// supply one.
	Role string
	// Status is the workspace's lifecycle state.
	Status Status
	// Endpoint is the A2A server endpoint serving the dispatched agent
	// (#70); empty in Phase 1's in-process dispatch.
	Endpoint string
	// AgentCard is the dispatched agent's A2A AgentCard (#70). Opaque to
	// this package to keep it import-cycle-free of internal/a2a.
	AgentCard any
	// TaskID is the A2A task ID the served dispatch's first stream event
	// named (#349); empty until then. It survives a dropped stream: with
	// it, the run is recoverable through tasks/resubscribe and tasks/get
	// and answerable through queries after the fact.
	TaskID string
	// StartedAt is when the dispatch's agent started running; zero
	// until then. FinishedAt is when it reached a terminal state. The
	// agent block (#65) renders elapsed time from the pair.
	StartedAt  time.Time
	FinishedAt time.Time
	// Result is the terminal DispatchResult (#66) recorded when the run
	// finished; nil until then. It is written once and read-only after,
	// so sharing the pointer between entry copies is safe. The
	// completed agent block (#65) renders its durable record — the
	// findings summary and diff stat — from it.
	Result *DispatchResult
}

// clone returns a copy of the entry.
func (e Entry) clone() Entry { return e }

// AgentRegistry is the in-memory registry of dispatch entries and
// their event stream. It is the single source of dispatch lifecycle
// state: the todo collector (#65), the @handle addressing layer
// (#313), and A2A discovery (#70) all read it, and it deliberately
// keeps no SCM or filesystem state of its own. All methods are safe
// for concurrent use.
type AgentRegistry struct {
	// events re-publishes every registry mutation as an entry event, so
	// the todo collector (#65) and later sinks observe lifecycle
	// transitions without the mutation call sites knowing about them.
	events *pubsub.Broker[Entry]

	mu      sync.Mutex
	entries map[string]Entry
}

// NewAgentRegistry returns an empty registry.
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{
		events:  pubsub.NewBroker[Entry](),
		entries: make(map[string]Entry),
	}
}

// Register adds e to the registry, replacing any entry already
// carrying its ID. It publishes no event: the entry is visible to Get,
// List, and the By* lookups immediately, and the first Update
// publishes the entry event.
func (r *AgentRegistry) Register(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[e.ID] = e
}

// Remove drops the entry for id from the registry and reports whether
// it was present. It publishes no event and touches no workspace: the
// provider's [WorkspaceProvider.Release] tears the directory down.
func (r *AgentRegistry) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[id]
	delete(r.entries, id)
	return ok
}

// Get returns a copy of the entry for id.
func (r *AgentRegistry) Get(id string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	return e.clone(), ok
}

// ByHandle returns the entry currently carrying handle, if any. A
// non-terminal entry always wins: handles live as long as their run
// (#399). When no live entry carries the handle, the most recently
// finished one answers (max FinishedAt), so a mention of a finished
// @handle still renders its read-only card until the handle is reused.
func (r *AgentRegistry) ByHandle(handle string) (Entry, bool) {
	if handle == "" {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var finished Entry
	haveFinished := false
	for _, e := range r.entries {
		if e.Handle != handle {
			continue
		}
		if !e.Status.IsTerminal() {
			return e.clone(), true
		}
		if !haveFinished || e.FinishedAt.After(finished.FinishedAt) {
			finished = e
			haveFinished = true
		}
	}
	if haveFinished {
		return finished.clone(), true
	}
	return Entry{}, false
}

// BySession returns the entry whose dispatched agent runs on sessionID,
// if any.
func (r *AgentRegistry) BySession(sessionID string) (Entry, bool) {
	if sessionID == "" {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.SessionID == sessionID {
			return e.clone(), true
		}
	}
	return Entry{}, false
}

// List returns copies of every entry, ordered by ID.
func (r *AgentRegistry) List() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, e.clone())
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		return strings.Compare(a.ID, b.ID)
	})
	return entries
}

// Update applies fn to the entry for id under the registry lock and
// reports whether the entry exists. Later phases use it for the fields
// they own: #64 stamps the session and status, #65 the timestamps and
// terminal result, #313 the handle, #70 the endpoint and card. Every
// successful mutation is published as an UpdatedEvent on the entry
// stream ([AgentRegistry.Subscribe]) so the todo collector can
// re-emit.
func (r *AgentRegistry) Update(id string, fn func(*Entry)) bool {
	if fn == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return false
	}
	fn(&e)
	r.entries[id] = e
	r.events.Publish(pubsub.UpdatedEvent, e)
	return true
}

// SetStatus updates the entry's lifecycle state, stamping the run's
// time bounds as it goes: StartedAt on the first transition to
// running, FinishedAt on the first terminal state. Both are stamped at
// most once, so re-setting the same state does not restart the clock.
func (r *AgentRegistry) SetStatus(id string, status Status) bool {
	return r.Update(id, func(e *Entry) {
		e.Status = status
		switch status {
		case StatusRunning:
			if e.StartedAt.IsZero() {
				e.StartedAt = time.Now()
			}
		case StatusCompleted, StatusFailed, StatusKilled:
			if e.FinishedAt.IsZero() {
				e.FinishedAt = time.Now()
			}
		}
	})
}

// SetResult records the terminal DispatchResult (#66) on the entry, so
// the completed agent block (#65) can render the durable record from
// the registry rather than from a transient event. Record it before the
// terminal SetStatus so the terminal entry event carries it.
func (r *AgentRegistry) SetResult(id string, result DispatchResult) bool {
	return r.Update(id, func(e *Entry) {
		c := result
		e.Result = &c
	})
}

// SetSession records the ephemeral session backing the dispatched agent.
func (r *AgentRegistry) SetSession(id, sessionID string) bool {
	return r.Update(id, func(e *Entry) { e.SessionID = sessionID })
}

// SetParentSessionID records the session the dispatch was created from
// (#399): the scope handle and session-ID addressing are validated
// against, so another session cannot address this dispatch.
func (r *AgentRegistry) SetParentSessionID(id, parentSessionID string) bool {
	return r.Update(id, func(e *Entry) { e.ParentSessionID = parentSessionID })
}

// SetTaskID records the A2A task ID serving the dispatched agent (#349),
// reported by the transport from the stream's first event onward.
func (r *AgentRegistry) SetTaskID(id, taskID string) bool {
	return r.Update(id, func(e *Entry) { e.TaskID = taskID })
}

// SetHandle records the @handle the dispatched agent is addressable by.
func (r *AgentRegistry) SetHandle(id, handle string) bool {
	return r.Update(id, func(e *Entry) { e.Handle = handle })
}

// AssignHandle assigns the entry its @handle and role (#313) and returns
// the assigned handle. requested is the handle the model asked for (the
// leading "@" and any surrounding space are tolerated); role is the
// one-line role label recorded alongside it. When requested is empty the
// handle is derived from the role, and when that is empty too it falls
// back to "agent". A handle lives as long as its run (#399): a handle
// that collides with one claimed by a non-terminal dispatch — or with a
// name in [ReservedHandles] — is suffixed numerically: tester, tester-2,
// tester-3. A finished dispatch's handle is free for reuse, and the
// suffixing truncates the base so the result never exceeds
// [MaxHandleLength]. The assignment is atomic under the registry
// lock, so two concurrent dispatches asking for the same handle get
// distinct suffixed ones, and it publishes the usual entry event so the
// dispatch block and @ completions observe the handle immediately.
func (r *AgentRegistry) AssignHandle(id, requested, role string) (string, bool) {
	handle := HandleSlug(requested)
	if handle == "" {
		handle = HandleSlug(role)
	}
	if handle == "" {
		handle = "agent"
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return "", false
	}
	handle = uniqueHandle(r.entries, handle)
	e.Handle = handle
	e.Role = strings.TrimSpace(role)
	r.entries[id] = e
	r.events.Publish(pubsub.UpdatedEvent, e)
	return handle, true
}

// uniqueHandle returns handle, or its first free numeric suffix, against
// every handle claimed by a non-terminal entry and every reserved name
// (#399): a finished dispatch releases its handle for reuse. Suffixing
// truncates the base so the result fits MaxHandleLength. Callers must
// hold r.mu.
func uniqueHandle(entries map[string]Entry, handle string) string {
	taken := make(map[string]bool, len(entries)+len(ReservedHandles))
	for name := range ReservedHandles {
		taken[name] = true
	}
	for _, e := range entries {
		if e.Handle != "" && !e.Status.IsTerminal() {
			taken[e.Handle] = true
		}
	}
	if !taken[handle] {
		return handle
	}
	for n := 2; ; n++ {
		candidate := suffixedHandle(handle, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// suffixedHandle appends -n to base, truncating the base (and any
// trailing dash the truncation leaves) so the result fits
// MaxHandleLength.
func suffixedHandle(base string, n int) string {
	suffix := fmt.Sprintf("-%d", n)
	if len(base)+len(suffix) > MaxHandleLength {
		base = strings.TrimRight(base[:MaxHandleLength-len(suffix)], "-")
	}
	return base + suffix
}

// HandleSlug normalizes a candidate handle or role into handle form:
// lowercase, runs of non-alphanumerics collapsed to single dashes, no
// leading or trailing dash, and capped at MaxHandleLength bytes (#399) —
// a trailing dash the cap leaves is trimmed. "@Team Lead" becomes
// "team-lead"; a candidate that slugs to nothing ("@__"), or an empty
// one, stays empty so the caller can fall through to its next fallback.
func HandleSlug(candidate string) string {
	candidate = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(candidate), "@"))
	candidate = strings.ToLower(candidate)
	var b strings.Builder
	dash := false
	for _, r := range candidate {
		if b.Len() >= MaxHandleLength {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// SetEndpoint records the A2A endpoint and AgentCard serving the
// dispatched agent (#70).
func (r *AgentRegistry) SetEndpoint(id, endpoint string, card any) bool {
	return r.Update(id, func(e *Entry) {
		e.Endpoint = endpoint
		e.AgentCard = card
	})
}

// Subscribe returns the registry's entry event stream: one
// UpdatedEvent per successful mutation, carrying the entry by value.
// The todo collector (#65) is its first subscriber; anything else that
// needs dispatch lifecycle state (#174's A2A bridge) reads the same
// stream rather than polling the registry.
func (r *AgentRegistry) Subscribe(ctx context.Context) <-chan pubsub.Event[Entry] {
	return r.events.Subscribe(ctx)
}
