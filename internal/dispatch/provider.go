package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
	"github.com/google/uuid"
)

// BranchPrefix namespaces every branch and directory a dispatched
// workspace creates, so cleanup can recognize its own artifacts no
// matter how they were left behind.
const BranchPrefix = "crush-dispatch-"

// ProvisionOptions configures [WorkspaceProvider.Provision].
type ProvisionOptions struct {
	// Base is the revision the workspace is cut from: a branch, tag, or
	// commit. Empty means the repository's current branch (or HEAD when
	// detached).
	Base string
}

// Placement is what a successful [WorkspaceProvider.Provision]
// created on disk: the directory the dispatched agent runs in, the
// branch it carries, and the base it was cut from, with Base resolved
// and BaseSHA pinned. The caller stamps these onto the registry entry.
type Placement struct {
	// Path is the absolute workspace directory.
	Path string
	// Branch is the workspace branch, [BranchPrefix] + the dispatch ID.
	Branch string
	// Base is the revision the workspace was cut from: the requested
	// base, or the repository's current branch when it was empty.
	Base string
	// BaseSHA is the commit Base resolved to at provision time. Diff
	// always diffs against it.
	BaseSHA string
}

// WorkspaceProvider owns the isolated-workspace lifecycle for
// dispatched agents: provisioning the directory an agent runs in,
// diffing its work product, and releasing it. Implementations own
// their on-disk state; the registry ([AgentRegistry]) owns the
// entries, and the two never reach into each other. All methods are
// safe for concurrent use.
type WorkspaceProvider interface {
	// Provision creates an isolated workspace for dispatch id and
	// returns its placement. The caller builds the entry and registers
	// it in the registry.
	Provision(ctx context.Context, id string, opts ProvisionOptions) (Placement, error)
	// Diff returns the workspace's work product as one unified diff.
	Diff(ctx context.Context, entry Entry) (string, error)
	// Release tears the workspace down. It is idempotent: releasing an
	// unknown or already-released dispatch succeeds.
	Release(ctx context.Context, entry Entry) error
}

// GitWorktreeProvider is the git worktree [WorkspaceProvider] for one
// parent repository: every Provision creates a git worktree on a fresh
// crush-dispatch-{id} branch under the provider's worktrees directory.
// The registry is injected so Sweep can drop and re-register entries
// around teardown. All methods are safe for concurrent use.
type GitWorktreeProvider struct {
	repoRoot string
	// worktreesDir is the directory provisioned workspaces are created
	// under, [WorktreesDir]'s result.
	worktreesDir string
	// commonDir is the repository's git common dir, resolved in
	// NewGitWorktreeProvider. It keys the per-repo provision lock, so
	// two providers on the same repository serialize their worktree
	// adds.
	commonDir string

	// reg is the registry the caller registers provisioned entries in.
	// Sweep reads it for the tracked set and drops entries from it as
	// their teardown starts.
	reg *AgentRegistry

	// provisionHook, when set, runs just before the locked worktree add
	// with the branch and path about to be created. Tests use it to
	// force a deterministic failure, such as a pre-created non-empty
	// target directory.
	provisionHook func(branch, path string)

	// instanceID identifies this provider instance in the owner
	// markers it writes, so humans can tell concurrent processes apart.
	instanceID string

	mu sync.Mutex
	// leases holds the release function for the ownership lease of
	// every provisioned workspace, keyed by dispatch ID.
	leases map[string]func()
}

var _ WorkspaceProvider = (*GitWorktreeProvider)(nil)

