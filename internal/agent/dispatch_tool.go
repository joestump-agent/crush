package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	// TodoKill observes the wander-kill escalation (#316): invoked when
	// the dispatched run ignores its nudges past the kill threshold, so
	// the coordinator can record the reason against this dispatch. The
	// run's cancellation is intrinsic; this only observes.
	TodoKill func(sessionID string, reason string)
	// LoopStop observes the loop-detection stop (#343): invoked when the
	// dispatched run's step loop ends on the loop-detection stop
	// condition, so the coordinator can record the tool-loop kill reason
	// against this dispatch. The run's own end is intrinsic; this only
	// observes — over the served transport path it is the only signal a
	// loop stop ever happened.
	LoopStop func(sessionID string)
}

// dispatchKill records the deterministic-kill outcome of one dispatched
// run (#316). The enforcement ladder's hook and the run's watchdog both
// feed it; runDispatch reads it once, after the run returns, to decide
// the terminal result. First reason wins. A nil kill is the no-op fast
// path, so runs without kill wiring (existing callers, tests) never
// branch on it.
type dispatchKill struct {
	mu     sync.Mutex
	reason string
}

func (k *dispatchKill) kill(reason string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.reason == "" {
		k.reason = reason
	}
}

func (k *dispatchKill) current() string {
	if k == nil {
		return ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reason
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

	// kill accumulates the wander-kill reason (#316) from the
	// enforcement ladder's hook and the watchdog; empty when the run is
	// never killed.
	kill *dispatchKill
	// killSettings are the resolved wander-kill thresholds for this
	// dispatch: nudges-before-kill, todos stall window, hard timeout.
	killSettings config.TodoEnforcementSettings
}

// registerLiveDispatch records a running dispatch (#371). The map is
// lazily created: tests construct the coordinator struct directly.
func (c *coordinator) registerLiveDispatch(id string, live *liveDispatch) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if c.liveDispatches == nil {
		c.liveDispatches = make(map[string]*liveDispatch)
	}
	c.liveDispatches[id] = live
}

// teardownLiveDispatch ends one dispatch: cancel the root (the bridge
// bound to it exits with it), drop the live record, and close done so
// waiters observe the teardown. Unknown IDs (a dispatch that never
// registered, or a second teardown) are a no-op.
func (c *coordinator) teardownLiveDispatch(id string) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	live, ok := c.liveDispatches[id]
	if !ok {
		return
	}
	delete(c.liveDispatches, id)
	live.cancel()
	close(live.done)
}

