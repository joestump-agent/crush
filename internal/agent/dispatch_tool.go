package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
)

//go:embed templates/dispatch_tool.md
var dispatchToolDescription string

// DispatchAgentToolName is the registered name of the DispatchAgent tool.
const DispatchAgentToolName = "dispatch_agent"

// DispatchAgentParams are the DispatchAgent tool's arguments.
type DispatchAgentParams struct {
	Prompt string `json:"prompt" description:"Self-contained task instructions for the dispatched agent"`
	// Model is the model type the dispatched agent runs on — "large" or
	// "small" — defaulting to the small model. Per-dispatch model choice
	// is by selected-model type, not raw model ID, matching how agent
	// configs select models.
	Model string `json:"model,omitempty" description:"Model type to run the dispatched agent on: \"large\" or \"small\" (default \"small\")"`
	// Skills names the skills the dispatched agent may use; the default
	// is every skill discovered in the workspace.
	Skills []string `json:"skills,omitempty" description:"Names of skills to make available to the dispatched agent (default: all skills discovered in the workspace)"`
	// Branch is the base revision the workspace is cut from; the default
	// is the repository's current branch.
	Branch string `json:"branch,omitempty" description:"Base revision the isolated workspace is cut from (default: current branch)"`
	// Handle is the @handle the dispatched agent is addressable by
	// (#313); the user steers it mid-run as "@handle stop writing Rust".
	// Collisions are suffixed numerically (tester-2). Default: derived
	// from Role, else "agent".
	Handle string `json:"handle,omitempty" description:"@handle to address the dispatched agent by while it runs (default: derived from role, else \"agent\")"`
	// Role is the one-line label of what the agent is for — shown next
	// to the handle in the editor's @ completions and used to derive the
	// handle when none was supplied.
	Role string `json:"role,omitempty" description:"One-line role label for the dispatched agent (e.g. \"tester\", \"docs writer\"), shown in the @ completions and used to derive its handle"`
}

// dispatchSweepTimeout bounds the session-end sweep: it runs git commands
// against every workspace, and a wedged worktree must not hang shutdown.
const dispatchSweepTimeout = 30 * time.Second

// dispatchAgentOptions configures the dispatched-agent constructor.
type dispatchAgentOptions struct {
	// Toolchain is the workspace-rooted toolchain the agent runs with
	// (#62); its Tools, Config, and WorkingDir feed the agent build.
	Toolchain *DispatchToolchain
	// ModelType selects which selected model the dispatched agent runs
	// on. The default (empty) is the small model.
	ModelType config.SelectedModelType
	// Skills restricts the rendered available-skills set; empty means
	// every skill discovered in the workspace.
	Skills []string
}

// dispatchedAgent is the agent one dispatch runs, plus the model and
// provider config its Run call needs — the same call shaping runSubAgent
// applies to a sub-agent's turn.
type dispatchedAgent struct {
	agent       SessionAgent
	model       Model
	providerCfg config.ProviderConfig
}

// dispatchRun carries one backgrounded dispatch from the tool call that
// started it to the goroutine that runs it.
type dispatchRun struct {
	workspace   *dispatch.Workspace
	entry       dispatch.Entry
	toolchain   *DispatchToolchain
	agent       SessionAgent
	model       Model
	providerCfg config.ProviderConfig
	prompt      string
	sessionID   string
	// parentSessionID is the session the DispatchAgent tool call ran in;
	// the dispatched session's cost is propagated to it and the terminal
	// result payload (#66) is delivered back into it.
	parentSessionID string
	// contentWidth is the parent turn's UI width hint, captured at tool
	// call time so the dispatched agent's turns render at the right size
	// (the PrepareStep stamp would otherwise clobber it with zero).
	contentWidth int
	// stopServer tears the dispatch's A2A server down (#70); nil when no
	// server was started. The run owns it: the server serves exactly as
	// long as the dispatch runs.
	stopServer func()
}