// NewGitWorktreeProvider returns a provider managing isolated
// workspaces for the git repository rooted at repoRoot, created under
// worktreesDir — the caller passes [WorktreesDir]'s result for the
// active data directory (#383) — and registers provisioned entries in
// reg. It prunes stale worktree admin entries left by previous runs,
// creates the worktrees directory, and keeps the location invisible to
// git by writing a "*" .gitignore beside it when one is missing: the
// data directory the worktrees live under is often inside the
// repository, and the ignore file must hold even when nothing else
// gitignores it.
func NewGitWorktreeProvider(repoRoot, worktreesDir string, reg *AgentRegistry) (*GitWorktreeProvider, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	if err := runGit(context.Background(), root, nil, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", repoRoot, err)
	}

	// The provision lock key: the canonical git common dir, so every
	// checkout of the same repository — this one or a worktree —
	// shares a lock.
	commonDir, err := gitCommonDir(root)
	if err != nil {
		return nil, err
	}

	dir, err := filepath.Abs(worktreesDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create worktrees directory: %w", err)
	}
	if err := ignoreDir(dir); err != nil {
		return nil, fmt.Errorf("keep worktrees directory out of git: %w", err)
	}

	// Clear admin entries for worktrees whose directories are gone, so a
	// crashed run's leftovers do not block the same paths later.
	if err := runGit(context.Background(), root, nil, "worktree", "prune"); err != nil {
		return nil, fmt.Errorf("prune stale worktrees: %w", err)
	}

	p := &GitWorktreeProvider{
		repoRoot:     root,
		worktreesDir: dir,
		commonDir:    commonDir,
		reg:          reg,
		instanceID:   uuid.New().String(),
		leases:       make(map[string]func()),
	}
	// Startup reconciliation (#367): a dead owner's workspace that
	// never produced work is removed; everything else is kept for
	// salvage.
	p.reconcileStartup(context.Background())
	return p, nil
}

// provisionLocks serializes the worktree add step per repository:
// git's .git/config lock makes concurrent worktree add calls fail
// ("could not lock config file") even when they create disjoint
// branches, so the add and its failure cleanup run one at a time per
// repo. Keying by git's common dir means two providers on the same
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

