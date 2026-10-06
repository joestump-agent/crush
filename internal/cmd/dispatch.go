package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// dispatchCmd is the out-of-session surface for dispatched agent
// workspaces (#369): workspaces survive exit since #367, and outside a
// session there was no way to see or clean them short of raw git
// plumbing against the worktrees directory.
var dispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Inspect and clean up dispatched agent workspaces",
	Long: `Inspect and clean up dispatched agent workspaces.

Dispatched agents run in isolated git worktrees that can outlive the
Crush process that spawned them. These commands list those workspaces
and prune the ones no live process owns.`,
}

var dispatchListCmd = &cobra.Command{
	Use:   "list",
	Short: "List dispatched agent workspaces",
	Long: `List dispatched agent workspaces.

One row per workspace: id, handle, branch, disposition, whether the
owning process is still alive, and whether the workspace holds changes
(commits ahead of its base, or a dirty tree).`,
	Example: `
# List dispatch workspaces in a table
crush dispatch list

# List as JSON
crush dispatch list --json
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		jsonOutput, _ := cmd.Flags().GetBool("json")

		_, _, infos, err := resolveDispatchWorkspaces(cmd)
		if err != nil {
			return err
		}

		if jsonOutput {
			data, err := json.Marshal(infos)
			if err != nil {
				return fmt.Errorf("marshal workspaces: %w", err)
			}
			cmd.Println(string(data))
			return nil
		}

		if len(infos) == 0 {
			return nil
		}

		if term.IsTerminal(os.Stdout.Fd()) {
			t := table.New().
				Border(lipgloss.RoundedBorder()).
				StyleFunc(func(row, col int) lipgloss.Style {
					return lipgloss.NewStyle().Padding(0, 1)
				}).
				Headers("ID", "Handle", "Branch", "Disposition", "Owner", "Changes")
			for _, w := range infos {
				t.Row(w.ID, w.Handle, w.Branch, w.Disposition, w.OwnerAliveState(), hasChangesLabel(w))
			}
			lipgloss.Println(t)
			return nil
		}

		for _, w := range infos {
			cmd.Printf("%s\t%s\t%s\t%s\t%s\t%s\n",
				w.ID, w.Handle, w.Branch, w.Disposition, w.OwnerAliveState(), hasChangesLabel(w))
		}
		return nil
	},
}

func hasChangesLabel(w dispatch.WorkspaceInfo) string {
	if w.HasChanges {
		return "yes"
	}
	return "no"
}

var dispatchPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove dispatched agent workspaces their owner no longer holds",
	Long: `Remove dispatched agent workspaces their owner no longer holds.

A workspace whose owning process is alive is never removed, whatever
the flags. By default only dead-owner workspaces whose disposition is
applied or dismissed go; --all-dead widens that to every dead owner,
skipping workspaces that hold changes unless --force is also given.
--dry-run prints the plan and deletes nothing.`,
	Example: `
# Remove dead-owner workspaces that were applied or dismissed
crush dispatch prune

# Show the plan without deleting anything
crush dispatch prune --dry-run

# Remove every dead-owner workspace, but keep ones holding changes
crush dispatch prune --all-dead

# Also remove dead-owner workspaces that hold changes
crush dispatch prune --all-dead --force
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		allDead, _ := cmd.Flags().GetBool("all-dead")
		force, _ := cmd.Flags().GetBool("force")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		dismissed, _ := cmd.Flags().GetBool("dismissed")

		cfg, dir, infos, err := resolveDispatchWorkspaces(cmd)
		if err != nil {
			return err
		}

		for _, w := range infos {
			remove, reason := pruneDecision(w, allDead, dismissed, force)
			if !remove {
				cmd.Printf("skipped %s: %s\n", w.Branch, reason)
				continue
			}
			if dryRun {
				cmd.Printf("would remove %s\n", w.Branch)
				continue
			}
			if err := pruneWorkspace(cmd, cfg.WorkingDir(), dir, w); err != nil {
				cmd.Printf("failed %s: %s\n", w.Branch, err)
				continue
			}
			cmd.Printf("removed %s\n", w.Branch)
		}
		return nil
	},
}

// pruneDecision decides one scanned workspace's fate: remove or skip,
// with the reason printed either way. A live owner is never removed;
// an unprovable owner (no lock file) never either. The default mode
// takes only dead owners judged applied or dismissed; --all-dead takes
// every dead owner, keeping ones with changes unless --force.
func pruneDecision(w dispatch.WorkspaceInfo, allDead, dismissed, force bool) (bool, string) {
	if !w.HasLock {
		return false, "no lock file; ownership cannot be proven"
	}
	if w.OwnerAlive {
		return false, "owner is alive"
	}
	if allDead {
		if w.HasChanges && !force {
			return false, "has unapplied changes (use --force to remove anyway)"
		}
		return true, ""
	}
	if !w.HasMarker {
		return false, "no owner marker (use --all-dead to remove unmarked entries)"
	}
	switch w.Disposition {
	case dispatch.DispositionApplied, dispatch.DispositionDismissed:
		return true, ""
	default:
		return false, "work not yet applied or dismissed (use --all-dead to remove regardless)"
	}
}

// pruneWorkspace removes one workspace, with the command name in the
// error so a mixed remove/skip run says which entry failed.
func pruneWorkspace(cmd *cobra.Command, repoRoot, dir string, w dispatch.WorkspaceInfo) error {
	pruner, err := dispatch.NewWorkspacePruner(repoRoot, dir)
	if err != nil {
		return err
	}
	if err := pruner.Remove(cmd.Context(), w); err != nil {
		return fmt.Errorf("dispatch %s: %w", w.Branch, err)
	}
	return nil
}

// resolveDispatchWorkspaces loads config for the invocation's cwd and
// data-dir flags, resolves the repository's dispatch worktrees
// directory the same way the coordinator does, and scans it. A missing
// directory scans as empty: a repository with no dispatched agents has
// nothing to list or prune. Outside a git repository the resolution
// fails with git's own error.
func resolveDispatchWorkspaces(cmd *cobra.Command) (*config.ConfigStore, string, []dispatch.WorkspaceInfo, error) {
	cwd, _ := cmd.Flags().GetString("cwd")
	dataDir, _ := cmd.Flags().GetString("data-dir")

	cfg, err := config.Load(cwd, dataDir, false)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to load configuration: %w", err)
	}
	dir, err := dispatch.WorktreesDir(cfg.Config().Options.DataDirectory, cfg.WorkingDir())
	if err != nil {
		return nil, "", nil, fmt.Errorf("resolve dispatch worktrees directory: %w", err)
	}
	infos, err := dispatch.ScanWorkspaces(cmd.Context(), dir, cfg.WorkingDir())
	if err != nil {
		return nil, "", nil, err
	}
	return cfg, dir, infos, nil
}

func init() {
	dispatchListCmd.Flags().Bool("json", false, "Output as JSON")
	dispatchPruneCmd.Flags().Bool("dismissed", true, "Remove dead-owner workspaces applied or dismissed (#368 dispositions)")
	dispatchPruneCmd.Flags().Bool("all-dead", false, "Remove every dead-owner workspace, keeping ones with changes unless --force")
	dispatchPruneCmd.Flags().Bool("force", false, "With --all-dead, also remove dead-owner workspaces holding changes")
	dispatchPruneCmd.Flags().Bool("dry-run", false, "Print the plan and delete nothing")

	dispatchCmd.AddCommand(dispatchListCmd, dispatchPruneCmd)
}
