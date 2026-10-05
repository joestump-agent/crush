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
	"github.com/charmbracelet/crush/internal/hooks"
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
// the LSP manager and permission service are scoped to the same directory.
// The file tools resolve every path against the workspace root and refuse
// anything outside it (#379): each dispatched call carries the root on its
// context. Bash is rooted at the workspace directory but its commands can
// reach anywhere the process can, so staying inside is advised for bash,
// not enforced. For the file tools the boundary is the toolset, not the
// call: SessionAgentCall is unchanged, and an agent built the usual way
// (via buildTools against the workspace root) behaves exactly as before.
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
// the parent's configuration viewed from the workspace directory: the
// parent's published config (permissions, command allow-lists, LSP
// servers, skills paths, MCP) with workingDir pointed at the workspace,
// so directory-scoped behavior resolves there while no policy value can
// come from the workspace's own config files (#374).
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

// Tools returns the constructed tool set, filtered to the task agent's
// allowed tools, widened with the dispatch write and support tools and
// narrowed by the parent's deny list.
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
	// rooted at — the path the git worktree provider (#63) provisions.
	// Required.
	WorkingDir string
}

// BuildDispatchToolchain builds a dispatched agent's entire toolchain
// rooted at opts.WorkingDir: the file/bash tools are constructed with
// workingDir = the workspace path, and the LSP manager and permission
// service are rooted there too. The scoped config store is the parent's
// configuration viewed from the workspace directory (#374): the model
// chooses the base revision, so nothing that revision's config files
// declare — shell config, permissions, command allow-lists, LSP
// commands, skills — is read or executed; policy is always the parent's.
// The parent's data directory is reused so no second database or lock
// is taken.
//
// #64's DispatchAgent tool consumes this: provision a clean workspace,
// bootstrap the toolchain against its path, run. The caller's context is
// the permission bridge's lifetime (#371): the tool passes the dispatch's
// root, never the tool-call context.
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

	// Scoped config: the parent's configuration viewed from the
	// dispatched workspace (#374). Nothing in the workspace is read or
	// executed, so a base revision the model chose cannot run its own
	// shell config or widen policy; directory-scoped behavior (path
	// tools, LSP roots) still resolves against the workspace, and the
	// parent's data directory keeps logs, spill files, and the database
	// shared.
	scoped := c.cfg.WithWorkingDir(dir)

	lspManager := lsp.NewManager(scoped)

	// Scoped permissions, mirroring app.New's construction: rooted at the
	// workspace directory and inheriting the parent's allowed-tools so a
	// dispatched agent starts from the same policy. The list is read from
	// the parent's config (#374): a base revision's permissions allow
	// must never auto-approve a dispatched agent's tools. With a parent
	// service, skip approval follows the parent's live state so a
	// runtime yolo toggle reaches dispatched agents; the startup flag is
	// only a fallback for callers with no parent service.
	var allowedTools []string
	if parentCfg := c.cfg.Config(); parentCfg.Permissions != nil && parentCfg.Permissions.AllowedTools != nil {
		allowedTools = parentCfg.Permissions.AllowedTools
	}
	var permissions permission.Service
	if c.permissions != nil {
		permissions = permission.NewScopedPermissionService(c.permissions, dir, allowedTools)
	} else {
		permissions = permission.NewPermissionService(dir, c.cfg.Overrides().SkipPermissionRequests, allowedTools)
	}

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

// dispatchCapabilityTools are the write tools a dispatch is useless
// without (#376): if the parent's deny list removes all of them, the
// dispatch tool refuses before provisioning a workspace: a dispatched
// agent that can neither run commands nor edit files cannot produce
// work.
var dispatchCapabilityTools = []string{
	tools.BashToolName,
	tools.EditToolName,
	tools.MultiEditToolName,
	tools.WriteToolName,
}

// dispatchWriteTools are the tools every dispatched agent gets on top of
// the task agent's AllowedTools (#64). The task agent's default set is
// deliberately read-only — it exists to answer research prompts — but a
// dispatch's whole point is producing work, so bash, the edit tools,
// write, and the todos tool the enforcement ladder needs (interaction
// model, #315) are added to the union. The union is still bounded by the
// parent's deny list: dispatchAllowedTools drops anything the user
// denied, so options.disabled_tools / permissions deny hold inside a
// dispatch (#376). Narrowing via config still works for read tools.
var dispatchWriteTools = slices.Concat(dispatchCapabilityTools, []string{tools.TodosToolName})