// gitCommonDir returns the repository's canonical git common dir: the
// absolute path, resolved through symlinks so every checkout of the
// same repository — whatever the spelling of its path — resolves to
// the same string. It keys the per-repo provision lock (#452) and the
// per-repo worktrees directory (#383).
func gitCommonDir(repoRoot string) (string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	out, err := gitOutput(context.Background(), root, nil, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s is not a git repository: %w", repoRoot, err)
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return filepath.Clean(dir), nil
}

// WorktreesDir returns the directory dispatch workspaces for the
// repository containing repoRoot are created in:
// <dataDir>/worktrees/<repo-key>, where repo-key is a short stable hash
// of the repository's canonical git common dir (#383). The key keeps
// repositories sharing one data directory apart and gives #367's "this
// repo's entries" a directory boundary. The common dir — not the
// working directory — is hashed, so every checkout of the same
// repository lands in the same key.
func WorktreesDir(dataDir, repoRoot string) (string, error) {
	common, err := gitCommonDir(repoRoot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(common))
	return filepath.Join(dataDir, "worktrees", hex.EncodeToString(sum[:6])), nil
}

// ignoreDir writes a "*" .gitignore beside dir — one level up, so a
// single file covers every repository key sharing the worktrees root —
// when it is missing (#383). Idempotent: a successful Stat means the
// file is already in place, and any other Stat error is fatal.
func ignoreDir(dir string) error {
	ignorePath := filepath.Join(filepath.Dir(dir), ".gitignore")
	if _, err := os.Stat(ignorePath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(ignorePath, []byte("*\n"), 0o644)
}

// Provision implements [WorkspaceProvider]: it creates an isolated
// workspace, a git worktree on the [BranchPrefix]+id branch under the
// provider's worktrees directory, and takes its ownership lease. The
// registry is not touched: the caller builds the [Entry] from the
// placement and registers it, with StatusProvisioned.
func (p *GitWorktreeProvider) Provision(ctx context.Context, id string, opts ProvisionOptions) (Placement, error) {
	base := opts.Base
	if base == "" {
		var err error
		base, err = currentRevision(ctx, p.repoRoot)
		if err != nil {
			return Placement{}, err
		}
	}
	// Defence in depth: a base starting with "-" would be read as a git
	// option further down the line, so refuse it before git sees it.
	if strings.HasPrefix(base, "-") {
		return Placement{}, fmt.Errorf("invalid base %q: must not start with \"-\"", base)
	}
	baseSHA, err := revisionSHA(ctx, p.repoRoot, base)
	if err != nil {
		return Placement{}, fmt.Errorf("resolve base %q: %w", base, err)
	}

	branch := BranchPrefix + id
	path := filepath.Join(p.worktreesDir, branch)

	// The add and its failure cleanup run one at a time per
	// repository, but nothing else in the provision is locked.
	addLock := provisionLock(p.commonDir)
	addLock.Lock()
	if p.provisionHook != nil {
		p.provisionHook(branch, path)
	}
	// The resolved SHA is what worktree add gets, never the raw base
	// string, and "--" ends option parsing before the positionals.
	err = runGit(ctx, p.repoRoot, nil, "worktree", "add", "--no-track", "-b", branch, "--", path, baseSHA)
	if err != nil {
		p.cleanupFailedProvision(ctx, branch, path)
		addLock.Unlock()
		return Placement{}, fmt.Errorf("create worktree: %w", err)
	}
	addLock.Unlock()

	// The lease and marker live next to the worktree, never inside it,
	// because Diff stages the whole workspace with git add -A.
	release, err := lock.TryFile(p.leasePath(branch))
	if err != nil {
		p.removeEntry(ctx, Entry{Path: path, Branch: branch}, nil)
		return Placement{}, fmt.Errorf("take ownership lease: %w", err)
	}
	if err := p.writeOwnerMarker(branch, baseSHA); err != nil {
		release()
		p.removeEntry(ctx, Entry{Path: path, Branch: branch}, nil)
		return Placement{}, fmt.Errorf("write owner marker: %w", err)
	}

	p.mu.Lock()
	p.leases[id] = release
	p.mu.Unlock()

	return Placement{Path: path, Branch: branch, Base: base, BaseSHA: baseSHA}, nil
}

// cleanupFailedProvision removes what a failed worktree add may have
// left behind: the branch -b can create before the add fails, a
// partially created directory, and stale worktree admin entries. It
// runs under the repository's provision lock, and its own failures are
// ignored: the caller returns the original error either way. The
// directory is removed before the branch, and prune runs before
// branch -D: while a worktree admin entry remains, git still regards
// the branch as checked out and refuses to delete it.
func (p *GitWorktreeProvider) cleanupFailedProvision(ctx context.Context, branch, path string) {
	// False positive: branch is generated here from a UUID, so path
	// always stays inside worktreesDir and RemoveAll cannot escape the
	// worktrees directory; the repo root is the directory the client
	// asked the server to open.
	// codeql[go/path-injection]
	_ = os.RemoveAll(path)
	_ = runGit(ctx, p.repoRoot, nil, "worktree", "prune")
	if _, err := gitOutput(ctx, p.repoRoot, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_ = runGit(ctx, p.repoRoot, nil, "branch", "-D", branch)
	}
}

// leasePath is the flock file guarding branch's workspace. It lives
// next to the worktree, never inside it, and the kernel releases the
// lock when the owning process dies — no stale-lock recovery needed.
func (p *GitWorktreeProvider) leasePath(branch string) string {
	return leasePathFor(p.worktreesDir, branch)
}

// leasePathFor is leasePath at an arbitrary worktrees directory: the
// same file name the scan and prune tooling (#369) must agree on.
func leasePathFor(dir, branch string) string {
	return filepath.Join(dir, branch+".lock")
}

// ownerPath is the human-readable owner marker for branch. It carries
// no authority: the lock file is what ownership is proven with.
func (p *GitWorktreeProvider) ownerPath(branch string) string {
	return ownerPathFor(p.worktreesDir, branch)
}

// ownerPathFor is ownerPath at an arbitrary worktrees directory.
func ownerPathFor(dir, branch string) string {
	return filepath.Join(dir, branch+ownerMarkerSuffix)
}

// ownerMarkerSuffix is the filename suffix of an owner marker next to
// its workspace directory.
const ownerMarkerSuffix = ".owner.json"

// ownerMarker records who holds a workspace's lease.
type ownerMarker struct {
	InstanceID string    `json:"instance_id"`
	PID        int       `json:"pid"`
	CreatedAt  time.Time `json:"created_at"`
	// BaseSHA is the commit the workspace's branch was cut from,
	// recorded so startup reconciliation can tell a workless leftover
	// from salvageable work (#367).
	BaseSHA string `json:"base_sha"`
	// Handle and Role are the dispatch's addressable identity (#313),
	// stamped by UpdateOwnerIdentity when the registry assigns the
	// handle, so tooling outside a session can show it (#369).
	Handle string `json:"handle,omitempty"`
	Role   string `json:"role,omitempty"`
	// Disposition is what became of the work (#368): applied or
	// dismissed. Empty means nobody has judged it yet.
	Disposition string `json:"disposition,omitempty"`
}

// writeOwnerMarker atomically records this provider instance as the
// owner of branch, with the base SHA the workspace was cut from.
func (p *GitWorktreeProvider) writeOwnerMarker(branch, baseSHA string) error {
	return writeMarkerFile(p.worktreesDir, branch, ownerMarker{
		InstanceID: p.instanceID,
		PID:        os.Getpid(),
		CreatedAt:  time.Now().UTC(),
		BaseSHA:    baseSHA,
	})
}

// UpdateOwnerIdentity stamps the assigned handle and role onto the
// workspace's owner marker (#369), so list and salvage tooling outside
// a session can show who the dispatch was. The caller must hold the
// workspace's lease — the provider does, for every workspace it
// provisioned. A missing marker is a no-op: the stamp is information,
// never load-bearing, and recreating the marker with only a partial
// record would lose the base SHA the salvage logic keys on.
func (p *GitWorktreeProvider) UpdateOwnerIdentity(id, handle, role string) error {
	return p.updateOwnerMarker(id, func(m *ownerMarker) {
		m.Handle = handle
		m.Role = role
	})
}

// SetDisposition records what became of the workspace's work (#368):
// [DispositionApplied] or [DispositionDismissed]. The caller must hold
// the workspace's lease; a missing marker is a no-op.
func (p *GitWorktreeProvider) SetDisposition(id, disposition string) error {
	return p.updateOwnerMarker(id, func(m *ownerMarker) {
		m.Disposition = disposition
	})
}

// updateOwnerMarker read-modify-writes the owner marker of the
// workspace the registry entry id names. A missing marker is a no-op:
// the stamp is information, not authority.
func (p *GitWorktreeProvider) updateOwnerMarker(id string, mutate func(*ownerMarker)) error {
	e, ok := p.reg.Get(id)
	if !ok {
		return fmt.Errorf("dispatch %q is not registered", id)
	}
	path := p.ownerPath(e.Branch)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read owner marker for %s: %w", e.Branch, err)
	}
	var m ownerMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse owner marker for %s: %w", e.Branch, err)
	}
	mutate(&m)
	return writeMarkerFile(p.worktreesDir, e.Branch, m)
}