// dispatchTool builds the DispatchAgent tool (#64): provision a clean
// workspace (#63), bootstrap the dispatched agent's toolchain rooted at
// it (#62), run a backgrounded SessionAgent on an ephemeral session
// (#48/#50), and return a running handle immediately so the main agent
// keeps working.
func (c *coordinator) dispatchTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		DispatchAgentToolName,
		dispatchToolDescription,
		func(ctx context.Context, params DispatchAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			modelType := config.SelectedModelType(params.Model)
			switch modelType {
			case "", config.SelectedModelTypeLarge, config.SelectedModelTypeSmall:
				// Empty defaults to the small model below.
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid model %q: must be \"large\" or \"small\"", params.Model)), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			workspace, err := c.dispatchWorkspace()
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("dispatch unavailable: %s", err)), nil
			}

			// Provision, bootstrap, and build while the tool call is
			// still open: every step is local and fast, and failures
			// here are actionable tool errors rather than silent
			// background failures.
			entry, err := workspace.Provision(ctx, dispatch.ProvisionOptions{Base: params.Branch})
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("provision dispatch workspace: %s", err)), nil
			}

			toolchain, err := c.BuildDispatchToolchain(ctx, DispatchToolchainOptions{WorkingDir: entry.Path})
			if err != nil {
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("build dispatch toolchain: %s", err)), nil
			}

			if unknown := missingSkills(toolchain.Config(), params.Skills); len(unknown) > 0 {
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown skills: %s", strings.Join(unknown, ", "))), nil
			}

			builder := c.dispatchAgentBuilder
			if builder == nil {
				builder = c.buildDispatchedAgent
			}
			dispatched, err := builder(ctx, dispatchAgentOptions{
				Toolchain: toolchain,
				ModelType: modelType,
				Skills:    params.Skills,
			})
			if err != nil {
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("build dispatched agent: %s", err)), nil
			}

			// One dispatch = one ephemeral task session (#48), keyed by
			// the tool call like the agent tool's sub-sessions.
			taskSessionID := c.sessions.CreateAgentToolSessionID(agentMessageID, call.ID)
			taskSession, err := c.sessions.CreateTaskSession(ctx, taskSessionID, sessionID, "Dispatched Agent")
			if err != nil {
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("create session: %s", err)), nil
			}

			// Keep the registry current from here on: #65's status card
			// and #313's handles read these entries. The handle is assigned
			// after the session so the assignment event carries the complete
			// entry — handle, role, session, running state.
			workspace.SetSession(entry.ID, taskSession.ID)
			workspace.SetStatus(entry.ID, dispatch.StatusRunning)
			assignedHandle, ok := workspace.AssignHandle(entry.ID, params.Handle, params.Role)
			if !ok {
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse("assign dispatch handle: registry entry vanished"), nil
			}

			// Stand up the dispatch's in-process A2A server (#70) and stamp
			// its endpoint and card on the registry entry — the in-memory
			// discovery surface. The server dies with the run; runDispatch
			// owns the stop.
			stopServer := c.startDispatchServer(ctx, workspace, entry.ID, taskSession.ID, assignedHandle, params.Role, dispatched.agent, resolvedSkills(toolchain.Config(), params.Skills))

			// The dispatch must outlive the parent turn that started it:
			// the main agent keeps working, and its tool-call context is
			// canceled as soon as the turn ends.
			go c.runDispatch(context.WithoutCancel(ctx), dispatchRun{
				workspace:       workspace,
				entry:           entry,
				toolchain:       toolchain,
				agent:           dispatched.agent,
				model:           dispatched.model,
				providerCfg:     dispatched.providerCfg,
				prompt:          params.Prompt,
				sessionID:       taskSession.ID,
				parentSessionID: sessionID,
				contentWidth:    tools.GetContentWidthFromContext(ctx),
				stopServer:      stopServer,
			})

			handle := dispatch.DispatchResult{
				DispatchID:    entry.ID,
				Handle:        assignedHandle,
				Branch:        entry.Branch,
				WorkspacePath: entry.Path,
				SessionID:     taskSession.ID,
				Status:        dispatch.StatusRunning,
			}
			return fantasy.NewTextResponse(handle.Render()), nil
		},
	)
}