// dispatchAllowedTools is the allow-list a dispatched agent's tools are
// filtered against: the task agent's allowed tools widened with
// dispatchWriteTools (#64) and dispatchSupportTools (#384), minus
// everything in disabled (#376).
// disabled is the parent's options.disabled_tools (the list
// permissions deny writes), never the dispatched workspace's own config,
// which must not widen what the user denied at the top (#374).
func dispatchAllowedTools(agentCfg config.Agent, disabled []string) []string {
	allowed := slices.Concat(agentCfg.AllowedTools, dispatchWriteTools, dispatchSupportTools)
	return slices.DeleteFunc(allowed, func(name string) bool {
		return slices.Contains(disabled, name)
	})
}

// dispatchSupportTools are the observation tools a dispatched agent gets
// on top of the task agent's AllowedTools (#384). bash auto-backgrounds
// any command past DefaultAutoBackgroundAfter and tells the agent to read
// the result back with job_output (or stop it with job_kill), and
// lsp_diagnostics is how the agent checks what the LSP thinks of an edit
// (constructed only while the LSP tools are registered). None of them
// writes, but without them a dispatch loses the output of any command it
// starts: the buffer lives only in this process.
var dispatchSupportTools = []string{
	tools.JobOutputToolName,
	tools.JobKillToolName,
	tools.DiagnosticsToolName,
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

	// The task agent's set widened with the dispatch write tools and
	// support tools, minus the parent's deny list (#376): a dispatched
	// agent must be able to edit, not just read (#64), and observe what
	// it ran and wrote (#384), but a tool the user denied stays denied.
	allowed := dispatchAllowedTools(agentCfg, c.cfg.Config().Options.DisabledTools)

	var filtered []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(allowed, tool.Info().Name) {
			filtered = append(filtered, tool)
		}
	}

	// Dispatched agents fire the parent's PreToolUse hooks (#377): they
	// carry bash, edit, multiedit, and write, so a hook that blocks a
	// command must reach them too. Hooks come from the parent store —
	// never the workspace's scoped config — and run with the parent's
	// working directory as both cwd and projectDir, so hook commands
	// resolve to the parent's trusted scripts, never a copy checked out
	// at a model-chosen base. The payload carries the dispatched
	// session's ID, which is what the hook sees.
	var hookRunner *hooks.Runner
	if preToolHooks := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	// Every dispatched tool call carries the workspace root on its
	// context (#379), so path-resolving tools refuse anything outside
	// it even when the tool itself does not know the working directory.
	for i, tool := range filtered {
		filtered[i] = containedTool{inner: tool, workspace: dir}
	}
	return wrapToolsWithHooks(filtered, hookRunner, false)
}

// containedTool injects the dispatch workspace root into a tool
// call's context before delegating to the inner tool.
type containedTool struct {
	inner     fantasy.AgentTool
	workspace string
}

func (c containedTool) Info() fantasy.ToolInfo {
	return c.inner.Info()
}

func (c containedTool) ProviderOptions() fantasy.ProviderOptions {
	return c.inner.ProviderOptions()
}

func (c containedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	c.inner.SetProviderOptions(opts)
}

func (c containedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return c.inner.Run(tools.WithContainmentRoot(ctx, c.workspace), call)
}

// bridgePermissions forwards permission requests raised inside a dispatch
// scope to the parent workspace's permission service, so dispatched-agent
// tool calls surface in the same approval flow as the main agent's. Each
// forwarded request resolves the scoped service's pending request with
// the parent's verdict. The bridge's context is the dispatch's root, not
// the parent turn's tool-call context: it lives as long as the dispatch
// (#371), so requests raised after the turn ends still reach the parent's
// subscribers. Requests still waiting when the bridge stops are denied by
// the canceled context.
func bridgePermissions(ctx context.Context, parent, scoped permission.Service) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	// Subscribe before spawning the consumer: Broker delivery is lossy
	// for events published before a subscriber registers, so subscribing
	// inside the goroutine could strand a request that arrived first and
	// leave its waiter blocked until the context is canceled.
	events := scoped.Subscribe(ctx)
	go func() {
		for ev := range events {
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
