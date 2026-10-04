// Package dispatch provides the isolated workspace lifecycle behind
// worktree dispatch (Worktree Dispatch epic, #61): the Workspace that
// provisions, tracks, diffs, and cleans up the directories dispatched
// agents run in, plus the dispatch registry every later phase reads.
//
// The component is SCM-generic — Workspace, not Worktree — with git
// worktrees as the first backend. Callers only ever see directories,
// branches, and diffs, so a jj, sapling, or plain-copy backend can
// replace the git calls without touching them.
//
// Phase 1 is in-process: the registry lives in memory on the Workspace,
// and the parent coordinator owns its lifetime. When dispatch crosses a
// process or network boundary (#72/#73), the backend.CreateWorkspace
// path already provisions a scoped app-level workspace for a given
// directory, so the same registry entries keep pointing at workspaces
// served remotely.
//
// The registry is deliberately the only one: A2A discovery (#70) reads
// its entries rather than keeping an endpoint map of its own, and the
// @handle addressing layer (#313) uses it as the handle registry. The
// AgentCard field is opaque to this package so it can stay
// import-cycle-free of internal/a2a.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
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

// BranchPrefix namespaces every branch and directory a dispatched
// workspace creates, so cleanup can recognize its own artifacts no
// matter how they were left behind.
const BranchPrefix = "crush-dispatch-"

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

// Entry is one dispatched workspace in the registry. Entries are handed
// out by value; mutations go through the Workspace's setter methods so
// the registry never hands out a shared pointer.
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
	// provisioned. Diff falls back to it when the base branch no longer
	// exists.
	BaseSHA string
	// SessionID is the ephemeral session backing the dispatched agent
	// (#48/#50); empty until the dispatch starts running.
	SessionID string
	// Handle is the workspace's @handle for agent addressing (#313);
	// empty until one is assigned by [Workspace.AssignHandle].
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

// Workspace owns the isolated-workspace lifecycle and the dispatch
// registry for one parent repository. All methods are safe for
// concurrent use.
type Workspace struct {
	repoRoot string
	// worktreesDir is the directory provisioned workspaces are created
	// under, <repoRoot>/.crush/worktrees.
	worktreesDir string

	// events re-publishes every registry mutation as an entry event, so
	// the todo collector (#65) and later sinks observe lifecycle
	// transitions without the mutation call sites knowing about them.
	events *pubsub.Broker[Entry]

	// instanceID identifies this Workspace instance in the owner
	// markers it writes, so humans can tell concurrent processes apart.
	instanceID string

	mu      sync.Mutex
	entries map[string]Entry
	// leases holds the release function for the ownership lease of
	// every registered entry, keyed by entry ID.
	leases map[string]func()
}

// NewWorkspace returns a Workspace managing isolated workspaces for the
// git repository rooted at repoRoot. It prunes stale worktree admin
// entries left by previous runs and creates the worktrees directory.
func NewWorkspace(repoRoot string) (*Workspace, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	if err := runGit(context.Background(), root, nil, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", repoRoot, err)
	}

	dir := filepath.Join(root, ".crush", "worktrees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create worktrees directory: %w", err)
	}

	// Clear admin entries for worktrees whose directories are gone, so a
	// crashed run's leftovers do not block the same paths later.
	if err := runGit(context.Background(), root, nil, "worktree", "prune"); err != nil {
		return nil, fmt.Errorf("prune stale worktrees: %w", err)
	}

	return &Workspace{
		repoRoot:     root,
		worktreesDir: dir,
		events:       pubsub.NewBroker[Entry](),
		entries:      make(map[string]Entry),
		instanceID:   uuid.New().String(),
		leases:       make(map[string]func()),
	}, nil
}

// ProvisionOptions configures Provision.
type ProvisionOptions struct {
	// Base is the revision the workspace is cut from — a branch, tag, or
	// commit. Empty means the repository's current branch (or HEAD when
	// detached).
	Base string
}