// writeMarkerFile atomically writes branch's owner marker into dir,
// the same temp-and-rename writeOwnerMarker has always used.
func writeMarkerFile(dir, branch string, m ownerMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".*.owner.json.tmp")
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
	if err := os.Rename(name, ownerPathFor(dir, branch)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
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
func (p *GitWorktreeProvider) Diff(ctx context.Context, entry Entry) (string, error) {
	if _, err := os.Stat(entry.Path); err != nil {
		return "", fmt.Errorf("workspace directory missing: %w", err)
	}
	if entry.BaseSHA == "" {
		return "", fmt.Errorf("dispatch %q has no recorded base SHA", entry.ID)
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

// Release tears down a dispatched workspace: it removes the worktree,
// deletes its branch, and releases the ownership lease and marker. It
// is idempotent: releasing an unknown or already-released dispatch
// succeeds, and it is safe to call on a dispatch whose directory or
// branch is already gone. The registry entry is the caller's to drop
// with [AgentRegistry.Remove].
func (p *GitWorktreeProvider) Release(ctx context.Context, entry Entry) error {
	p.mu.Lock()
	lease := p.leases[entry.ID]
	delete(p.leases, entry.ID)
	p.mu.Unlock()
	return p.removeEntry(ctx, entry, lease)
}

// Sweep removes every tracked workspace plus every orphaned
// crush-dispatch-* directory whose ownership lease is free (from a
// crashed run whose registry was lost). Tracked entries are dropped
// from the registry as their teardown starts and re-registered when
// the teardown fails, so a failed workspace stays registered — with
// its lease — for a later sweep or Release to retry. Orphans a live
// process still holds — another provider on the same repository — and
// orphans with no lease file at all, whose ownership cannot be proven,
// are left alone (#369 is the explicit cleanup tool for those). It is
// the session-end backstop: nothing dispatch created survives it, and
// no dangling branches or worktrees-directory entries are left behind.
// One stubborn workspace does not stop the sweep: every entry and every
// orphan is attempted, the errors are joined and returned, and a failed
// entry stays registered so a later sweep or Release can retry it.
func (p *GitWorktreeProvider) Sweep(ctx context.Context) error {
	entries := p.reg.List()
	p.mu.Lock()
	type sweepEntry struct {
		entry   Entry
		release func()
	}
	items := make([]sweepEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, sweepEntry{e, p.leases[e.ID]})
		delete(p.leases, e.ID)
	}
	p.mu.Unlock()

	var errs []error
	for _, item := range items {
		p.reg.Remove(item.entry.ID)
		if err := p.removeEntry(ctx, item.entry, item.release); err != nil {
			errs = append(errs, err)
			// The removal failed, so the entry and its lease stay
			// registered for a later sweep or Release to retry.
			p.reg.Register(item.entry)
			p.mu.Lock()
			if item.release != nil {
				p.leases[item.entry.ID] = item.release
			}
			p.mu.Unlock()
		}
	}

	// Orphaned directories: registered names — including entries this
	// sweep just failed, whose removal a later sweep or Release will
	// retry — are not ours to take, so anything left under the
	// worktrees dir with our prefix and an unregistered branch belongs
	// to a run whose registry entry was lost. The directory name is the
	// branch name, so the branch is recoverable from it.
	registered := make(map[string]struct{}, len(items))
	for _, e := range p.reg.List() {
		registered[e.Branch] = struct{}{}
	}

	dirs, err := os.ReadDir(p.worktreesDir)
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
			lockPath := p.leasePath(d.Name())
			if _, err := os.Stat(lockPath); err != nil {
				if os.IsNotExist(err) {
					// Without a lease file ownership cannot be proven,
					// so the directory is not ours to take.
					slog.Debug("Skipping dispatch worktree without a lease file", "path", filepath.Join(p.worktreesDir, d.Name()))
					continue
				}
				errs = append(errs, err)
				continue
			}
			release, err := lock.TryFile(lockPath)
			if err != nil {
				if errors.Is(err, lock.ErrContended) {
					// A live process — another provider on this
					// repository — still owns this workspace.
					slog.Debug("Skipping dispatch worktree with a held lease", "path", filepath.Join(p.worktreesDir, d.Name()))
					continue
				}
				errs = append(errs, err)
				continue
			}
			entry := Entry{
				Path:   filepath.Join(p.worktreesDir, d.Name()),
				Branch: d.Name(),
			}
			if err := p.removeEntry(ctx, entry, release); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := runGit(ctx, p.repoRoot, nil, "worktree", "prune"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// reconcileStartup applies the worktrees directory's owner markers at
// construction (#367): a dead owner's workspace that never produced
// work — no commits ahead of its recorded base and a clean tree — is
// removed. Everything else is kept: pending work from a crashed run
// stays on disk for salvage, and a live owner's entries are never
// touched. Failures are logged and left for a later reconciliation or
// cleanup; startup never fails because leftovers could not be removed.
func (p *GitWorktreeProvider) reconcileStartup(ctx context.Context) {
	// Marker access goes through os.Root: every open is confined to the
	// worktrees directory itself, so neither the directory the path
	// derives from nor a hostile or symlinked marker name can address a
	// file outside — the root rejects traversal and escapes instead of
	// trusting string validation.
	root, err := os.OpenRoot(p.worktreesDir)
	if err != nil {
		slog.Debug("Skipping dispatch startup reconciliation", "worktrees_dir", p.worktreesDir, "error", err)
		return
	}
	defer root.Close()
	markers, err := fs.Glob(root.FS(), "*"+ownerMarkerSuffix)
	if err != nil {
		return
	}
	for _, name := range markers {
		branch := strings.TrimSuffix(name, ownerMarkerSuffix)
		// A marker's file name is the only thing naming the workspace it
		// describes, and that name feeds every path below, so refuse
		// anything that is not a plain dispatch branch name: a stray or
		// malicious file in the worktrees directory cannot point cleanup
		// outside it.
		if !strings.HasPrefix(branch, BranchPrefix) || strings.ContainsAny(branch, `/\`) || strings.Contains(branch, "..") {
			slog.Debug("Skipping dispatch owner marker with unusable name", "name", name)
			continue
		}
		// Confined to the worktrees directory by the os.Root above: a
		// marker whose name escapes it — directly or through a symlink —
		// fails here instead of being read.
		data, err := root.ReadFile(name)
		if err != nil {
			slog.Debug("Skipping unreadable dispatch owner marker", "name", name, "error", err)
			continue
		}
		var m ownerMarker
		if err := json.Unmarshal(data, &m); err != nil {
			slog.Debug("Skipping malformed dispatch owner marker", "name", name, "error", err)
			continue
		}

		// Take the lease: a contended lease belongs to a live process,
		// whose entries are never touched; a free lease means the owner
		// died and the cleanup is ours to apply.
		// The name is validated to BranchPrefix with no separators or
		// traversal above, so leasePath stays inside worktreesDir.
		release, err := lock.TryFile(p.leasePath(branch))
		if err != nil {
			continue
		}
		entry := Entry{
			Path:    filepath.Join(p.worktreesDir, branch),
			Branch:  branch,
			BaseSHA: m.BaseSHA,
		}
		if p.hasWork(ctx, entry) {
			release()
			continue
		}
		if err := p.removeEntry(ctx, entry, release); err != nil {
			slog.Debug("Startup reconciliation left a dispatch workspace in place", "branch", branch, "error", err)
		}
	}
	_ = runGit(ctx, p.repoRoot, nil, "worktree", "prune")
}

// hasWork reports whether the workspace produced work a human might
// want (#367): commits on its branch ahead of the recorded base, or
// uncommitted changes on disk. An unknown base, a branch that no longer
// resolves, or a git probe error count as work — the fail-safe is to
// keep the workspace, never to discard it.
func (p *GitWorktreeProvider) hasWork(ctx context.Context, entry Entry) bool {
	if entry.BaseSHA == "" {
		return true
	}
	out, err := gitOutput(ctx, p.repoRoot, nil, "rev-list", "--count", entry.BaseSHA+".."+entry.Branch)
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

// ReleaseUnworked is the selective exit cleanup (#367): registered
// entries that never produced work are removed with their branches;
// entries with work — committed or just uncommitted — stay on disk for
// salvage and stay registered. A git probe error or an unknown base
// keeps the workspace: the fail-safe is never to discard work a human
// might want. The orphan pass is not part of exit — Sweep owns it —
// and a stubborn entry stays registered so a later ReleaseUnworked or
// Release can retry it. The errors are joined and returned.
func (p *GitWorktreeProvider) ReleaseUnworked(ctx context.Context) error {
	candidates := p.reg.List()

	// The disposability probe runs git commands, so it happens outside
	// the registry and lease locks, like every other git work here; the
	// removal is then claimed, re-checking that the entry survived the
	// probe.
	var doomed []Entry
	for _, entry := range candidates {
		if p.hasWork(ctx, entry) {
			continue
		}
		if !p.reg.Remove(entry.ID) {
			continue
		}
		doomed = append(doomed, entry)
	}

	var errs []error
	for _, entry := range doomed {
		p.mu.Lock()
		release := p.leases[entry.ID]
		delete(p.leases, entry.ID)
		p.mu.Unlock()
		if err := p.removeEntry(ctx, entry, release); err != nil {
			errs = append(errs, err)
			// The removal failed, so the entry and its lease stay
			// registered for a later ReleaseUnworked or Release to retry.
			p.reg.Register(entry)
			p.mu.Lock()
			if release != nil {
				p.leases[entry.ID] = release
			}
			p.mu.Unlock()
		}
	}
	if err := runGit(ctx, p.repoRoot, nil, "worktree", "prune"); err != nil {
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
func (p *GitWorktreeProvider) removeEntry(ctx context.Context, entry Entry, lease func()) error {
	markerPath := ""
	if lease != nil {
		markerPath = p.ownerPath(entry.Branch)
	}
	return removeWorkspaceEntry(ctx, p.repoRoot, entry, markerPath, lease)
}

// removeWorkspaceEntry tears down one workspace on disk: the worktree
// (forced if it is dirty), the branch, and the admin entry. Missing
// artifacts are not errors. The lock file is never unlinked: flock is
// keyed by inode, so removing it can let two processes lock different
// inodes at the same path and both believe they own the workspace.
// When lease is not nil the owner marker is removed — only a lease
// holder may touch ownership artifacts — and the lease is released
// last, after everything else is gone. markerPath is the owner marker
// to remove, empty for none: only a lease holder may pass one.
func removeWorkspaceEntry(ctx context.Context, repoRoot string, entry Entry, markerPath string, lease func()) error {
	// Errors are prefixed with the entry so a joined sweep error says
	// what failed. Orphan entries carry no ID; their branch is the name.
	label := entry.ID
	if label == "" {
		label = entry.Branch
	}
	if entry.Path != "" {
		if err := runGit(ctx, repoRoot, nil, "worktree", "remove", entry.Path); err != nil {
			// A dirty workspace still removes with --force; a missing
			// one is already gone and prune cleans the admin entry.
			if err := runGit(ctx, repoRoot, nil, "worktree", "remove", "--force", entry.Path); err != nil {
				if err := runGit(ctx, repoRoot, nil, "worktree", "prune"); err != nil {
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
		if hasBranch(ctx, repoRoot, entry.Branch) {
			if err := runGit(ctx, repoRoot, nil, "branch", "-D", entry.Branch); err != nil {
				// A concurrent Release may have deleted the branch
				// between the existence check and -D; gone is gone, so
				// only an error on a branch still there is real.
				if hasBranch(ctx, repoRoot, entry.Branch) {
					return fmt.Errorf("dispatch %s: delete branch %s: %w", label, entry.Branch, err)
				}
			}
		}
	}
	if markerPath != "" {
		os.Remove(markerPath)
	}
	if lease != nil {
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