// buildDispatchedAgent constructs the agent a dispatch runs: the chosen
// selected model (small by default), a system prompt rendered at dispatch
// time from the dispatch template against the workspace's scoped store
// (template + dispatch context: git status, context files, skills), and
// the workspace-rooted toolchain's tools.
func (c *coordinator) buildDispatchedAgent(ctx context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
	large, small, err := c.buildAgentModels(ctx, true)
	if err != nil {
		return nil, err
	}

	// The session agent runs on its "large" slot, so the chosen model
	// goes there; the small model stays available for auxiliary work.
	model := small
	if opts.ModelType == config.SelectedModelTypeLarge {
		model = large
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	promptOpts := []prompt.Option{prompt.WithWorkingDir(opts.Toolchain.WorkingDir())}
	if len(opts.Skills) > 0 {
		promptOpts = append(promptOpts, prompt.WithSkills(opts.Skills))
	}
	systemPrompt, err := dispatchPrompt(promptOpts...)
	if err != nil {
		return nil, err
	}
	rendered, err := systemPrompt.Build(ctx, model.Model.Provider(), model.Model.Model(), opts.Toolchain.Config())
	if err != nil {
		return nil, fmt.Errorf("render dispatch system prompt: %w", err)
	}

	// Prompt and tools are known at construction, so the agent's
	// readiness latch is satisfied immediately (newSessionAgent) — no
	// build-time goroutines to wait for.
	agent := newSessionAgent(SessionAgentOptions{
		LargeModel:           model,
		SmallModel:           small,
		SystemPromptPrefix:   providerCfg.SystemPromptPrefix,
		SystemPrompt:         rendered,
		IsSubAgent:           true,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Cfg:                  opts.Toolchain.Config(),
		Tools:                opts.Toolchain.Tools(),
		Notify:               c.notify,
		RunComplete:          c.runComplete,
	})

	return &dispatchedAgent{agent: agent, model: model, providerCfg: providerCfg}, nil
}

// runDispatch runs one dispatched agent to completion in the background
// (#64): it drives the ephemeral session's turn, keeps the registry's
// status current, propagates the dispatched session's cost to the parent,
// and delivers the terminal DispatchResult back to the main agent (#66).
func (c *coordinator) runDispatch(ctx context.Context, run dispatchRun) {
	defer func() {
		// The A2A server dies with the run (#70): teardown clears the
		// registry's endpoint and card too, so discovery never hands out
		// a dead endpoint. It stops before the toolchain closes, which
		// ends the shared process-wide resources underneath it.
		if run.stopServer != nil {
			c.stopDispatchServer(run.workspace, run.entry.ID, run.stopServer)
		}
		// The toolchain outlives the turn: Close stops the permission
		// bridge and the scoped LSP clients once nothing runs in the
		// workspace anymore.
		run.toolchain.Close(ctx)
	}()

	maxTokens := run.model.CatwalkCfg.DefaultMaxTokens
	if run.model.ModelCfg.MaxTokens != 0 {
		maxTokens = run.model.ModelCfg.MaxTokens
	}

	call := SessionAgentCall{
		SessionID:        run.sessionID,
		ContentWidth:     run.contentWidth,
		Prompt:           run.prompt,
		MaxOutputTokens:  maxTokens,
		ProviderOptions:  getProviderOptions(run.model, run.providerCfg),
		Temperature:      run.model.ModelCfg.Temperature,
		TopP:             run.model.ModelCfg.TopP,
		TopK:             callTopK(run.providerCfg, run.model.ModelCfg.TopK),
		FrequencyPenalty: run.model.ModelCfg.FrequencyPenalty,
		PresencePenalty:  run.model.ModelCfg.PresencePenalty,
		NonInteractive:   true,
		OnAuthRefresh:    c.makeAuthRefreshCallback(run.providerCfg),
	}

	// Make the running agent addressable for mid-run injection (#312)
	// for exactly the run's lifetime: injected messages clone this call's
	// shaping, and the target is dropped the moment the run returns so a
	// finished dispatch refuses instead of running another turn.
	if injectable, ok := run.agent.(injectableAgent); ok {
		c.registerDispatchRun(run.sessionID, injectable, call)
		defer c.unregisterDispatchRun(run.sessionID)
	}

	result, err := run.agent.Run(ctx, call)

	// Drop the injection target as soon as the run returns, before the
	// terminal status is published below: relying on the deferred
	// unregister alone left a window in which a terminal entry still
	// resolved to a live target and accepted a message into a session
	// whose run had ended. unregisterDispatchRun is idempotent; the
	// deferred call above stays for the early-return paths.
	c.unregisterDispatchRun(run.sessionID)

	// A nil result with a nil error means no turn ran — the session was
	// busy or a cancel landed during dispatch (#173 review note on #64).
	// With one ephemeral session per dispatch it should not fire, but it
	// is a failure, never a success. One dispatch = one turn, so the
	// queued-behind-a-busy-session path cannot produce a late result
	// either.
	if err != nil {
		slog.Error("Dispatched agent run failed", "dispatch_id", run.entry.ID, "session_id", run.sessionID, "error", err)
	} else if result == nil {
		slog.Error("Dispatched agent ran no turn", "dispatch_id", run.entry.ID, "session_id", run.sessionID)
	}

	terminal := c.assembleDispatchResult(ctx, run, result, err)
	// Record the terminal payload before the terminal status so the
	// terminal entry event carries it: the completed agent block (#65)
	// renders its durable record from the registry.
	run.workspace.SetResult(run.entry.ID, terminal)
	run.workspace.SetStatus(run.entry.ID, terminal.Status)

	// Cost propagation is best-effort, mirroring runSubAgent: a failure
	// here must not lose the run's outcome.
	if err := c.updateParentSessionCost(ctx, run.sessionID, run.parentSessionID); err != nil {
		slog.Warn("Failed to update parent session cost", "child_session", run.sessionID, "parent_session", run.parentSessionID, "error", err)
	}

	c.deliverDispatchResult(ctx, run.parentSessionID, terminal)
}

// assembleDispatchResult builds the terminal DispatchResult (#66) from a
// finished dispatched run: the run outcome maps to the status, the
// agent's final text to the key findings, and Workspace.Diff's output —
// condensed — to the diff summary. A completed run stays completed even
// when diff capture fails; the failure is recorded in the summary so the
// main agent knows why it is missing.
func (c *coordinator) assembleDispatchResult(ctx context.Context, run dispatchRun, result *fantasy.AgentResult, runErr error) dispatch.DispatchResult {
	terminal := dispatch.DispatchResult{
		DispatchID:    run.entry.ID,
		Branch:        run.entry.Branch,
		WorkspacePath: run.entry.Path,
		SessionID:     run.sessionID,
	}
	// The handle was assigned after the dispatchRun's entry snapshot was
	// taken, so read it back from the registry: the terminal payload is
	// the model's addressable record of the run (#313).
	if entry, ok := run.workspace.Get(run.entry.ID); ok {
		terminal.Handle = entry.Handle
	}
	switch {
	case runErr != nil:
		terminal.Status = dispatch.StatusFailed
		terminal.Error = runErr.Error()
	case result == nil:
		terminal.Status = dispatch.StatusFailed
		terminal.Error = "agent session did not start a turn (busy or canceled)"
	default:
		terminal.Status = dispatch.StatusCompleted
		terminal.KeyFindings = subAgentOutput(result)
		diff, diffErr := run.workspace.Diff(ctx, run.entry.ID)
		switch {
		case diffErr != nil:
			terminal.DiffSummary = fmt.Sprintf("(diff unavailable: %s)", diffErr)
		case diff == "":
			terminal.DiffSummary = "(no changes)"
		default:
			terminal.DiffSummary = dispatch.SummarizeDiff(diff)
		}
	}
	return terminal
}

// deliverDispatchResult hands the terminal payload to the main agent
// (#66). The tool call that started the dispatch returned its running
// handle long ago, so the payload is delivered as a follow-up turn on
// the parent session — hidden, so it reads as dispatch output rather
// than a user message — through the same path a scheduled task fires
// (fireScheduledTask): a normal run that respects the session's busy
// queue. This is the in-process Phase 1 stand-in for #71's A2A terminal
// status message; the payload shape is identical either way.
//
// Delivery is dropped, logged, when there is nothing to deliver into: a
// parent session that no longer exists (deleted, or a `crush run`
// process that already exited), or no runnable main agent.
func (c *coordinator) deliverDispatchResult(ctx context.Context, parentSessionID string, terminal dispatch.DispatchResult) {
	if _, err := c.sessions.Get(ctx, parentSessionID); err != nil {
		slog.Debug("Dispatch result dropped: parent session is gone", "parent_session", parentSessionID, "dispatch_id", terminal.DispatchID)
		return
	}
	if c.currentAgent() == nil {
		slog.Debug("Dispatch result dropped: no main agent", "parent_session", parentSessionID, "dispatch_id", terminal.DispatchID)
		return
	}

	payload := terminal.TerminalMessage()
	go func() {
		// Detached: the dispatch goroutine's context ends when this
		// function returns, and the delivered turn must outlive it. The
		// hidden marker keeps the injected prompt out of the chat UI —
		// the agent block (#65) is the visible surface for dispatch
		// state, and the parent's own reply to the payload is the
		// visible outcome.
		runCtx := message.WithHiddenUserMessage(context.WithoutCancel(ctx))
		if _, err := c.run(runCtx, nil, parentSessionID, payload); err != nil {
			slog.Error("Dispatch result delivery failed", "parent_session", parentSessionID, "dispatch_id", terminal.DispatchID, "error", err)
		}
	}()
}

// dispatchWorkspace returns the coordinator's dispatch workspace
// registry, creating it on first use. Creation can fail — the working
// directory may not be a git repository — and the failure is cached so
// every later dispatch reports it instead of retrying, while coordinator
// construction stays git-agnostic. The first successful creation also
// starts the todo collector (#65): one subscription to the session
// event stream and the registry's own transitions, reduced once and
// sunk to every configured sink (the agent block now, #174's A2A
// bridge later).
func (c *coordinator) dispatchWorkspace() (*dispatch.Workspace, error) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if c.dispatchWS == nil && c.dispatchWSErr == nil {
		c.dispatchWS, c.dispatchWSErr = dispatch.NewWorkspace(c.cfg.WorkingDir())
		if c.dispatchWS != nil {
			c.dispatchCollector = dispatch.NewTodoCollector(c.dispatchWS, c.sessions, c.dispatchSinks...)
			ctx := c.dispatchCtx
			if ctx == nil {
				// Tests construct the coordinator struct directly; a nil
				// context would panic the collector's subscription.
				ctx = context.Background()
			}
			// Start subscribes synchronously before returning, so the
			// session and entry events of the dispatch being provisioned
			// right now are already observed.
			c.dispatchCollector.Start(ctx)
		}
	}
	return c.dispatchWS, c.dispatchWSErr
}