// Provision creates an isolated workspace: a git worktree on a fresh
// crush-dispatch-{uuid} branch under .crush/worktrees/, registers it,
// and returns the new entry. The workspace starts as StatusProvisioned.
func (w *Workspace) Provision(ctx context.Context, opts ProvisionOptions) (Entry, error) {
	base := opts.Base
	if base == "" {
		var err error
		base, err = currentRevision(ctx, w.repoRoot)
		if err != nil {
			return Entry{}, err
		}
	}
	baseSHA, err := revisionSHA(ctx, w.repoRoot, base)
	if err != nil {
		return Entry{}, fmt.Errorf("resolve base %q: %w", base, err)
	}

	id := uuid.New().String()
	branch := BranchPrefix + id
	path := filepath.Join(w.worktreesDir, branch)

	if err := runGit(ctx, w.repoRoot, nil, "worktree", "add", "-b", branch, path, base); err != nil {
		return Entry{}, fmt.Errorf("create worktree: %w", err)
	}

	// The lease and marker live next to the worktree, never inside it,
	// because Diff stages the whole workspace with git add -A.
	release, err := lock.TryFile(w.leasePath(branch))
	if err != nil {
		w.removeEntry(ctx, Entry{Path: path, Branch: branch}, nil)
		return Entry{}, fmt.Errorf("take ownership lease: %w", err)
	}
	if err := w.writeOwnerMarker(branch); err != nil {
		release()
		w.removeEntry(ctx, Entry{Path: path, Branch: branch}, nil)
		return Entry{}, fmt.Errorf("write owner marker: %w", err)
	}

	entry := Entry{
		ID:      id,
		Path:    path,
		Branch:  branch,
		Base:    base,
		BaseSHA: baseSHA,
		Status:  StatusProvisioned,
	}

	w.mu.Lock()
	w.entries[id] = entry
	w.leases[id] = release
	w.mu.Unlock()
	return entry, nil
}

// leasePath is the flock file guarding branch's workspace. It lives
// next to the worktree, never inside it, and the kernel releases the
// lock when the owning process dies — no stale-lock recovery needed.
func (w *Workspace) leasePath(branch string) string {
	return filepath.Join(w.worktreesDir, branch+".lock")
}

// ownerPath is the human-readable owner marker for branch. It carries
// no authority: the lock file is what ownership is proven with.
func (w *Workspace) ownerPath(branch string) string {
	return filepath.Join(w.worktreesDir, branch+".owner.json")
}

// ownerMarker records who holds a workspace's lease.
type ownerMarker struct {
	InstanceID string    `json:"instance_id"`
	PID        int       `json:"pid"`
	CreatedAt  time.Time `json:"created_at"`
}

