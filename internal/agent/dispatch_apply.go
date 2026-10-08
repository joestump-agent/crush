package agent

// Act on a finished dispatch (#368): apply_dispatch brings its work into
// the parent checkout (merge, squash, or cherry-pick) and removes the
// workspace; dismiss_dispatch discards it. Together they are the model's
// front door for the review decision the terminal message asks for, and
// they replace hand-run git — a hand merge carries only commits, losing
// the uncommitted work the dispatch template counts as part of the
// product, and a hand worktree remove leaves the registry entry and the
// handle behind.

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/permission"
)

//go:embed templates/apply_dispatch.md
var applyDispatchToolDescription string

//go:embed templates/dismiss_dispatch.md
var dismissDispatchToolDescription string

// ApplyDispatchToolName is the registered name of the ApplyDispatch tool.
const ApplyDispatchToolName = "apply_dispatch"

// DismissDispatchToolName is the registered name of the DismissDispatch tool.
const DismissDispatchToolName = "dismiss_dispatch"

// ApplyDispatchParams are the ApplyDispatch tool's arguments. Address the
// dispatch by ID or by its @handle (#313); exactly one is needed.
type ApplyDispatchParams struct {
	// DispatchID is the dispatch identifier — the "dispatch_id" field of
	// the dispatch handle the dispatch_agent tool returned.
	DispatchID string `json:"dispatch_id,omitempty" description:"Dispatch ID of the finished dispatched agent (the \"dispatch_id\" from its dispatch handle)"`
	// Handle is the dispatched agent's @handle (#313) — the "handle"
	// field of the dispatch handle.
	Handle string `json:"handle,omitempty" description:"@handle of the finished dispatched agent (the \"handle\" from its dispatch handle)"`
	// Mode is how the work lands in the parent checkout: "merge" (the
	// default), "squash", or "cherry-pick".
	Mode string `json:"mode,omitempty" description:"How to bring the work in: \"merge\" (default), \"squash\" (staged, not committed), or \"cherry-pick\""`
}

// DismissDispatchParams are the DismissDispatch tool's arguments. Address
// the dispatch by ID or by its @handle (#313); exactly one is needed.
type DismissDispatchParams struct {
	// DispatchID is the dispatch identifier — the "dispatch_id" field of
	// the dispatch handle the dispatch_agent tool returned.
	DispatchID string `json:"dispatch_id,omitempty" description:"Dispatch ID of the finished dispatched agent (the \"dispatch_id\" from its dispatch handle)"`
	// Handle is the dispatched agent's @handle (#313) — the "handle"
	// field of the dispatch handle.
	Handle string `json:"handle,omitempty" description:"@handle of the finished dispatched agent (the \"handle\" from its dispatch handle)"`
}

func (p ApplyDispatchParams) addresses() (string, string) { return p.DispatchID, p.Handle }

func (p DismissDispatchParams) addresses() (string, string) { return p.DispatchID, p.Handle }

