package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a dispatched workspace. It doubles as
// the source for the A2A task state a dispatch reports (#70/#174): the
// registry is the one place both the local dashboard and the protocol
// layer read a dispatch's progress from.
type Status string

const (
	// StatusPending is a provisioned workspace whose agent has not yet
	// started a turn.
	StatusPending Status = "pending"
	// StatusRunning is a workspace whose dispatched agent is working.
	StatusRunning Status = "running"
	// StatusComplete is a workspace whose agent finished successfully.
	StatusComplete Status = "complete"
	// StatusFailed is a workspace whose agent errored or never ran.
	StatusFailed Status = "failed"
)

// Workspace is one isolated workspace provisioned for a dispatched
// agent. It is the registry's tracked unit: the on-disk scratch
// directory plus the dispatch coordinates every phase keys off — the
// backing session, the A2A endpoint/card once served (#70), and the
// current status.
//
// Values handed back by the [Registry] are snapshots; mutate a
// workspace only through the registry's setters so concurrent
// dispatches never race on the same entry.
type Workspace struct {
	// ID is the dispatch identifier: unique per provisioned workspace
	// and the key the registry tracks it under.
	ID string
	// Path is the absolute path to the workspace's working directory.
	Path string
	// Base is the resolved, stable identifier of the ref the workspace
	// was forked from (a commit SHA for the git backend). Diffs are
	// taken against this, so they stay correct after the agent commits.
	Base string
	// Branch is the SCM-specific branch/ref created for the workspace
	// (empty for backends without a branch concept).
	Branch string
	// SessionID is the (ephemeral) session backing the dispatched agent.
	// Empty until the dispatch tool binds one.
	SessionID string
	// Endpoint is the A2A endpoint the dispatch is served at (#70).
	// Empty for in-process dispatch.
	Endpoint string
	// Card carries the dispatch's A2A agent card once served (#70). It
	// is typed as any so this package stays a leaf dependency free of
	// the A2A SDK; #70 stores an *a2a.AgentCard.
	Card any
	// Status is the dispatch's lifecycle state.
	Status Status
}

// Provisioned is what a [Backend] returns after creating a workspace.
type Provisioned struct {
	// Path is the workspace's working directory.
	Path string
	// Branch is the SCM branch/ref created for it, if any.
	Branch string
	// Base is the resolved, stable identifier of the fork point — a
	// concrete commit rather than a moving ref, so later diffs against
	// it survive commits the dispatched agent makes.
	Base string
}

// Backend is the SCM-specific machinery behind a workspace: it forks an
// isolated working directory, diffs its work product against the fork
// point, and tears it down. Implementations must be safe for concurrent
// use across distinct workspaces. Defined here, in the consuming
// package, per the project's interface convention.
type Backend interface {
	// Create provisions an isolated workspace at dest, forked from base
	// (empty means the backend's current head). id is the dispatch id,
	// available for naming a branch. It returns the resolved fork point
	// so diffs stay stable across the agent's own commits.
	Create(ctx context.Context, dest, id, base string) (Provisioned, error)
	// Diff returns the work product of the workspace at path: everything
	// from base to the current working tree — committed, staged,
	// unstaged, and untracked — as one unified diff.
	Diff(ctx context.Context, path, base string) (string, error)
	// Remove tears down the workspace at path along with branch. It is
	// idempotent: removing an already-removed workspace is not an error.
	Remove(ctx context.Context, path, branch string) error
}

// Registry is the single directory of live dispatched workspaces. It
// provisions workspaces through a [Backend], tracks them in memory keyed
// by dispatch id, and sweeps them on close so an abandoned dispatch
// never leaves an orphaned worktree or dangling branch behind.
//
// The Registry is safe for concurrent use.
type Registry struct {
	// dir is the parent directory workspaces are provisioned under.
	dir     string
	backend Backend

	mu      sync.RWMutex
	entries map[string]*Workspace
}