// writeOwnerMarker atomically records this workspace instance as the
// owner of branch.
func (w *Workspace) writeOwnerMarker(branch string) error {
	data, err := json.Marshal(ownerMarker{
		InstanceID: w.instanceID,
		PID:        os.Getpid(),
		CreatedAt:  time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(w.worktreesDir, ".*.owner.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, w.ownerPath(branch)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Get returns a copy of the entry for id.
func (w *Workspace) Get(id string) (Entry, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.entries[id]
	return e.clone(), ok
}

// ByHandle returns the entry currently carrying handle, if any.
func (w *Workspace) ByHandle(handle string) (Entry, bool) {
	if handle == "" {
		return Entry{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.entries {
		if e.Handle == handle {
			return e.clone(), true
		}
	}
	return Entry{}, false
}

// BySession returns the entry whose dispatched agent runs on sessionID,
// if any.
func (w *Workspace) BySession(sessionID string) (Entry, bool) {
	if sessionID == "" {
		return Entry{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.entries {
		if e.SessionID == sessionID {
			return e.clone(), true
		}
	}
	return Entry{}, false
}

// List returns copies of every entry, ordered by ID.
func (w *Workspace) List() []Entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	entries := make([]Entry, 0, len(w.entries))
	for _, e := range w.entries {
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
// stream ([Workspace.Subscribe]) so the todo collector can re-emit.
func (w *Workspace) Update(id string, fn func(*Entry)) bool {
	if fn == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.entries[id]
	if !ok {
		return false
	}
	fn(&e)
	w.entries[id] = e
	w.events.Publish(pubsub.UpdatedEvent, e)
	return true
}

// SetStatus updates the entry's lifecycle state, stamping the run's
// time bounds as it goes: StartedAt on the first transition to
// running, FinishedAt on the first terminal state. Both are stamped at
// most once, so re-setting the same state does not restart the clock.
func (w *Workspace) SetStatus(id string, status Status) bool {
	return w.Update(id, func(e *Entry) {
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
func (w *Workspace) SetResult(id string, result DispatchResult) bool {
	return w.Update(id, func(e *Entry) {
		r := result
		e.Result = &r
	})
}

// SetSession records the ephemeral session backing the dispatched agent.
func (w *Workspace) SetSession(id, sessionID string) bool {
	return w.Update(id, func(e *Entry) { e.SessionID = sessionID })
}

// SetHandle records the @handle the dispatched agent is addressable by.
func (w *Workspace) SetHandle(id, handle string) bool {
	return w.Update(id, func(e *Entry) { e.Handle = handle })
}

// AssignHandle assigns the entry its @handle and role (#313) and returns
// the assigned handle. requested is the handle the model asked for (the
// leading "@" and any surrounding space are tolerated); role is the
// one-line role label recorded alongside it. When requested is empty the
// handle is derived from the role, and when that is empty too it falls
// back to "agent". A handle that collides with one already claimed by
// any registered dispatch — live or finished, since the registry is the
// handle namespace until the entry is removed — is suffixed numerically:
// tester, tester-2, tester-3. The assignment is atomic under the registry
// lock, so two concurrent dispatches asking for the same handle get
// distinct suffixed ones, and it publishes the usual entry event so the
// dispatch block and @ completions observe the handle immediately.
func (w *Workspace) AssignHandle(id, requested, role string) (string, bool) {
	handle := HandleSlug(requested)
	if handle == "" {
		handle = HandleSlug(role)
	}
	if handle == "" {
		handle = "agent"
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.entries[id]
	if !ok {
		return "", false
	}
	handle = uniqueHandle(w.entries, handle)
	e.Handle = handle
	e.Role = strings.TrimSpace(role)
	w.entries[id] = e
	w.events.Publish(pubsub.UpdatedEvent, e)
	return handle, true
}

// uniqueHandle returns handle, or its first free numeric suffix, against
// every handle claimed in entries. Callers must hold w.mu.
func uniqueHandle(entries map[string]Entry, handle string) string {
	taken := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Handle != "" {
			taken[e.Handle] = true
		}
	}
	if !taken[handle] {
		return handle
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", handle, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// HandleSlug normalizes a candidate handle or role into handle form:
// lowercase, runs of non-alphanumerics collapsed to single dashes, and
// no leading or trailing dash. "@Team Lead" becomes "team-lead"; a
// candidate that slugs to nothing ("@__"), or an empty one, stays empty
// so the caller can fall through to its next fallback.
func HandleSlug(candidate string) string {
	candidate = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(candidate), "@"))
	candidate = strings.ToLower(candidate)
	var b strings.Builder
	dash := false
	for _, r := range candidate {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// SetEndpoint records the A2A endpoint and AgentCard serving the
// dispatched agent (#70).
func (w *Workspace) SetEndpoint(id, endpoint string, card any) bool {
	return w.Update(id, func(e *Entry) {
		e.Endpoint = endpoint
		e.AgentCard = card
	})
}

// Subscribe returns the registry's entry event stream: one
// UpdatedEvent per successful mutation, carrying the entry by value.
// The todo collector (#65) is its first subscriber; anything else that
// needs dispatch lifecycle state (#174's A2A bridge) reads the same
// stream rather than polling the registry.
func (w *Workspace) Subscribe(ctx context.Context) <-chan pubsub.Event[Entry] {
	return w.events.Subscribe(ctx)
}

// Diff returns the dispatched agent's work product as one unified diff:
// base...HEAD plus the uncommitted state (staged, unstaged, and
// untracked files), so committed work is never lost. The base is the
// merge-base of the entry's Base and HEAD — falling back to the
// recorded BaseSHA when the base branch no longer exists. Uncommitted
// changes are staged into a throwaway temporary index, mirroring
// a2a.GitDiff, so the workspace's real index is never touched.
//
// The contract is owned here and consumed twice: #66's DispatchResult
// diff and the executor DiffFunc injected in #71.
func (w *Workspace) Diff(ctx context.Context, id string) (string, error) {
	entry, ok := w.Get(id)
	if !ok {
		return "", fmt.Errorf("unknown dispatch %q", id)
	}
	if _, err := os.Stat(entry.Path); err != nil {
		return "", fmt.Errorf("workspace directory missing: %w", err)
	}

	base := entry.BaseSHA
	if out, err := gitOutput(ctx, entry.Path, nil, "merge-base", entry.Base, "HEAD"); err == nil {
		base = strings.TrimSpace(string(out))
	}

	tmp, err := os.CreateTemp("", "crush-dispatch-index-")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	env := append(os.Environ(), "GIT_INDEX_FILE="+tmp.Name())

	// Seed the temp index from HEAD, stage the whole workspace into it
	// (respecting .gitignore so untracked files are included), and diff
	// it against the base. Committed changes are in the index via HEAD;
	// uncommitted changes via the add.
	if err := runGit(ctx, entry.Path, env, "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("seed temp index: %w", err)
	}
	if err := runGit(ctx, entry.Path, env, "add", "-A"); err != nil {
		return "", fmt.Errorf("stage workspace: %w", err)
	}
	out, err := gitOutput(ctx, entry.Path, env, "diff", "--cached", base)
	if err != nil {
		return "", fmt.Errorf("diff against base: %w", err)
	}
	return string(out), nil
}

// Remove cleans up a dispatched workspace: it removes the worktree,
// deletes its branch, and drops the registry entry. It is idempotent —
// removing an unknown or already-removed dispatch succeeds — and safe
// to call on a dispatch whose directory or branch is already gone.
func (w *Workspace) Remove(ctx context.Context, id string) error {
	w.mu.Lock()
	entry, ok := w.entries[id]
	release := w.leases[id]
	delete(w.entries, id)
	delete(w.leases, id)
	w.mu.Unlock()
	if !ok {
		return nil
	}
	return w.removeEntry(ctx, entry, release)
}

// Sweep removes every tracked workspace plus every orphaned
// crush-dispatch-* directory whose ownership lease is free (from a
// crashed run whose registry was lost). Orphans a live process still
// holds — another Workspace on the same repository — and orphans with
// no lease file at all, whose ownership cannot be proven, are left
// alone (#369 is the explicit cleanup tool for those).
func (w *Workspace) Sweep(ctx context.Context) error {
	w.mu.Lock()
	type sweepEntry struct {
		entry   Entry
		release func()
	}
	items := make([]sweepEntry, 0, len(w.entries))
	for id, e := range w.entries {
		items = append(items, sweepEntry{e, w.leases[id]})
		delete(w.leases, id)
	}
	w.entries = make(map[string]Entry)
	w.mu.Unlock()

	for _, item := range items {
		if err := w.removeEntry(ctx, item.entry, item.release); err != nil {
			return err
		}
	}

	// Orphaned directories: registered names are gone by now, so
	// anything left under the worktrees dir with our prefix belongs to a
	// run whose registry entry was lost. The directory name is the
	// branch name, so the branch is recoverable from it.
	dirs, err := os.ReadDir(w.worktreesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), BranchPrefix) {
			continue
		}
		lockPath := w.leasePath(d.Name())
		if _, err := os.Stat(lockPath); err != nil {
			if os.IsNotExist(err) {
				// Without a lease file ownership cannot be proven,
				// so the directory is not ours to take.
				slog.Debug("Skipping dispatch worktree without a lease file", "path", filepath.Join(w.worktreesDir, d.Name()))
				continue
			}
			return err
		}
		release, err := lock.TryFile(lockPath)
		if err != nil {
			if errors.Is(err, lock.ErrContended) {
				// A live process — another Workspace on this
				// repository — still owns this workspace.
				slog.Debug("Skipping dispatch worktree with a held lease", "path", filepath.Join(w.worktreesDir, d.Name()))
				continue
			}
			return err
		}
		entry := Entry{
			Path:   filepath.Join(w.worktreesDir, d.Name()),
			Branch: d.Name(),
		}
		if err := w.removeEntry(ctx, entry, release); err != nil {
			return err
		}
	}
	return runGit(ctx, w.repoRoot, nil, "worktree", "prune")
}

// removeEntry tears down one workspace on disk: the worktree (forced if
// it is dirty), the branch, the ownership artifacts, and the admin
// entry. Missing artifacts are not errors. When lease is not nil the
// lock file is unlinked while the lease is still held — dispatch IDs
// are never reused, so no other holder can appear in between — and the
// lease is released last. Windows cannot unlink a file its own process
// holds open, so there the lease is dropped first and the unlink
// retried.
func (w *Workspace) removeEntry(ctx context.Context, entry Entry, lease func()) error {
	if entry.Path != "" {
		if err := runGit(ctx, w.repoRoot, nil, "worktree", "remove", entry.Path); err != nil {
			// A dirty workspace still removes with --force; a missing
			// one is already gone and prune cleans the admin entry.
			if err := runGit(ctx, w.repoRoot, nil, "worktree", "remove", "--force", entry.Path); err != nil {
				if err := runGit(ctx, w.repoRoot, nil, "worktree", "prune"); err != nil {
					return fmt.Errorf("remove worktree %s: %w", entry.Path, err)
				}
			}
		}
	}
	if entry.Branch != "" {
		// -D because a dispatched branch may be unmerged — that is the
		// point of an explicit review step — and a missing branch is
		// already gone.
		if err := runGit(ctx, w.repoRoot, nil, "branch", "-D", entry.Branch); err != nil {
			if !isBranchMissing(err) {
				return fmt.Errorf("delete branch %s: %w", entry.Branch, err)
			}
		}
	}
	if entry.Branch != "" {
		os.Remove(w.ownerPath(entry.Branch))
	}
	if lease != nil {
		if os.Remove(w.leasePath(entry.Branch)) != nil {
			lease()
			os.Remove(w.leasePath(entry.Branch))
			return nil
		}
		lease()
	}
	return nil
}

// isBranchMissing reports whether a git branch error is just "no such
// branch", which cleanup treats as success.
func isBranchMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// currentRevision returns the repository's current branch name, or HEAD
// when detached.
func currentRevision(ctx context.Context, repoRoot string) (string, error) {
	out, err := gitOutput(ctx, repoRoot, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve current revision: %w", err)
	}
	rev := strings.TrimSpace(string(out))
	if rev == "" || rev == "HEAD" {
		return "HEAD", nil
	}
	return rev, nil
}

// revisionSHA resolves a revision to its commit SHA.
func revisionSHA(ctx context.Context, repoRoot, rev string) (string, error) {
	out, err := gitOutput(ctx, repoRoot, nil, "rev-parse", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitOutput runs git in dir with env and returns stdout.
func gitOutput(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.Output()
}

// runGit runs git in dir with env, attaching stderr to the error.
func runGit(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