// The registry-sourced refs are pinned to the shapes git itself allows
// before they reach a command line: a branch with no leading dash (git
// would read it as an option), no ".." range, and a base that is nothing
// but a hex object name. A provisioned workspace always satisfies both;
// anything else refuses instead of running.
var (
	dispatchBranchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./_-]*$`)
	dispatchSHAPattern    = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)
)

// validateDispatchRef refuses a registry entry whose branch or base SHA
// is not a plain refname or object name.
func validateDispatchRef(entry dispatch.Entry) error {
	if !dispatchBranchPattern.MatchString(entry.Branch) || strings.Contains(entry.Branch, "..") {
		return fmt.Errorf("dispatch %s carries an unusable branch %q", entry.ID, entry.Branch)
	}
	if !dispatchSHAPattern.MatchString(entry.BaseSHA) {
		return fmt.Errorf("dispatch %s carries an unusable base SHA %q", entry.ID, entry.BaseSHA)
	}
	return nil
}

// pathInside reports whether path sits within dir, so a path assembled
// from subprocess output can only ever point back into the repository.
func pathInside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveFinishedDispatch resolves the dispatch an address pair names and
// enforces the shared preconditions of apply and dismiss: exactly one
// address, a known dispatch, and a finished one — a running dispatch
// refuses with the cancel hint, so neither tool ever races a live run.
func (c *coordinator) resolveFinishedDispatch(id, handle string) (dispatch.Entry, error) {
	if (id == "") == (handle == "") {
		return dispatch.Entry{}, fmt.Errorf("provide dispatch_id or handle, exactly one")
	}
	entry, ok := c.dispatchRegistry().Get(id)
	if !ok {
		entry, ok = c.dispatchRegistry().ByHandle(dispatch.HandleSlug(handle))
	}
	if !ok {
		ref := id
		if ref == "" {
			ref = handle
		}
		return dispatch.Entry{}, fmt.Errorf("no dispatch %q is known; dispatch one first", ref)
	}
	if !entry.Status.IsTerminal() {
		return dispatch.Entry{}, fmt.Errorf("dispatch %s is still running; cancel it first", entry.ID)
	}
	// An external agent ran remotely (#434): it wrote nothing here, so
	// there is no workspace or branch to bring in or throw away.
	if entry.Source != "" {
		return dispatch.Entry{}, fmt.Errorf("dispatch %s ran on an external agent: nothing was written to disk, so there is no workspace or branch to apply or dismiss", entry.ID)
	}
	if err := validateDispatchRef(entry); err != nil {
		return dispatch.Entry{}, err
	}
	return entry, nil
}

// requestDispatchPermission asks the parent's permission service before
// either tool changes anything: the tool name is the calling tool and
// the path is the parent repository root, the thing about to change.
func (c *coordinator) requestDispatchPermission(ctx context.Context, toolName, description string, call fantasy.ToolCall) (fantasy.ToolResponse, bool) {
	granted, err := c.permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:   tools.GetSessionFromContext(ctx),
		ToolCallID:  call.ID,
		ToolName:    toolName,
		Action:      "apply",
		Description: description,
		Path:        c.cfg.WorkingDir(),
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("permission check failed: %s", err)), false
	}
	if !granted {
		return tools.NewPermissionDeniedResponse(), false
	}
	return fantasy.ToolResponse{}, true
}

// git runs one git command in dir and returns its combined output on
// failure. The user's own git config is used throughout — no env
// overrides — so anything committed here is the user's.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// gitQuiet is git for calls whose failure carries no useful message.
func gitQuiet(ctx context.Context, dir string, args ...string) bool {
	_, err := git(ctx, dir, args...)
	return err == nil
}

// parentCheckoutBusy reports whether the parent repository cannot start
// an apply, and why: uncommitted changes, or a merge, rebase, revert, or
// cherry-pick already in progress. A previous operation's state must
// never be silently reset by a dispatch apply.
func parentCheckoutBusy(ctx context.Context, repoRoot string) (string, bool) {
	status, err := git(ctx, repoRoot, "status", "--porcelain")
	if err != nil {
		return fmt.Sprintf("could not read the checkout status: %s", err), true
	}
	if strings.TrimSpace(status) != "" {
		return "the checkout is dirty; commit or stash your changes first", true
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		out, err := git(ctx, repoRoot, "rev-parse", "--git-path", marker)
		if err != nil {
			continue
		}
		path := strings.TrimSpace(out)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoRoot, path)
		}
		if !pathInside(repoRoot, path) {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Sprintf("a merge, rebase, or cherry-pick is already in progress (%s); finish or abort it first", marker), true
		}
	}
	return "", false
}

// abortDispatchConflict backs a failed apply out and reports the
// conflicting paths. The unmerged paths are collected before anything is
// aborted — aborting erases them. merge --abort and cherry-pick --abort
// each only work for their own kind of conflict; a conflicted squash
// merge records neither MERGE_HEAD nor CHERRY_PICK_HEAD, so both refuse,
// and the hard reset is what restores the pre-merge state there. That
// reset only runs on the abort path, after the checkout was verified
// clean, so it restores exactly what was there.
func abortDispatchConflict(ctx context.Context, repoRoot string) []string {
	out, _ := git(ctx, repoRoot, "diff", "--name-only", "--diff-filter=U")
	var conflicts []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			conflicts = append(conflicts, line)
		}
	}
	if gitQuiet(ctx, repoRoot, "merge", "--abort") {
		return conflicts
	}
	if gitQuiet(ctx, repoRoot, "cherry-pick", "--abort") {
		return conflicts
	}
	_ = gitQuiet(ctx, repoRoot, "reset", "--hard", "HEAD")
	return conflicts
}

// ApplyDispatch brings a finished dispatch's work into the parent
// checkout and removes its workspace (#368). The workspace's uncommitted
// changes are committed on the dispatch branch first — the dispatch
// template counts them as part of the work product — then the chosen
// mode lands the branch: merge (a --no-ff merge commit), squash (staged,
// left for the caller to commit), or cherry-pick (the commits since the
// dispatch's base). A conflict aborts with the parent exactly as it was
// and the conflicting paths reported; on success the dispatch is marked
// applied and its workspace, branch, and registry entry are removed.
func (c *coordinator) ApplyDispatch(ctx context.Context, entry dispatch.Entry, mode string) (string, error) {
	repoRoot := c.cfg.WorkingDir()
	if reason, busy := parentCheckoutBusy(ctx, repoRoot); busy {
		return "", fmt.Errorf("cannot apply dispatch %s: %s", entry.ID, reason)
	}

	// Uncommitted workspace changes are part of the work product
	// (templates/dispatch.md.tpl): commit them on the dispatch branch so
	// every mode carries them. The worktree shares the parent's object
	// store, so the new commit is immediately visible to the merge.
	status, err := git(ctx, entry.Path, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("read workspace status: %s", err)
	}
	uncommitted := strings.TrimSpace(status) != ""
	if uncommitted {
		if out, err := git(ctx, entry.Path, "add", "-A"); err != nil {
			return "", fmt.Errorf("stage workspace changes: %s", strings.TrimSpace(out))
		}
		if out, err := git(ctx, entry.Path, "commit", "-m", fmt.Sprintf("crush-dispatch %s: uncommitted work", entry.ID)); err != nil {
			return "", fmt.Errorf("commit workspace changes: %s", strings.TrimSpace(out))
		}
	}

	// Land the work in the chosen mode. A failing merge, squash, or
	// cherry-pick is treated as a conflict first: collect the unmerged
	// paths, abort, and leave the parent exactly as it was.
	var applyErr error
	switch mode {
	case "squash":
		_, applyErr = git(ctx, repoRoot, "merge", "--squash", entry.Branch)
	case "cherry-pick":
		_, applyErr = git(ctx, repoRoot, "cherry-pick", entry.BaseSHA+".."+entry.Branch)
	default:
		_, applyErr = git(ctx, repoRoot, "merge", "--no-ff", "--no-edit", entry.Branch)
	}
	if applyErr != nil {
		conflicts := abortDispatchConflict(ctx, repoRoot)
		if len(conflicts) > 0 {
			return "", fmt.Errorf("dispatch %s conflicts with your checkout; nothing was changed, conflicting paths: %s",
				entry.ID, strings.Join(conflicts, ", "))
		}
		return "", fmt.Errorf("apply dispatch %s: %s", entry.ID, applyErr)
	}

	if err := c.settleDispatch(ctx, entry, dispatch.DispositionApplied); err != nil {
		return "", err
	}

	var summary string
	switch mode {
	case "squash":
		summary = fmt.Sprintf("Dispatch %s applied as a squash merge: the work from %s is staged in your checkout but NOT committed — review it and commit it yourself. The workspace and its branch are removed.", entry.ID, entry.Branch)
	case "cherry-pick":
		summary = fmt.Sprintf("Dispatch %s applied: the commits since its base were cherry-picked from %s into your checkout. The workspace and its branch are removed.", entry.ID, entry.Branch)
	default:
		summary = fmt.Sprintf("Dispatch %s applied: %s was merged into your checkout with a merge commit. The workspace and its branch are removed.", entry.ID, entry.Branch)
	}
	if uncommitted {
		summary += " The workspace's uncommitted changes were committed on the dispatch branch first, so they are part of what landed."
	}
	return summary, nil
}

// DismissDispatch discards a finished dispatch's work and removes its
// workspace (#368): worktree, branch, registry entry — which frees the
// handle. The disposition is recorded before the teardown, so exit
// release and the list/prune CLI see a decided workspace even if the
// teardown below cannot finish.
func (c *coordinator) DismissDispatch(ctx context.Context, entry dispatch.Entry) (string, error) {
	if err := c.settleDispatch(ctx, entry, dispatch.DispositionDismissed); err != nil {
		return "", err
	}
	return fmt.Sprintf("Dispatch %s dismissed: the worktree, the %s branch, and the registry entry are removed, and the handle is freed. The work is discarded.", entry.ID, entry.Branch), nil
}

// settleDispatch records the decision on the owner marker — best-effort,
// the teardown is what actually removes the workspace — and tears the
// workspace down: registry entry, handle, worktree, branch.
func (c *coordinator) settleDispatch(ctx context.Context, entry dispatch.Entry, disposition string) error {
	provider, err := c.dispatchWorkspaceProvider()
	if err != nil {
		return fmt.Errorf("workspace provider unavailable: %s", err)
	}
	if err := provider.SetDisposition(entry.ID, disposition); err != nil {
		slog.Warn("Failed to record dispatch disposition", "dispatch_id", entry.ID, "disposition", disposition, "error", err)
	}
	c.dispatchRegistry().Remove(entry.ID)
	if err := provider.Release(ctx, entry); err != nil {
		return fmt.Errorf("remove workspace: %s", err)
	}
	return nil
}

// applyDispatchTool builds the ApplyDispatch tool (#368): the model's
// front door for bringing a finished dispatch's work in, next to the
// dispatch_agent tool that starts them. Resolution and the permission
// ask live in the handler so a refused or denied call never touches git.
func (c *coordinator) applyDispatchTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ApplyDispatchToolName,
		applyDispatchToolDescription,
		func(ctx context.Context, params ApplyDispatchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			mode := params.Mode
			switch mode {
			case "":
				mode = "merge"
			case "merge", "squash", "cherry-pick":
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid mode %q: must be \"merge\", \"squash\", or \"cherry-pick\"", params.Mode)), nil
			}
			entry, err := c.resolveFinishedDispatch(params.DispatchID, params.Handle)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if resp, ok := c.requestDispatchPermission(ctx, ApplyDispatchToolName,
				fmt.Sprintf("Apply dispatch %s (branch %s) into your checkout via %s", entry.ID, entry.Branch, mode), call); !ok {
				return resp, nil
			}
			summary, err := c.ApplyDispatch(ctx, entry, mode)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(summary), nil
		},
	)
}

// dismissDispatchTool builds the DismissDispatch tool (#368): the model's
// front door for discarding a finished dispatch's work and freeing its
// workspace. Resolution and the permission ask live in the handler so a
// refused or denied call never touches the workspace.
func (c *coordinator) dismissDispatchTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		DismissDispatchToolName,
		dismissDispatchToolDescription,
		func(ctx context.Context, params DismissDispatchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			entry, err := c.resolveFinishedDispatch(params.DispatchID, params.Handle)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if resp, ok := c.requestDispatchPermission(ctx, DismissDispatchToolName,
				fmt.Sprintf("Dismiss dispatch %s (branch %s): discard its work and remove its workspace", entry.ID, entry.Branch), call); !ok {
				return resp, nil
			}
			summary, err := c.DismissDispatch(ctx, entry)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(summary), nil
		},
	)
}
