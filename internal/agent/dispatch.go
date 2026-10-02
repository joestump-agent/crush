package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
)

// ErrNoWorkingDir is returned by BuildDispatchToolchain when no workspace
// directory was given.
var ErrNoWorkingDir = errors.New("dispatch toolchain requires a workspace directory")

// DispatchToolchain is the isolated toolset for an agent dispatched to an
// isolated workspace directory (Worktree Dispatch epic, #62).
//
// Every path-rooted tool (bash, edit, multi-edit, write, view, glob, grep,
// ls, fetch, download) is constructed against the workspace directory, and
// the LSP manager and permission service are scoped to the same directory,
// so a SessionAgent built from this toolchain (#64) can only address files
// inside its workspace through its tools. The isolation boundary is the
// toolset, not the call: SessionAgentCall is unchanged, and an agent built
// the usual way — via buildTools against the workspace root — behaves
// exactly as before.
//
// Deliberately absent, because each would reach back across the isolation
// boundary:
//
//   - the agent and agentic_fetch tools, which build sub-agents from the
//     parent coordinator's scope;
//   - the question tool, since dispatched runs are non-interactive;
//   - MCP tools, whose clients are rooted at the parent workspace;
//   - semantic search, whose index is rooted at the parent workspace.
//
// Phase 1 is in-process: the parent's session, history, and file-tracker
// services are shared (they are keyed by session ID, not path), and the
// scoped permission service bridges to the parent's so a dispatched
// agent's permission requests surface in the same approval flow as the
// main agent's.
type DispatchToolchain struct {
	workingDir  string
	store       *config.ConfigStore
	lspManager  *lsp.Manager
	permissions permission.Service
	tools       []fantasy.AgentTool

	// cancel stops the permission bridge. Nil when the toolchain was
	// built without a parent permission service to bridge to.
	cancel context.CancelFunc
}

// WorkingDir returns the isolated workspace directory the toolchain is
// rooted at.
func (t *DispatchToolchain) WorkingDir() string {
	return t.workingDir
}

// Config returns the scoped config store the toolchain was built from —
// rooted at the workspace directory, with the workspace's own project
// config, LSP config, and skills paths.
func (t *DispatchToolchain) Config() *config.ConfigStore {
	return t.store
}

// LSPManager returns the LSP manager scoped to the workspace directory.
// Clients start lazily, rooted at the workspace.
func (t *DispatchToolchain) LSPManager() *lsp.Manager {
	return t.lspManager
}

// Permissions returns the permission service scoped to the workspace
// directory.
func (t *DispatchToolchain) Permissions() permission.Service {
	return t.permissions
}

// Tools returns the constructed tool set, filtered by the task agent's
// AllowedTools.
func (t *DispatchToolchain) Tools() []fantasy.AgentTool {
	return t.tools
}

// Close tears the toolchain down: it stops the permission bridge and
// shuts down every LSP client the scoped manager started. It is safe to
// call more than once and on a nil toolchain.
func (t *DispatchToolchain) Close(ctx context.Context) {
	if t == nil {
		return
	}
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	if t.lspManager != nil {
		t.lspManager.StopAll(ctx)
	}
}

// DispatchToolchainOptions configures BuildDispatchToolchain.
type DispatchToolchainOptions struct {
	// WorkingDir is the isolated workspace directory the toolchain is
	// rooted at — the path Workspace (#63) provisions. Required.
	WorkingDir string
}

// BuildDispatchToolchain builds a dispatched agent's entire toolchain
// rooted at opts.WorkingDir: the file/bash tools are constructed with
// workingDir = the workspace path, and the LSP manager and permission
// service are rooted there too. A scoped config store is loaded from the
// workspace directory (picking up its project config), while the parent's
// data directory is reused so no second database or lock is taken.
//
// #64's DispatchAgent tool consumes this: provision a clean workspace,
// bootstrap the toolchain against its path, run.
func (c *coordinator) BuildDispatchToolchain(ctx context.Context, opts DispatchToolchainOptions) (*DispatchToolchain, error) {
	if opts.WorkingDir == "" {
		return nil, ErrNoWorkingDir
	}
	dir, err := filepath.Abs(opts.WorkingDir)
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("dispatch working directory %q does not exist", opts.WorkingDir)
	}

	agentCfg, ok := c.cfg.Config().Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}

	// Scoped config: config loading, LSP config, and skills paths resolve
	// against the dispatched workspace. The parent's data directory is
	// passed through so logs, spill files, and the database stay shared.
	scoped, err := config.Load(dir, c.cfg.Config().Options.DataDirectory, c.cfg.Config().Options.Debug)
	if err != nil {
		return nil, err
	}

	lspManager := lsp.NewManager(scoped)

	// Scoped permissions, mirroring app.New's construction: rooted at the
	// workspace directory, inheriting the parent's skip setting and
	// allowed-tools so a dispatched agent starts from the same policy.
	skip := c.cfg.Overrides().SkipPermissionRequests
	var allowedTools []string
	if scoped.Config().Permissions != nil && scoped.Config().Permissions.AllowedTools != nil {
		allowedTools = scoped.Config().Permissions.AllowedTools
	}
	permissions := permission.NewPermissionService(dir, skip, allowedTools)

	var cancel context.CancelFunc
	if c.permissions != nil {
		cancel = bridgePermissions(ctx, c.permissions, permissions)
	}

	t := &DispatchToolchain{
		workingDir:  dir,
		store:       scoped,
		lspManager:  lspManager,
		permissions: permissions,
		cancel:      cancel,
	}
	t.tools = c.buildDispatchTools(agentCfg, t)
	return t, nil
}