// call builds the full SessionAgentCall a dispatch's turns run with:
// the chosen model's shaping, the parent turn's content width, and the
// non-interactive flag. Built per consumer — the server stamps its
// executor's template with it at start (#71), and the direct run and the
// injection queue clone it at run time.
func (r dispatchRun) call(c *coordinator) SessionAgentCall {
	maxTokens := r.model.CatwalkCfg.DefaultMaxTokens
	if r.model.ModelCfg.MaxTokens != 0 {
		maxTokens = r.model.ModelCfg.MaxTokens
	}
	return SessionAgentCall{
		SessionID:        r.sessionID,
		ContentWidth:     r.contentWidth,
		Prompt:           r.prompt,
		MaxOutputTokens:  maxTokens,
		ProviderOptions:  getProviderOptions(r.model, r.providerCfg),
		Temperature:      r.model.ModelCfg.Temperature,
		TopP:             r.model.ModelCfg.TopP,
		TopK:             callTopK(r.providerCfg, r.model.ModelCfg.TopK),
		FrequencyPenalty: r.model.ModelCfg.FrequencyPenalty,
		PresencePenalty:  r.model.ModelCfg.PresencePenalty,
		NonInteractive:   true,
		OnAuthRefresh:    c.makeAuthRefreshCallback(r.providerCfg),
	}
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

			// The dispatch outlives the turn that started it: its root
			// context is detached from the tool call's, so the run and
			// the permission bridge bound to the root (#371) survive the
			// turn's end. runDispatch's teardown cancels it, and every
			// setup-failure path below does too. Not derived from
			// c.dispatchCtx: in server mode that is still the request
			// context (#419).
			rootCtx, rootCancel := context.WithCancel(context.WithoutCancel(ctx))

			// The toolchain's permission bridge binds to the dispatch's
			// root (#371): the bridge lives as long as the dispatch, not
			// the tool call.
			toolchain, err := c.BuildDispatchToolchain(rootCtx, DispatchToolchainOptions{WorkingDir: entry.Path})
			if err != nil {
				rootCancel()
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("build dispatch toolchain: %s", err)), nil
			}

			if unknown := missingSkills(toolchain.Config(), params.Skills); len(unknown) > 0 {
				rootCancel()
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown skills: %s", strings.Join(unknown, ", "))), nil
			}

			builder := c.dispatchAgentBuilder
			if builder == nil {
				builder = c.buildDispatchedAgent
			}
			// The wander-kill state (#316): the enforcement ladder's hook
			// and the run's watchdog both feed it; the run goroutine reads
			// it to assemble the terminal result.
			kill := &dispatchKill{}
			killSettings := config.ResolveTodoEnforcement(c.cfg.Config().Options.TodoEnforcement, nil)
			dispatched, err := builder(ctx, dispatchAgentOptions{
				Toolchain: toolchain,
				ModelType: modelType,
				Skills:    params.Skills,
				TodoKill: func(sessionID string, reason string) {
					kill.kill(reason)
				},
				LoopStop: func(sessionID string) {
					kill.kill(dispatch.ReasonToolLoop)
				},
			})
			if err != nil {
				rootCancel()
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("build dispatched agent: %s", err)), nil
			}

			// One dispatch = one ephemeral task session (#48), keyed by
			// the tool call like the agent tool's sub-sessions.
			taskSessionID := c.sessions.CreateAgentToolSessionID(agentMessageID, call.ID)
			taskSession, err := c.sessions.CreateTaskSession(ctx, taskSessionID, sessionID, "Dispatched Agent")
			if err != nil {
				rootCancel()
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
				rootCancel()
				c.removeDispatch(ctx, workspace, entry.ID, toolchain)
				return fantasy.NewTextErrorResponse("assign dispatch handle: registry entry vanished"), nil
			}

			run := dispatchRun{
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
				kill:            kill,
				killSettings:    killSettings,
			}

			// Stand up the dispatch's in-process A2A server (#70) and stamp
			// its endpoint and card on the registry entry — the in-memory
			// discovery surface. The executor's served turns run with the
			// dispatch's own call shaping (#71); the server dies with the
			// run, and runDispatch owns the stop. The inactivity backstop
			// (#360) is armed with the dispatch's resolved enforcement
			// settings, and the kill's reason (#316) rides along (#342):
			// an out-of-band kill surfaces on the A2A task as a Canceled
			// status carrying it.
			run.stopServer = c.startDispatchServer(ctx, workspace, entry.ID, taskSession.ID, assignedHandle, params.Role, dispatched.agent, resolvedSkills(toolchain.Config(), params.Skills), run.call(c), run.killSettings.InactivityTimeout, run.kill.current)

			// The dispatch runs on its root context, detached from the
			// tool call's (#371): the permission bridge bound to the root
			// lives as long as the dispatch, not the turn. The live record
			// goes in before the run starts so teardown cancels the root
			// and closes done exactly once per dispatch.
			live := &liveDispatch{
				cancel:    rootCancel,
				sessionID: taskSession.ID,
				agent:     dispatched.agent,
				kill:      kill,
				done:      make(chan struct{}),
			}
			c.registerLiveDispatch(entry.ID, live)
			go c.runDispatch(rootCtx, run)

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
	// build-time goroutines to wait for. Dispatched agents are not agent
	// definitions, so the todo enforcement ladder (#315) uses the global
	// options: every surface that consumes their todos (the block's
	// current-todo line, the A2A event bridge) starves without it.
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
		TodoEnforcement:      config.ResolveTodoEnforcement(c.cfg.Config().Options.TodoEnforcement, nil),
		// The dispatched agent is the one agent whose run may be killed
		// (#316): the observers hand the reasons to the coordinator's kill
		// state so the terminal result carries them — the ladder's
		// escalation and the loop-detection stop alike (#343).
		TodoKill: opts.TodoKill,
		LoopStop: opts.LoopStop,
	})

	return &dispatchedAgent{agent: agent, model: model, providerCfg: providerCfg}, nil
}

