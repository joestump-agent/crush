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
	"github.com/charmbracelet/crush/internal/question"
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
//   - MCP tools, whose clients are rooted at the parent workspace;
//   - semantic search, whose index is rooted at the parent workspace.
//
// The question tool is present only when the parent is interactive
// (#352), and it never reaches the parent's question service directly:
// it asks through the toolchain's own service, which the served
// executor turns into an input-required pause on the A2A task, and the
// parent's transport carries the question to its user.
//
// Phase 1 is in-process: the parent's session, history, and file-tracker
// services are shared (they are keyed by session ID, not path). The
// scoped permission service follows the parent's live yolo state, and a
// request that needs a person rides the A2A task the same way a question
// does (#353): the served executor parks the run in input-required, and
// the parent's transport puts the request through the parent's approval
// flow.
type DispatchToolchain struct {
	workingDir  string
	store       *config.ConfigStore
	lspManager  *lsp.Manager
	permissions permission.Service
	tools       []fantasy.AgentTool
	// questions is the dispatched agent's own question service (#352),
	// the one its question tool asks through. Nil when the parent was
	// not interactive at build time, and the agent has no question tool.
	questions question.Service
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

// Questions returns the dispatched agent's own question service (#352):
// the served executor watches it and parks the run on each question.
// Nil when the agent has no question tool.
func (t *DispatchToolchain) Questions() question.Service {
	if t == nil {
		return nil
	}
	return t.questions
}

// Tools returns the constructed tool set, filtered to the task agent's
// allowed tools, widened with the dispatch write and support tools and
// narrowed by the parent's deny list.
func (t *DispatchToolchain) Tools() []fantasy.AgentTool {
	return t.tools
}

// Close tears the toolchain down: it shuts down every LSP client the
// scoped manager started. It is safe to call more than once and on a nil
// toolchain.
func (t *DispatchToolchain) Close(ctx context.Context) {
	if t == nil {
		return
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
	// Agent is the id of the agent definition the toolchain's tools are
	// built from (#433); empty means the worker definition.
	Agent string
}

// BuildDispatchToolchain builds a dispatched agent's entire toolchain
// rooted at opts.WorkingDir: the file/bash tools are constructed with
// workingDir = the workspace path, and the LSP manager and permission
// service are rooted there too. The tool palette is the selected agent
// definition's (#433): opts.Agent names it, empty meaning the worker
// definition. The scoped config store is the parent's
// configuration viewed from the workspace directory (#374): the model
// chooses the base revision, so nothing that revision's config files
// declare — shell config, permissions, command allow-lists, LSP
// commands, skills — is read or executed; policy is always the parent's.
// The parent's data directory is reused so no second database or lock
// is taken.
//
// #64's DispatchAgent tool consumes this: provision a clean workspace,
// bootstrap the toolchain against its path, run. The tool passes the
// dispatch's root context (#371), never the tool-call context; nothing in
// the toolchain holds it since the permission bridge gave way to A2A
// permission prompts (#353).
func (c *coordinator) BuildDispatchToolchain(_ context.Context, opts DispatchToolchainOptions) (*DispatchToolchain, error) {
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

	agentID := opts.Agent
	if agentID == "" {
		agentID = config.AgentWorker
	}
	agentCfg, ok := c.cfg.Config().Agents[agentID]
	if !ok {
		return nil, fmt.Errorf("agent %q not configured", agentID)
	}
	if agentCfg.Disabled {
		return nil, fmt.Errorf("agent %q is disabled", agentID)
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

	t := &DispatchToolchain{
		workingDir:  dir,
		store:       scoped,
		lspManager:  lspManager,
		permissions: permissions,
	}
	// A dispatched agent may ask the parent's user a question (#352), but
	// only while someone can answer: a non-interactive parent's dispatch
	// gets no question service, and with it no question tool.
	if c.isInteractive() {
		t.questions = question.NewService()
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

// dispatchAllowedTools is the allow-list a dispatched agent's tools are
// filtered against: the worker definition's resolved tools (#432), minus
// everything in disabled (#376). The definition can narrow the palette
// but never widen user policy: disabled_tools is applied last.
// disabled is the parent's options.disabled_tools (the list
// permissions deny writes), never the dispatched workspace's own config,
// which must not widen what the user denied at the top (#374).
func dispatchAllowedTools(workerCfg config.Agent, disabled []string) []string {
	return effectiveToolNames(workerCfg.AllowedTools, disabled)
}

// buildDispatchTools constructs the dispatched agent's tools against the
// scoped toolchain, mirroring buildTools for everything that is rooted at
// a directory. The parent's session-scoped services (todos) and data-dir
// services (logs, jobs) are reused; every path-resolving tool gets the
// workspace directory.
func (c *coordinator) buildDispatchTools(workerCfg config.Agent, t *DispatchToolchain) []fantasy.AgentTool {
	scoped := t.store.Config()
	dir := t.workingDir

	// Model ID for the bash tool's error attribution, same derivation as
	// buildTools: an explicit pin (#432) names it, otherwise the agent's
	// slot does.
	modelID := ""
	if workerCfg.ModelRef != nil {
		if model := scoped.GetModel(workerCfg.ModelRef.Provider, workerCfg.ModelRef.Model); model != nil {
			modelID = model.ID
		}
	} else if modelCfg, ok := scoped.Models[workerCfg.Model]; ok {
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

	// The worker definition's tool set (#432), minus the parent's deny
	// list (#376): the definition narrows the palette but can never
	// widen user policy, and a tool the user denied stays denied.
	allowed := dispatchAllowedTools(workerCfg, c.cfg.Config().Options.DisabledTools)

	var filtered []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(allowed, tool.Info().Name) {
			filtered = append(filtered, tool)
		}
	}

	// MCP tools ride the process-wide server registry (#432): the
	// servers are shared with the parent, not rooted at the workspace,
	// and the worker definition's AllowedMCP decides what the
	// dispatched agent may call.
	filtered = append(filtered, filterMCPTools(workerCfg, tools.GetMCPTools(t.permissions, t.store, dir))...)

	// The question tool (#352) asks through the toolchain's own service,
	// never the parent's: the served executor parks the run on each
	// question and the parent's transport carries it to the user. A
	// question tool the user denied stays denied.
	if t.questions != nil && !slices.Contains(c.cfg.Config().Options.DisabledTools, tools.QuestionToolName) {
		filtered = append(filtered, tools.NewQuestionTool(t.questions))
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