// dispatchWriteTools are the tools every dispatched agent gets on top of
// the task agent's AllowedTools (#64). The task agent's default set is
// deliberately read-only — it exists to answer research prompts — but a
// dispatch's whole point is producing work, so bash, the edit tools, and
// write are non-negotiable, and the todo enforcement ladder
// (interaction model, #315) needs the todos tool. The union keeps
// everything the task agent was already allowed: narrowing via config
// still works for read tools, it just cannot remove write capability
// from a dispatch.
var dispatchWriteTools = []string{
	tools.BashToolName,
	tools.EditToolName,
	tools.MultiEditToolName,
	tools.WriteToolName,
	tools.TodosToolName,
}

// buildDispatchTools constructs the dispatched agent's tools against the
// scoped toolchain, mirroring buildTools for everything that is rooted at
// a directory. The parent's session-scoped services (todos) and data-dir
// services (logs, jobs) are reused; every path-resolving tool gets the
// workspace directory.
func (c *coordinator) buildDispatchTools(agentCfg config.Agent, t *DispatchToolchain) []fantasy.AgentTool {
	scoped := t.store.Config()
	dir := t.workingDir

	// Model ID for the bash tool's error attribution, same derivation as
	// buildTools.
	modelID := ""
	if modelCfg, ok := scoped.Models[agentCfg.Model]; ok {
		if model := scoped.GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	// Effective bash allow-list, same merge as buildTools: the workspace
	// config's list plus the parent store's runtime overrides
	// (--allow-commands / --allow-all-commands).
	bashOverrides := c.cfg.Overrides()
	allowedCommands := append(append([]string{}, scoped.Options.AllowedCommands...), bashOverrides.AllowedCommands...)
	allowAllCommands := scoped.Options.AllowAllCommands || bashOverrides.AllowAllCommands

	dataDir := scoped.Options.DataDirectory
	logFile := filepath.Join(dataDir, "logs", "crush.log")

	allTools := []fantasy.AgentTool{
		tools.NewBashTool(t.permissions, dir, dataDir, scoped.Options.Attribution, modelID, allowedCommands, allowAllCommands),
		tools.NewCrushLogsTool(logFile),
		tools.NewJobOutputTool(dataDir),
		tools.NewJobKillTool(),
		tools.NewDownloadTool(t.permissions, dir, nil),
		tools.NewEditTool(t.lspManager, t.permissions, c.history, c.filetracker, dir),
		tools.NewMultiEditTool(t.lspManager, t.permissions, c.history, c.filetracker, dir),
		tools.NewFetchTool(t.permissions, dir, nil),
		tools.NewGlobTool(dir, scoped.Tools.Glob),
		tools.NewGrepTool(dir, scoped.Tools.Grep),
		tools.NewLsTool(t.permissions, dir, scoped.Tools.Ls),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(t.lspManager, t.permissions, c.filetracker, c.skillTracker, dir, scoped.Options.SkillsPaths...),
		tools.NewWriteTool(t.lspManager, t.permissions, c.history, c.filetracker, dir),
	}

	// LSP tools under the same registration condition as buildTools, but
	// against the scoped manager so they resolve files inside the
	// workspace.
	if len(scoped.LSP) > 0 || scoped.Options.AutoLSP == nil || *scoped.Options.AutoLSP {
		allTools = append(
			allTools,
			tools.NewDiagnosticsTool(t.lspManager),
			tools.NewReferencesTool(t.lspManager),
			tools.NewLSPRestartTool(t.lspManager),
			tools.NewSymbolsTool(t.lspManager),
			tools.NewDefinitionTool(t.lspManager),
			tools.NewCallHierarchyTool(t.lspManager),
			tools.NewRenameTool(t.lspManager, t.permissions, c.history, c.filetracker),
			tools.NewReplaceSymbolTool(t.lspManager, t.permissions, c.history, c.filetracker),
		)
	}

	// The task agent's set widened with the dispatch write tools: a
	// dispatched agent must be able to edit, not just read (#64).
	allowed := slices.Concat(agentCfg.AllowedTools, dispatchWriteTools)

	var filtered []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(allowed, tool.Info().Name) {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

// bridgePermissions forwards permission requests raised inside a dispatch
// scope to the parent workspace's permission service, so dispatched-agent
// tool calls surface in the same approval flow as the main agent's. Each
// forwarded request resolves the scoped service's pending request with
// the parent's verdict. The bridge runs until cancel is called; requests
// still waiting when the bridge stops are denied by the canceled context.
func bridgePermissions(ctx context.Context, parent, scoped permission.Service) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		for ev := range scoped.Subscribe(ctx) {
			req := ev.Payload
			allowed, err := parent.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   req.SessionID,
				ToolCallID:  req.ToolCallID,
				ToolName:    req.ToolName,
				Description: req.Description,
				Action:      req.Action,
				Params:      req.Params,
				Path:        req.Path,
			})
			// A parent error (including the bridge's context being
			// canceled while the parent waits on the UI) denies the
			// scoped request so its waiter is never stranded.
			if err != nil {
				allowed = false
			}
			if allowed {
				scoped.Grant(req)
			} else {
				scoped.Deny(req)
			}
		}
	}()
	return cancel
}