// runDispatch runs one dispatched agent to completion in the background
// (#64): it drives the ephemeral session's turn, keeps the registry's
// status current, propagates the dispatched session's cost to the parent,
// and delivers the terminal DispatchResult back to the main agent (#66).
// The wander-kill watchdog (#316) runs alongside the turn and cancels it
// deterministically when a kill threshold trips.
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
		// The dispatch's root dies with the dispatch (#371): canceling
		// it ends the bridge goroutine bound to it, and dropping the
		// live record keeps the registry holding exactly the running
		// dispatches.
		c.teardownLiveDispatch(run.entry.ID)
	}()

	watchStop := c.startDispatchKillWatch(ctx, run)
	defer watchStop()

	call := run.call(c)

	// Make the running agent addressable for mid-run injection (#312)
	// for exactly the run's lifetime: injected messages clone this call's
	// shaping, and the target is dropped the moment the run returns so a
	// finished dispatch refuses instead of running another turn.
	if injectable, ok := run.agent.(injectableAgent); ok {
		c.registerDispatchRun(run.sessionID, injectable, call)
		defer c.unregisterDispatchRun(run.sessionID)
	}

	// The transport swap (#71): a served dispatch — an endpoint and card
	// stamped on its registry entry, and a transport wired with the
	// server factory — runs over the A2A client, its SSE stream consumed
	// to the terminal state. Everything else (an unserved dispatch, no
	// factory wired) keeps the direct in-process run. Either way the
	// injection target above is the same: the agent behind the session
	// is one and the same object on both paths.
	var terminal dispatch.DispatchResult
	if transported, ok := c.runDispatchOverTransport(ctx, run); ok {
		terminal = transported
	} else {
		result, err := run.agent.Run(ctx, call)
		watchStop()

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

		terminal = c.assembleTerminalDispatchResult(ctx, run, dispatchNaturalOutcome{
			completed:     err == nil && result != nil,
			findings:      subAgentOutput(result),
			runErr:        err,
			stoppedInLoop: dispatchRunStoppedInLoop(result),
			diff: func(ctx context.Context) (string, error) {
				return run.workspace.Diff(ctx, run.entry.ID)
			},
		})
	}
	// The kill watch ends with the run on both paths, before terminal
	// assembly fires the escalation hook against a finished dispatch.
	watchStop()

	// Drop the injection target before the terminal status is published
	// below: relying on the deferred unregister alone left a window in
	// which a terminal entry still resolved to a live target and accepted
	// a message into a session whose run had ended. Not earlier: while
	// the result is assembled (diff capture can be slow) the entry still
	// reads running, and the registered target's refusal stays the
	// informative "no longer running" rather than the unknown-session
	// one. unregisterDispatchRun is idempotent; the deferred call above
	// stays for the early-return paths. The transported path returns only
	// once the served turn reached its terminal state, so the run has
	// ended here on both paths.
	c.unregisterDispatchRun(run.sessionID)
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

// dispatchNaturalOutcome is the path-neutral natural outcome of one
// dispatched run (#343): what the run did, before the kill and loop
// rules are applied. The direct path fills it from Run's (result, err);
// the transport path fills it from the DispatchTransportOutcome — so
// both paths assemble through one set of rules and read the same.
type dispatchNaturalOutcome struct {
	// completed reports whether the run finished its turn naturally —
	// false for failed, canceled, and runs that never started a turn.
	completed bool
	// findings is the run's final assistant text.
	findings string
	// runErr is the run's error; nil when the turn ran to a natural end
	// or was stopped by the loop-detection stop condition.
	runErr error
	// stoppedInLoop reports that the run ended on the loop-detection
	// stop condition, detected from the result's steps. The transport
	// path carries no steps: there a loop stop arrives as the kill
	// state's tool-loop reason instead, recorded in-process by the
	// served agent's observer.
	stoppedInLoop bool
	// diff resolves the run's diff-vs-base on demand. The direct path
	// captures in-process after the run; the transport path prefers the
	// diff that arrived on the wire and falls back to an in-process
	// capture when none did, so a capture error surfaces as "(diff
	// unavailable: ...)" instead of "(no changes)" (#361 puts the error
	// on the wire and deletes this).
	diff func(ctx context.Context) (string, error)
}

