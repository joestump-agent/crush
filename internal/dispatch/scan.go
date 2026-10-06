package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/lock"
)

// Disposition values recorded on an owner marker (#368): what became of
// a dispatch's work. Applied means a human folded it into the parent
// branch; dismissed means they looked at it and threw it away. Empty
// means nobody has judged the work yet.
const (
	DispositionApplied   = "applied"
	DispositionDismissed = "dismissed"
)

// WorkspaceInfo is one dispatch workspace found on disk, as the
// dispatch list and prune commands (#369) and any other out-of-session
// tooling see it. Everything is read-only observation: the lock probe
// takes and immediately releases a free lease, so scanning never
// changes who owns a workspace.
type WorkspaceInfo struct {
	// ID is the dispatch ID: the branch name minus the [BranchPrefix].
	ID string `json:"id"`
	// Branch is the workspace's dispatch branch name.
	Branch string `json:"branch"`
	// Path is the workspace directory.
	Path string `json:"path"`
	// Handle and Role are the dispatch's addressable identity (#313),
	// recorded on the owner marker by [GitWorktreeProvider.UpdateOwnerIdentity].
	// Both are empty on a marker that predates the stamp or was never
	// assigned one.
	Handle string `json:"handle"`
	Role   string `json:"role"`
	// Disposition is what became of the work (#368): [DispositionApplied]
	// or [DispositionDismissed], empty when unjudged.
	Disposition string `json:"disposition"`
	// HasMarker reports whether an owner marker was found and parsed.
	HasMarker bool `json:"has_marker"`
	// HasLock reports whether a lease file exists. A missing lock file
	// means ownership cannot be proven either way — cleanup tools skip
	// such entries, exactly as [GitWorktreeProvider.Sweep] does.
	HasLock bool `json:"has_lock"`
	// OwnerAlive reports whether the ownership lease is held. It is
	// meaningless without HasLock: an unknown owner is not a dead one.
	OwnerAlive bool `json:"owner_alive"`
	// HasChanges reports work a human might want: commits on the branch
	// ahead of the recorded base, or a dirty tree. A git probe error or
	// an unknown base counts as changes — the fail-safe is to keep the
	// workspace, never to discard it, mirroring the provider's hasWork.
	HasChanges bool `json:"has_changes"`
	// BaseSHA is the commit the branch was cut from, from the marker.
	BaseSHA string `json:"base_sha,omitempty"`
	// InstanceID, PID and CreatedAt identify the recording owner, from
	// the marker.
	InstanceID string    `json:"instance_id,omitempty"`
	PID        int       `json:"pid,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// OwnerAliveState renders the three-way liveness a list column shows:
// yes, no, or unknown when there is no lock file to probe.
func (w WorkspaceInfo) OwnerAliveState() string {
	switch {
	case !w.HasLock:
		return "unknown"
	case w.OwnerAlive:
		return "yes"
	default:
		return "no"
	}
}

// ScanWorkspaces reports every dispatch workspace under dir, the
// worktrees directory [WorktreesDir] resolves for the repository rooted
// at repoRoot. A missing directory is an empty result, not an error: a
// repository with no dispatched agents has nothing to list or prune.
// The scan is read-only apart from one unavoidable side effect of the
// liveness probe — taking and releasing a free lease recreates an
// empty lock file, which is exactly what a live owner would have left.
func ScanWorkspaces(ctx context.Context, dir, repoRoot string) ([]WorkspaceInfo, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []WorkspaceInfo{}, nil
		}
		return nil, fmt.Errorf("open worktrees directory: %w", err)
	}
	defer root.Close()

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		if os.IsNotExist(err) {
			return []WorkspaceInfo{}, nil
		}
		return nil, fmt.Errorf("read worktrees directory: %w", err)
	}

	var infos []WorkspaceInfo
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), BranchPrefix) {
			continue
		}
		info, err := scanWorkspace(ctx, root, dir, e.Name(), repoRoot)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// scanWorkspace observes one workspace directory. The name is
// validated like every marker consumer: a directory whose name is not
// a plain dispatch branch name cannot address anything outside the
// worktrees directory.
func scanWorkspace(ctx context.Context, root *os.Root, dir, branch, repoRoot string) (WorkspaceInfo, error) {
	info := WorkspaceInfo{
		Branch: branch,
		ID:     strings.TrimPrefix(branch, BranchPrefix),
		Path:   filepath.Join(dir, branch),
	}

	if m, ok, err := readOwnerMarker(root, branch); err != nil {
		return WorkspaceInfo{}, err
	} else if ok {
		info.HasMarker = true
		info.Handle = m.Handle
		info.Role = m.Role
		info.Disposition = m.Disposition
		info.BaseSHA = m.BaseSHA
		info.InstanceID = m.InstanceID
		info.PID = m.PID
		info.CreatedAt = m.CreatedAt
	}

	lockPath := leasePathFor(dir, branch)
	if _, err := os.Stat(lockPath); err == nil {
		info.HasLock = true
		release, err := lock.TryFile(lockPath)
		switch {
		case err == nil:
			// The lease is free: whoever wrote the marker is gone.
			info.OwnerAlive = false
			release()
		case errors.Is(err, lock.ErrContended):
			info.OwnerAlive = true
		default:
			return WorkspaceInfo{}, fmt.Errorf("probe ownership lease for %s: %w", branch, err)
		}
	} else if !os.IsNotExist(err) {
		return WorkspaceInfo{}, fmt.Errorf("stat ownership lease for %s: %w", branch, err)
	}

	info.HasChanges = workspaceHasChanges(ctx, repoRoot, info)
	return info, nil
}

// readOwnerMarker reads and parses branch's owner marker confined to
// the worktrees root, reporting ok=false for a missing marker. A
// malformed marker is not an error: it is reported as absent, the same
// stance reconcileStartup takes, because a marker no parseable content
// can be salvaged from is indistinguishable from none.
func readOwnerMarker(root *os.Root, branch string) (ownerMarker, bool, error) {
	var m ownerMarker
	if !validBranchName(branch) {
		return m, false, nil
	}
	// The relative name resolves inside the os.Root, which rejects any
	// escape — the same confinement reconcileStartup relies on.
	data, err := root.ReadFile(branch + ownerMarkerSuffix)
	if err != nil {
		if os.IsNotExist(err) {
			return m, false, nil
		}
		return m, false, fmt.Errorf("read owner marker for %s: %w", branch, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, false, nil
	}
	return m, true, nil
}

// validBranchName refuses anything that is not a plain dispatch branch
// name: no separators, no traversal, no escapes — the name feeds every
// path below, so a stray or hostile name cannot point cleanup outside
// the worktrees directory.
func validBranchName(branch string) bool {
	return strings.HasPrefix(branch, BranchPrefix) &&
		!strings.ContainsAny(branch, `/\`) &&
		!strings.Contains(branch, "..")
}

// workspaceHasChanges reports commits ahead of the recorded base or a
// dirty tree, with the provider's hasWork fail-safe: an unknown base,
// an unresolvable branch, or a git error all count as changes.
func workspaceHasChanges(ctx context.Context, repoRoot string, info WorkspaceInfo) bool {
	if info.BaseSHA == "" {
		return true
	}
	out, err := gitOutput(ctx, repoRoot, nil, "rev-list", "--count", info.BaseSHA+".."+info.Branch)
	if err != nil {
		return true
	}
	if strings.TrimSpace(string(out)) != "0" {
		return true
	}
	out, err = gitOutput(ctx, info.Path, nil, "status", "--porcelain")
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(out)) != ""
}

// WorkspacePruner removes dispatch workspaces for tooling outside a
// session (#369): the same teardown the provider performs, without a
// provider construction's startup reconciliation — a prune command
// must execute exactly the plan it printed, and a dry run must touch
// nothing at all.
type WorkspacePruner struct {
	repoRoot     string
	worktreesDir string
}

// NewWorkspacePruner validates that repoRoot is a git repository and
// returns a pruner removing workspaces under worktreesDir.
func NewWorkspacePruner(repoRoot, worktreesDir string) (*WorkspacePruner, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	if err := runGit(context.Background(), root, nil, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", repoRoot, err)
	}
	dir, err := filepath.Abs(worktreesDir)
	if err != nil {
		return nil, err
	}
	return &WorkspacePruner{repoRoot: root, worktreesDir: dir}, nil
}

// Remove tears one scanned workspace down: worktree, branch, owner
// marker and lease. It refuses a workspace whose owner is alive — the
// lease is re-acquired at removal time, so a workspace whose owner died
// between scan and prune is still removed, and one whose owner appeared
// in between is refused. A workspace with no lock file is also refused:
// unprovable ownership is not ours to take, matching Sweep. Missing
// artifacts are not errors; like [GitWorktreeProvider.Release], removal
// is idempotent.
func (w *WorkspacePruner) Remove(ctx context.Context, info WorkspaceInfo) error {
	if !info.HasLock {
		return fmt.Errorf("refusing to remove %s: no lock file, ownership cannot be proven", info.Branch)
	}
	if info.OwnerAlive {
		return fmt.Errorf("refusing to remove %s: owner is alive", info.Branch)
	}
	release, err := lock.TryFile(leasePathFor(w.worktreesDir, info.Branch))
	if err != nil {
		if errors.Is(err, lock.ErrContended) {
			return fmt.Errorf("refusing to remove %s: owner is alive", info.Branch)
		}
		return fmt.Errorf("take ownership lease for %s: %w", info.Branch, err)
	}
	return removeWorkspaceEntry(ctx, w.repoRoot, Entry{
		Path:    info.Path,
		Branch:  info.Branch,
		BaseSHA: info.BaseSHA,
	}, ownerPathFor(w.worktreesDir, info.Branch), release)
}
