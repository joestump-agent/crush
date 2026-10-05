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
	// Disposition is what a human decided about the work (#367):
	// pending review until they act, then applied or dismissed. Exit
	// cleanup and startup reconciliation remove applied and dismissed
	// workspaces and keep pending ones that produced work.
	Disposition Disposition
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
	// commonDir is the repository's git common dir, resolved in
	// NewWorkspace. It keys the per-repo provision lock, so two
	// Workspaces on the same repository serialize their worktree adds.
	commonDir string

	// events re-publishes every registry mutation as an entry event, so
	// the todo collector (#65) and later sinks observe lifecycle
	// transitions without the mutation call sites knowing about them.
	events *pubsub.Broker[Entry]

	// provisionHook, when set, runs just before the locked worktree add
	// with the branch and path about to be created. Tests use it to
	// force a deterministic failure, such as a pre-created non-empty
	// target directory.
	provisionHook func(branch, path string)

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

	// The provision lock key: the git common dir, so every checkout of
	// the same repository — this one or a worktree — shares a lock.
	common, err := gitOutput(context.Background(), root, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir: %w", err)
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return nil, err
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

	w := &Workspace{
		repoRoot:     root,
		worktreesDir: dir,
		commonDir:    commonDir,
		events:       pubsub.NewBroker[Entry](),
		entries:      make(map[string]Entry),
		instanceID:   uuid.New().String(),
		leases:       make(map[string]func()),
	}
	// Startup reconciliation (#367): workspaces a human decided about,
	// and dead owners' workspaces that never produced work, are removed;
	// everything else is kept for salvage.
	w.reconcileStartup(context.Background())
	return w, nil
}