// assembleTerminalDispatchResult maps a finished dispatched run onto its
// terminal DispatchResult, preferring the kill outcome (#316) over the
// natural one: a run the ladder or watchdog killed — or one the
// loop-detection stop ended — is killed, and everything else falls
// through to the completed/failed mapping. A run that completed
// naturally before the kill's cancel took effect delivers its natural
// completion (the late kill is discarded).
func (c *coordinator) assembleTerminalDispatchResult(ctx context.Context, run dispatchRun, natural dispatchNaturalOutcome) dispatch.DispatchResult {
	if reason := run.kill.current(); reason != "" {
		// The kill's cancel either ended the run (error) or stopped it on
		// loop detection — recorded as the tool-loop kill reason by the
		// served agent's observer, or visible in the direct path's
		// result steps.
		if natural.runErr != nil || reason == dispatch.ReasonToolLoop || natural.stoppedInLoop {
			return c.assembleKilledDispatchResult(ctx, run, reason)
		}
	} else if natural.stoppedInLoop {
		// Loop detection's StopWhen ended the run in-process with no kill
		// recorded; the block records it as the tool-loop kill reason.
		return c.assembleKilledDispatchResult(ctx, run, dispatch.ReasonToolLoop)
	}
	return c.assembleDispatchResult(ctx, run, natural)
}

// dispatchRunStoppedInLoop reports whether a finished run ended on the
// loop-detection stop condition (#316's tool-loop kill reason): the same
// signature check the StopWhen in Run uses, applied to the result's
// steps.
func dispatchRunStoppedInLoop(result *fantasy.AgentResult) bool {
	if result == nil {
		return false
	}
	return hasRepeatedToolCalls(result.Steps, loopDetectionWindowSize, loopDetectionMaxRepeats)
}

// startDispatchKillWatch runs the time-based kill reasons (#316) for one
// dispatched run: the hard timeout kills outright; the todos stall window
// kills a run whose todo list has not been updated for the configured
// window while it keeps working. It returns a stop func that ends the
// watch; the deferred stop plus the explicit one after Run bound the
// watchdog to the run's lifetime.
func (c *coordinator) startDispatchKillWatch(ctx context.Context, run dispatchRun) (stop func()) {
	settings := run.killSettings
	if settings.HardTimeout <= 0 && settings.StallWindow <= 0 {
		return func() {}
	}

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	killFromWatch := func(reason string) {
		run.kill.kill(reason)
		slog.Warn("Dispatch run killed by watchdog", "dispatch_id", run.entry.ID, "session_id", run.sessionID, "reason", reason)
		run.agent.Cancel(run.sessionID)
	}

	if settings.HardTimeout > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timer := time.NewTimer(settings.HardTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				killFromWatch(dispatch.ReasonHardTimeout)
			case <-ctx.Done():
			}
		}()
	}

	if settings.StallWindow > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.watchTodosStall(ctx, run, settings.StallWindow, killFromWatch)
		}()
	}

	return func() {
		cancel()
		wg.Wait()
	}
}