// DispatchStatus returns the current progress snapshot for the
// dispatched agent running on sessionID (#65). It backs the UI's
// seed-on-load path: a reloaded session's persisted dispatch_agent
// result is the running handle, and this is how the block learns the
// dispatch's real state without waiting for the next event.
func (c *coordinator) DispatchStatus(sessionID string) (dispatch.TodoSnapshot, bool) {
	c.dispatchMu.Lock()
	collector := c.dispatchCollector
	c.dispatchMu.Unlock()
	if collector == nil {
		return dispatch.TodoSnapshot{}, false
	}
	return collector.Snapshot(sessionID)
}

// sweepDispatchOnDone is the session-end backstop (#63's Sweep): when the
// coordinator's context ends — app shutdown — every workspace dispatch
// created is torn down, in-flight runs included (their runs fail against
// a removed workspace; wander kill in #316 owns deterministic
// cancellation of live runs).
func (c *coordinator) sweepDispatchOnDone(ctx context.Context) {
	<-ctx.Done()
	c.sweepDispatch()
}

// sweepDispatch sweeps the dispatch workspace registry if one exists.
func (c *coordinator) sweepDispatch() {
	c.dispatchMu.Lock()
	workspace := c.dispatchWS
	c.dispatchMu.Unlock()
	if workspace == nil {
		return
	}
	// A fresh, bounded context: the coordinator's own is already done.
	ctx, cancel := context.WithTimeout(context.Background(), dispatchSweepTimeout)
	defer cancel()
	if err := workspace.Sweep(ctx); err != nil {
		slog.Error("Dispatch workspace sweep failed", "error", err)
	}
}

// removeDispatch tears down a dispatch that failed before its background
// run started: the registry entry, the workspace, and the toolchain. Best
// effort — the tool error being returned to the model matters more.
func (c *coordinator) removeDispatch(ctx context.Context, workspace *dispatch.Workspace, id string, toolchain *DispatchToolchain) {
	if toolchain != nil {
		toolchain.Close(ctx)
	}
	if err := workspace.Remove(ctx, id); err != nil {
		slog.Warn("Failed to remove failed dispatch workspace", "dispatch_id", id, "error", err)
	}
}

// missingSkills returns the requested skill names that do not exist in
// the store's discovered skills, so a dispatch with a typo fails at tool
// call time instead of silently running without them.
func missingSkills(store *config.ConfigStore, requested []string) []string {
	if len(requested) == 0 {
		return nil
	}
	all, _ := discoverSkills(store)
	known := make(map[string]struct{}, len(all))
	for _, s := range all {
		known[s.Name] = struct{}{}
	}
	var unknown []string
	for _, name := range requested {
		if _, ok := known[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	return unknown
}