// reconcileStartup applies the decisions recorded in the worktrees
// directory's owner markers (#367): applied and dismissed workspaces
// are removed, and a dead owner's workspace that never produced work —
// no commits ahead of its recorded base and a clean tree — is removed
// too. Everything else is kept: pending work from a crashed run stays
// on disk for salvage, and a live owner's entries are never touched.
// Failures are logged and left for a later reconciliation or cleanup;
// startup never fails because leftovers could not be removed.
func (w *Workspace) reconcileStartup(ctx context.Context) {
	markers, err := filepath.Glob(filepath.Join(w.worktreesDir, "*"+ownerMarkerSuffix))
	if err != nil {
		return
	}
	for _, path := range markers {
		branch := strings.TrimSuffix(filepath.Base(path), ownerMarkerSuffix)
		// A marker's file name is the only thing naming the workspace it
		// describes, and that name feeds every path below, so refuse
		// anything that is not a plain dispatch branch name: a stray or
		// malicious file in the worktrees directory cannot point cleanup
		// outside it.
		if !strings.HasPrefix(branch, BranchPrefix) || strings.ContainsAny(branch, `/\`) || strings.Contains(branch, "..") {
			slog.Debug("Skipping dispatch owner marker with unusable name", "path", path)
			continue
		}
		// codeql[go/path-injection] the name is validated to BranchPrefix
		// with no separators or traversal above, so path stays inside
		// worktreesDir.
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Debug("Skipping unreadable dispatch owner marker", "path", path, "error", err)
			continue
		}
		var m ownerMarker
		if err := json.Unmarshal(data, &m); err != nil {
			slog.Debug("Skipping malformed dispatch owner marker", "path", path, "error", err)
			continue
		}

		// Take the lease: a contended lease belongs to a live process,
		// whose entries are never touched; a free lease means the owner
		// died and the decision is ours to apply.
		release, err := lock.TryFile(w.leasePath(branch))
		if err != nil {
			continue
		}
		entry := Entry{
			Path:    filepath.Join(w.worktreesDir, branch),
			Branch:  branch,
			BaseSHA: m.BaseSHA,
		}
		if m.Disposition != DispositionApplied && m.Disposition != DispositionDismissed && w.hasWork(ctx, entry) {
			release()
			continue
		}
		if err := w.removeEntry(ctx, entry, release); err != nil {
			slog.Debug("Startup reconciliation left a dispatch workspace in place", "branch", branch, "error", err)
		}
	}
	_ = runGit(ctx, w.repoRoot, nil, "worktree", "prune")
}

// hasWork reports whether the workspace produced work a human might
// want (#367): commits on its branch ahead of the recorded base, or
// uncommitted changes on disk. An unknown base, a branch that no longer
// resolves, or a git probe error count as work — the fail-safe is to
// keep the workspace, never to discard it.
func (w *Workspace) hasWork(ctx context.Context, entry Entry) bool {
	if entry.BaseSHA == "" {
		return true
	}
	out, err := gitOutput(ctx, w.repoRoot, nil, "rev-list", "--count", entry.BaseSHA+".."+entry.Branch)
	if err != nil {
		return true
	}
	if strings.TrimSpace(string(out)) != "0" {
		return true
	}
	if entry.Path != "" {
		out, err = gitOutput(ctx, entry.Path, nil, "status", "--porcelain")
		if err != nil {
			return true
		}
		if strings.TrimSpace(string(out)) != "" {
			return true
		}
	}
	return false
}

// provisionLocks serializes the worktree add step per repository:
// git's .git/config lock makes concurrent worktree add calls fail
// ("could not lock config file") even when they create disjoint
// branches, so the add and its failure cleanup run one at a time per
// repo. Keying by git's common dir means two Workspaces on the same
// repository in one process serialize on the same lock.
var (
	provisionLocksMu sync.Mutex
	provisionLocks   = make(map[string]*sync.Mutex)
)

// provisionLock returns the lock guarding worktree add for the repo
// whose git common dir is key, creating it on first use.
func provisionLock(key string) *sync.Mutex {
	provisionLocksMu.Lock()
	defer provisionLocksMu.Unlock()
	lock, ok := provisionLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		provisionLocks[key] = lock
	}
	return lock
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
	// Defence in depth: a base starting with "-" would be read as a git
	// option further down the line, so refuse it before git sees it.
	if strings.HasPrefix(base, "-") {
		return Entry{}, fmt.Errorf("invalid base %q: must not start with \"-\"", base)
	}
	baseSHA, err := revisionSHA(ctx, w.repoRoot, base)
	if err != nil {
		return Entry{}, fmt.Errorf("resolve base %q: %w", base, err)
	}

	id := uuid.New().String()
	branch := BranchPrefix + id
	path := filepath.Join(w.worktreesDir, branch)

	// The add and its failure cleanup run one at a time per
	// repository, but nothing else in the provision is locked.
	addLock := provisionLock(w.commonDir)
	addLock.Lock()
	if w.provisionHook != nil {
		w.provisionHook(branch, path)
	}
	// The resolved SHA is what worktree add gets, never the raw base
	// string, and "--" ends option parsing before the positionals.
	err = runGit(ctx, w.repoRoot, nil, "worktree", "add", "--no-track", "-b", branch, "--", path, baseSHA)
	if err != nil {
		w.cleanupFailedProvision(ctx, branch, path)
		addLock.Unlock()
		return Entry{}, fmt.Errorf("create worktree: %w", err)
	}
	addLock.Unlock()

	// The lease and marker live next to the worktree, never inside it,
	// because Diff stages the whole workspace with git add -A.
	release, err := lock.TryFile(w.leasePath(branch))
	if err != nil {
		w.removeEntry(ctx, Entry{Path: path, Branch: branch}, nil)
		return Entry{}, fmt.Errorf("take ownership lease: %w", err)
	}
	if err := w.writeOwnerMarker(branch, baseSHA); err != nil {
		release()
		w.removeEntry(ctx, Entry{Path: path, Branch: branch}, nil)
		return Entry{}, fmt.Errorf("write owner marker: %w", err)
	}

	entry := Entry{
		ID:          id,
		Path:        path,
		Branch:      branch,
		Base:        base,
		BaseSHA:     baseSHA,
		Status:      StatusProvisioned,
		Disposition: DispositionPendingReview,
	}

	w.mu.Lock()
	w.entries[id] = entry
	w.leases[id] = release
	w.mu.Unlock()
	return entry, nil
}

// cleanupFailedProvision removes what a failed worktree add may have
// left behind: the branch -b can create before the add fails, a
// partially created directory, and stale worktree admin entries. It
// runs under the repository's provision lock, and its own failures are
// ignored: the caller returns the original error either way. The
// directory is removed before the branch, and prune runs before
// branch -D: while a worktree admin entry remains, git still regards
// the branch as checked out and refuses to delete it.
func (w *Workspace) cleanupFailedProvision(ctx context.Context, branch, path string) {
	// False positive: branch is generated here from a UUID, so path
	// always stays inside worktreesDir and RemoveAll cannot escape the
	// worktrees directory; the repo root is the directory the client
	// asked the server to open.
	// codeql[go/path-injection]
	_ = os.RemoveAll(path)
	_ = runGit(ctx, w.repoRoot, nil, "worktree", "prune")
	if _, err := gitOutput(ctx, w.repoRoot, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_ = runGit(ctx, w.repoRoot, nil, "branch", "-D", branch)
	}
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
	return filepath.Join(w.worktreesDir, branch+ownerMarkerSuffix)
}

// ownerMarkerSuffix is the filename suffix of an owner marker next to
// its worktree.
const ownerMarkerSuffix = ".owner.json"

// ownerMarker records who holds a workspace's lease, plus what the
// workspace needs to be judged without the registry (#367): the base
// it was cut from, so commits-ahead can be counted after a crash, and
// the human's disposition, so decided workspaces can be cleaned up.
type ownerMarker struct {
	InstanceID  string      `json:"instance_id"`
	PID         int         `json:"pid"`
	CreatedAt   time.Time   `json:"created_at"`
	BaseSHA     string      `json:"base_sha,omitempty"`
	Disposition Disposition `json:"disposition"`
}

// writeOwnerMarker atomically records this workspace instance as the
// owner of branch, with the base the workspace was cut from and the
// initial pending-review disposition (#367).
func (w *Workspace) writeOwnerMarker(branch, baseSHA string) error {
	data, err := json.Marshal(ownerMarker{
		InstanceID:  w.instanceID,
		PID:         os.Getpid(),
		CreatedAt:   time.Now().UTC(),
		BaseSHA:     baseSHA,
		Disposition: DispositionPendingReview,
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

// ByHandle returns the entry currently carrying handle, if any. A
// non-terminal entry always wins: handles live as long as their run
// (#399). When no live entry carries the handle, the most recently
// finished one answers (max FinishedAt), so a mention of a finished
// @handle still renders its read-only card until the handle is reused.
func (w *Workspace) ByHandle(handle string) (Entry, bool) {
	if handle == "" {
		return Entry{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var finished Entry
	haveFinished := false
	for _, e := range w.entries {
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

// Disposition records what a human decided about a workspace's work
// (#367). A provisioned workspace starts as pending review; #368's
// apply and dismiss tools move it to applied or dismissed. Exit
// cleanup and startup reconciliation remove decided workspaces and
// keep pending ones that produced work — nothing automatically
// discards work-in-progress a human might want.
type Disposition string

const (
	// DispositionPendingReview is the initial disposition: the work is
	// on disk and on its branch, waiting for the human.
	DispositionPendingReview Disposition = "pending_review"
	// DispositionApplied means the work was merged or adopted; the
	// workspace has nothing left to say and is cleaned up.
	DispositionApplied Disposition = "applied"
	// DispositionDismissed means the human decided the work is not
	// wanted; the workspace is cleaned up.
	DispositionDismissed Disposition = "dismissed"
)

// SetDisposition records the human's decision on the entry and in the
// owner marker (#367), so the decision survives a crash. It reports
// whether the entry exists.
func (w *Workspace) SetDisposition(id string, d Disposition) bool {
	w.mu.Lock()
	entry, ok := w.entries[id]
	w.mu.Unlock()
	if !ok {
		return false
	}
	if err := w.setMarkerDisposition(entry.Branch, d); err != nil {
		slog.Warn("Failed to record dispatch disposition in owner marker", "dispatch_id", id, "branch", entry.Branch, "error", err)
	}
	return w.Update(id, func(e *Entry) { e.Disposition = d })
}

// setMarkerDisposition rewrites branch's owner marker with the new
// disposition, atomically. A missing marker is not an error: the
// provision that writes it failed before the marker step, and the
// entry-level record still stands.
func (w *Workspace) setMarkerDisposition(branch string, d Disposition) error {
	path := w.ownerPath(branch)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var m ownerMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	m.Disposition = d
	updated, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(w.worktreesDir, ".*.owner.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(updated); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// SetParentSessionID records the session the dispatch was created from
// (#399): the scope handle and session-ID addressing are validated
// against, so another session cannot address this dispatch.
func (w *Workspace) SetParentSessionID(id, parentSessionID string) bool {
	return w.Update(id, func(e *Entry) { e.ParentSessionID = parentSessionID })
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
// back to "agent". A handle lives as long as its run (#399): a handle
// that collides with one claimed by a non-terminal dispatch — or with a
// name in [ReservedHandles] — is suffixed numerically: tester, tester-2,
// tester-3. A finished dispatch's handle is free for reuse, and the
// suffixing truncates the base so the result never exceeds
// [MaxHandleLength]. The assignment is atomic under the registry
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
// every handle claimed by a non-terminal entry and every reserved name
// (#399): a finished dispatch releases its handle for reuse. Suffixing
// truncates the base so the result fits MaxHandleLength. Callers must
// hold w.mu.
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
// the recorded base SHA...HEAD plus the uncommitted state (staged,
// unstaged, and untracked files), so committed work is never lost. The
// base is always entry.BaseSHA, the commit the dispatch branch was cut
// from, resolved in the repository root at provision time. Never
// re-resolve the base revision against the worktree: a "HEAD" or
// "HEAD~1" base would resolve to the worktree's own tip and the
// agent's commits would vanish from the diff (#380). An empty BaseSHA
// is an error, not a fallback. Uncommitted changes are staged into a
// throwaway temporary index, mirroring a2a.GitDiff, so the
// workspace's real index is never touched.
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
	if entry.BaseSHA == "" {
		return "", fmt.Errorf("dispatch %q has no recorded base SHA", id)
	}

	tmp, err := os.CreateTemp("", "crush-dispatch-index-")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	// The temporary index is the only git variable this call adds;
	// gitCmd scrubs the inherited ones so an outer GIT_DIR,
	// GIT_INDEX_FILE or GIT_WORK_TREE cannot redirect the commands.
	extraEnv := []string{"GIT_INDEX_FILE=" + tmp.Name()}

	// Seed the temp index from HEAD, stage the whole workspace into it
	// (respecting .gitignore so untracked files are included), and diff
	// it against the base. Committed changes are in the index via HEAD;
	// uncommitted changes via the add.
	if err := runGit(ctx, entry.Path, extraEnv, "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("seed temp index: %w", err)
	}
	if err := runGit(ctx, entry.Path, extraEnv, "add", "-A"); err != nil {
		return "", fmt.Errorf("stage workspace: %w", err)
	}
	// The diff's output is parsed by SummarizeDiff, so pin the format on
	// the command line: no external diff driver or textconv, no color,
	// and the a/ b/ prefixes the parser expects.
	out, err := gitOutput(ctx, entry.Path, extraEnv,
		"-c", "color.ui=never",
		"-c", "diff.noprefix=false",
		"-c", "diff.mnemonicPrefix=false",
		"diff", "--no-ext-diff", "--no-textconv", "--no-color",
		"--src-prefix=a/", "--dst-prefix=b/",
		"--cached", entry.BaseSHA)
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

// Release tears down this instance's dispatch workspaces at exit, the
// synchronous replacement for the exit-time Sweep (#367): entries the
// human decided about (applied or dismissed) and entries that never
// produced work — no commits ahead of the recorded base and a clean
// tree — are removed with their branches. Completed and killed entries
// that produced work stay on disk and on their branch for salvage, and
// another instance's live entries are never touched: only this
// registry's entries are considered. The orphan pass is not part of
// exit — startup reconciliation owns it. A stubborn entry stays
// registered so a later Release or Remove can retry it, and the errors
// are joined and returned.
func (w *Workspace) Release(ctx context.Context) error {
	w.mu.Lock()
	type releaseEntry struct {
		entry   Entry
		release func()
	}
	var doomed []releaseEntry
	for id, e := range w.entries {
		if w.entryDisposable(ctx, e) {
			doomed = append(doomed, releaseEntry{e, w.leases[id]})
			delete(w.entries, id)
			delete(w.leases, id)
		}
	}
	w.mu.Unlock()

	var errs []error
	for _, item := range doomed {
		if err := w.removeEntry(ctx, item.entry, item.release); err != nil {
			errs = append(errs, err)
			// The removal failed, so the entry and its lease stay
			// registered for a later Release or Remove to retry.
			w.mu.Lock()
			w.entries[item.entry.ID] = item.entry
			if item.release != nil {
				w.leases[item.entry.ID] = item.release
			}
			w.mu.Unlock()
		}
	}
	if err := runGit(ctx, w.repoRoot, nil, "worktree", "prune"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// entryDisposable reports whether exit cleanup may remove the entry
// (#367): a disposition the human recorded (applied or dismissed), or
// a pending workspace that never produced work. A git probe error or
// an unknown base keeps the workspace — the fail-safe is never to
// discard work a human might want.
func (w *Workspace) entryDisposable(ctx context.Context, entry Entry) bool {
	switch entry.Disposition {
	case DispositionApplied, DispositionDismissed:
		return true
	default:
		return !w.hasWork(ctx, entry)
	}
}

// Sweep removes every tracked workspace plus every orphaned
// crush-dispatch-* directory whose ownership lease is free (from a
// crashed run whose registry was lost). Orphans a live process still
// holds — another Workspace on the same repository — and orphans with
// no lease file at all, whose ownership cannot be proven, are left
// alone (#369 is the explicit cleanup tool for those). It is the
// explicit "discard everything" cleanup: nothing dispatch created
// survives it, and no dangling branches or .crush/worktrees/ entries
// are left behind. One stubborn workspace does not stop the sweep:
// every entry and every orphan is attempted, the errors are joined and
// returned, and a failed entry stays registered so a later sweep or
// Remove can retry it.
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

	var errs []error
	for _, item := range items {
		if err := w.removeEntry(ctx, item.entry, item.release); err != nil {
			errs = append(errs, err)
			// The removal failed, so the entry and its lease stay
			// registered for a later sweep or Remove to retry.
			w.mu.Lock()
			w.entries[item.entry.ID] = item.entry
			if item.release != nil {
				w.leases[item.entry.ID] = item.release
			}
			w.mu.Unlock()
		}
	}

	// Orphaned directories: registered names — including entries this
	// sweep just failed, whose removal a later sweep or Remove will
	// retry — are not ours to take, so anything left under the
	// worktrees dir with our prefix and an unregistered branch belongs
	// to a run whose registry entry was lost. The directory name is the
	// branch name, so the branch is recoverable from it.
	w.mu.Lock()
	registered := make(map[string]struct{}, len(w.entries))
	for _, e := range w.entries {
		registered[e.Branch] = struct{}{}
	}
	w.mu.Unlock()

	dirs, err := os.ReadDir(w.worktreesDir)
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	} else {
		for _, d := range dirs {
			if !d.IsDir() || !strings.HasPrefix(d.Name(), BranchPrefix) {
				continue
			}
			if _, ok := registered[d.Name()]; ok {
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
				errs = append(errs, err)
				continue
			}
			release, err := lock.TryFile(lockPath)
			if err != nil {
				if errors.Is(err, lock.ErrContended) {
					// A live process — another Workspace on this
					// repository — still owns this workspace.
					slog.Debug("Skipping dispatch worktree with a held lease", "path", filepath.Join(w.worktreesDir, d.Name()))
					continue
				}
				errs = append(errs, err)
				continue
			}
			entry := Entry{
				Path:   filepath.Join(w.worktreesDir, d.Name()),
				Branch: d.Name(),
			}
			if err := w.removeEntry(ctx, entry, release); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := runGit(ctx, w.repoRoot, nil, "worktree", "prune"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeEntry tears down one workspace on disk: the worktree (forced if
// it is dirty), the branch, and the admin entry. Missing artifacts are
// not errors. The lock file is never unlinked: flock is keyed by inode,
// so removing it can let two processes lock different inodes at the
// same path and both believe they own the workspace. When lease is not
// nil the owner marker is removed — only a lease holder may touch
// ownership artifacts — and the lease is released last, after
// everything else is gone.
func (w *Workspace) removeEntry(ctx context.Context, entry Entry, lease func()) error {
	// Errors are prefixed with the entry so a joined sweep error says
	// what failed. Orphan entries carry no ID; their branch is the name.
	label := entry.ID
	if label == "" {
		label = entry.Branch
	}
	if entry.Path != "" {
		if err := runGit(ctx, w.repoRoot, nil, "worktree", "remove", entry.Path); err != nil {
			// A dirty workspace still removes with --force; a missing
			// one is already gone and prune cleans the admin entry.
			if err := runGit(ctx, w.repoRoot, nil, "worktree", "remove", "--force", entry.Path); err != nil {
				if err := runGit(ctx, w.repoRoot, nil, "worktree", "prune"); err != nil {
					return fmt.Errorf("dispatch %s: remove worktree %s: %w", label, entry.Path, err)
				}
			}
		}
	}
	if entry.Branch != "" {
		// -D because a dispatched branch may be unmerged — that is the
		// point of an explicit review step. Existence is checked by exit
		// code, not by git's message text, so a deleted branch is
		// tolerated under any locale.
		if hasBranch(ctx, w.repoRoot, entry.Branch) {
			if err := runGit(ctx, w.repoRoot, nil, "branch", "-D", entry.Branch); err != nil {
				// A concurrent Remove may have deleted the branch
				// between the existence check and -D; gone is gone, so
				// only an error on a branch still there is real.
				if hasBranch(ctx, w.repoRoot, entry.Branch) {
					return fmt.Errorf("dispatch %s: delete branch %s: %w", label, entry.Branch, err)
				}
			}
		}
	}
	if entry.Branch != "" && lease != nil {
		os.Remove(w.ownerPath(entry.Branch))
		lease()
	}
	return nil
}

// hasBranch reports whether the repository has the local branch, decided
// by git's exit code so the check never reads localized message text.
func hasBranch(ctx context.Context, dir, branch string) bool {
	cmd := gitCmd(ctx, dir, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
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

// revisionSHA resolves a revision to its commit SHA. --end-of-options
// stops a rev that starts with "-" from being parsed as an option, and
// every failure, including empty output, is an unknown-revision error.
func revisionSHA(ctx context.Context, repoRoot, rev string) (string, error) {
	out, err := gitOutput(ctx, repoRoot, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("unknown revision %q", rev)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCmd builds a git command that runs hermetically. It starts from
// the current environment and strips the git variables that leak in
// from a surrounding shell or hook: GIT_DIR, GIT_WORK_TREE,
// GIT_INDEX_FILE, GIT_COMMON_DIR, GIT_OBJECT_DIRECTORY,
// GIT_ALTERNATE_OBJECT_DIRECTORIES and GIT_PREFIX. With them gone the
// command can only act on the repository rooted at dir. LC_ALL is
// pinned to C for stable, locale-independent output, and extraEnv is
// appended on top so callers can still override, for example a
// temporary GIT_INDEX_FILE.
func gitCmd(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	env := make([]string, 0, len(os.Environ())+1+len(extraEnv))
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "LC_ALL=C")
	env = append(env, extraEnv...)
	cmd.Env = env
	return cmd
}

// gitOutput runs git in dir with extraEnv and returns stdout.
func gitOutput(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	return gitCmd(ctx, dir, extraEnv, args...).Output()
}

// runGit runs git in dir with extraEnv, attaching stderr to the error.
func runGit(ctx context.Context, dir string, extraEnv []string, args ...string) error {
	out, err := gitCmd(ctx, dir, extraEnv, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