// watchTodosStall polls the dispatched session's todo list and kills the
// run once an existing list goes untouched for the stall window ("stalled
// todos"). The poll cadence is a quarter of the window, clamped so tiny
// windows still poll and huge ones do not hammer the DB.
func (c *coordinator) watchTodosStall(ctx context.Context, run dispatchRun, window time.Duration, kill func(string)) {
	tick := window / 4
	if tick < 50*time.Millisecond {
		tick = 50 * time.Millisecond
	}
	if tick > 30*time.Second {
		tick = 30 * time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	fingerprint, ok := c.dispatchTodosFingerprint(ctx, run.sessionID)
	if !ok {
		return
	}
	stalledSince := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, ok := c.dispatchTodosFingerprint(ctx, run.sessionID)
		if !ok {
			continue
		}
		if current != fingerprint {
			fingerprint = current
			stalledSince = time.Now()
			continue
		}
		if time.Since(stalledSince) >= window {
			kill(dispatch.ReasonStalledTodos)
			return
		}
	}
}

// dispatchTodosFingerprint returns a comparable form of the session's
// current todo list, ok=false while the session has no todos (an absent
// list is the nudge ladder's problem, not a stall).
func (c *coordinator) dispatchTodosFingerprint(ctx context.Context, sessionID string) (string, bool) {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil || len(sess.Todos) == 0 {
		return "", false
	}
	b, err := json.Marshal(sess.Todos)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// assembleKilledDispatchResult builds the structured failure result a
// killed run delivers to its parent (#316): the kill reason, the run's
// last assistant state as the findings, and the salvageable diff-vs-base
// as the summary. The workspace is deliberately left alone: cleanup
// defers to the parent's re-dispatch-or-dismiss decision, and the
// session-end sweep remains the only automatic teardown.
func (c *coordinator) assembleKilledDispatchResult(ctx context.Context, run dispatchRun, reason string) dispatch.DispatchResult {
	terminal := dispatch.DispatchResult{
		DispatchID:    run.entry.ID,
		Branch:        run.entry.Branch,
		WorkspacePath: run.entry.Path,
		SessionID:     run.sessionID,
		Status:        dispatch.StatusKilled,
		KilledReason:  reason,
		KeyFindings:   c.dispatchLastAssistantText(ctx, run.sessionID),
	}
	// The handle was assigned after the dispatchRun's entry snapshot was
	// taken, so read it back from the registry like the natural result:
	// the killed payload is what the parent re-dispatches against.
	if entry, ok := run.workspace.Get(run.entry.ID); ok {
		terminal.Handle = entry.Handle
	}
	diff, diffErr := run.workspace.Diff(ctx, run.entry.ID)
	switch {
	case diffErr != nil:
		terminal.DiffSummary = fmt.Sprintf("(diff unavailable: %s)", diffErr)
	case diff == "":
		terminal.DiffSummary = "(no changes)"
	default:
		terminal.DiffSummary = dispatch.SummarizeDiff(diff)
	}
	return terminal
}

// dispatchLastAssistantText returns the dispatched session's final
// non-empty assistant text, the "last state" a killed run's parent
// reasons over when deciding whether to re-dispatch.
func (c *coordinator) dispatchLastAssistantText(ctx context.Context, sessionID string) string {
	msgs, err := c.messages.List(ctx, sessionID)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant {
			continue
		}
		if text := strings.TrimSpace(msg.Content().String()); text != "" {
			return text
		}
	}
	return ""
}

// assembleDispatchResult builds the terminal DispatchResult (#66) from a
// dispatched run's natural outcome: the outcome maps to the status, the
// agent's final text to the key findings, and the diff-vs-base —
// condensed — to the diff summary. A completed run stays completed even
// when diff capture fails; the failure is recorded in the summary so the
// main agent knows why it is missing.
func (c *coordinator) assembleDispatchResult(ctx context.Context, run dispatchRun, natural dispatchNaturalOutcome) dispatch.DispatchResult {
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
	case natural.runErr != nil:
		terminal.Status = dispatch.StatusFailed
		terminal.Error = natural.runErr.Error()
	case !natural.completed:
		terminal.Status = dispatch.StatusFailed
		terminal.Error = "agent session did not start a turn (busy or canceled)"
	default:
		terminal.Status = dispatch.StatusCompleted
		terminal.KeyFindings = natural.findings
		diff, diffErr := natural.diff(ctx)
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