// NewRegistry returns a Registry that provisions workspaces under dir
// via backend.
func NewRegistry(dir string, backend Backend) *Registry {
	return &Registry{
		dir:     dir,
		backend: backend,
		entries: make(map[string]*Workspace),
	}
}

// Create provisions a new workspace forked from base (empty means the
// backend's current head), tracks it as StatusPending, and returns a
// snapshot of it.
func (r *Registry) Create(ctx context.Context, base string) (Workspace, error) {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("create workspace root: %w", err)
	}

	id := uuid.NewString()
	dest := filepath.Join(r.dir, id)

	prov, err := r.backend.Create(ctx, dest, id, base)
	if err != nil {
		return Workspace{}, fmt.Errorf("provision workspace: %w", err)
	}

	ws := &Workspace{
		ID:     id,
		Path:   prov.Path,
		Base:   prov.Base,
		Branch: prov.Branch,
		Status: StatusPending,
	}

	r.mu.Lock()
	r.entries[id] = ws
	r.mu.Unlock()

	return *ws, nil
}

// Get returns a snapshot of the tracked workspace with the given id.
func (r *Registry) Get(id string) (Workspace, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ws, ok := r.entries[id]
	if !ok {
		return Workspace{}, false
	}
	return *ws, true
}

// List returns snapshots of every tracked workspace, ordered by id for
// stable iteration.
func (r *Registry) List() []Workspace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Workspace, 0, len(r.entries))
	for _, ws := range r.entries {
		out = append(out, *ws)
	}
	slices.SortFunc(out, func(a, b Workspace) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

// update applies mutate to the tracked entry under the write lock. It
// reports whether the entry existed.
func (r *Registry) update(id string, mutate func(*Workspace)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ws, ok := r.entries[id]
	if !ok {
		return false
	}
	mutate(ws)
	return true
}

// SetStatus updates a tracked workspace's status. It reports whether the
// workspace was found.
func (r *Registry) SetStatus(id string, status Status) bool {
	return r.update(id, func(ws *Workspace) { ws.Status = status })
}

// SetSessionID binds the backing session to a tracked workspace. It
// reports whether the workspace was found.
func (r *Registry) SetSessionID(id, sessionID string) bool {
	return r.update(id, func(ws *Workspace) { ws.SessionID = sessionID })
}

// SetServed records the A2A endpoint and card a tracked workspace is
// served at (#70). It reports whether the workspace was found.
func (r *Registry) SetServed(id, endpoint string, card any) bool {
	return r.update(id, func(ws *Workspace) {
		ws.Endpoint = endpoint
		ws.Card = card
	})
}

// Diff returns the work product of the tracked workspace: everything
// from its base to the current working tree.
func (r *Registry) Diff(ctx context.Context, id string) (string, error) {
	ws, ok := r.Get(id)
	if !ok {
		return "", fmt.Errorf("workspace %q not tracked", id)
	}
	return r.backend.Diff(ctx, ws.Path, ws.Base)
}

// Remove tears down the tracked workspace and stops tracking it. It is
// idempotent: removing an unknown or already-removed workspace is not an
// error.
func (r *Registry) Remove(ctx context.Context, id string) error {
	r.mu.Lock()
	ws, ok := r.entries[id]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	delete(r.entries, id)
	r.mu.Unlock()

	if err := r.backend.Remove(ctx, ws.Path, ws.Branch); err != nil {
		return fmt.Errorf("remove workspace %q: %w", id, err)
	}
	return nil
}

// Close sweeps every remaining workspace, tearing each down even if its
// dispatch was abandoned. Errors from individual removals are joined so
// one failure does not strand the rest.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	entries := r.entries
	r.entries = make(map[string]*Workspace)
	r.mu.Unlock()

	var errs []error
	for id, ws := range entries {
		if err := r.backend.Remove(ctx, ws.Path, ws.Branch); err != nil {
			errs = append(errs, fmt.Errorf("remove workspace %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
