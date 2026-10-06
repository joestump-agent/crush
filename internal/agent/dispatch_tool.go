package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/shell"
	"github.com/google/uuid"
)

//go:embed templates/dispatch_tool.md
var dispatchToolDescription string

// DispatchAgentToolName is the registered name of the DispatchAgent tool.
const DispatchAgentToolName = "dispatch_agent"

// dispatchRetryBackoff paces the dispatch-delivery retry chain (#388):
// every failed delivery attempt re-pends and re-arms the flush, so a
// deterministic failure would otherwise loop back-to-back.
const dispatchRetryBackoff = 2 * time.Second

// dispatchResultPersistWindow bounds how long the run waits for the
// parent turn to persist the dispatch_agent tool result before stamping
// the terminal result onto it (#410): the run is launched before the
// tool returns, so a run that ends almost instantly can beat the write.
const dispatchResultPersistWindow = 5 * time.Second

// dispatchResultPersistPoll paces that wait.
const dispatchResultPersistPoll = 200 * time.Millisecond

// DispatchAgentParams are the DispatchAgent tool's arguments.
type DispatchAgentParams struct {
	Prompt string `json:"prompt" description:"Self-contained task instructions for the dispatched agent"`
	// Agent selects the agent definition the dispatch runs (#433): the
	// id of one of the resolved dispatch agents, stamped onto the
	// parameter schema's enum at Info time by dispatchAgentTool. Empty
	// runs the configured default agent (options.dispatch.default_agent,
	// "worker" unless configured).
	Agent string `json:"agent,omitempty" description:"Agent definition to run the dispatch on: one of the dispatch agent ids in the enum. Omit to use the configured default agent"`
	// Model is the model type the dispatched agent runs on — "large" or
	// "small" — defaulting to the selected agent definition's slot (#433;
	// the worker's is the small model). Per-dispatch model choice is
	// by selected-model type, not raw model ID, matching how agent
	// configs select models. A definition pinned to an explicit model
	// refuses the parameter with a tool error.
	Model string `json:"model,omitempty" description:"Model type to run the dispatched agent on: \"large\" or \"small\" (default: the selected agent's configured model slot; refused for agents pinned to an explicit model)"`
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

// dispatchSweepTimeout bounds the session-end ReleaseDispatches: it runs
// git commands against every workspace, and a wedged worktree must not
// hang shutdown.
const dispatchSweepTimeout = 30 * time.Second

// dispatchAgentOptions configures the dispatched-agent constructor.
type dispatchAgentOptions struct {
	// Toolchain is the workspace-rooted toolchain the agent runs with
	// (#62); its Tools, Config, and WorkingDir feed the agent build.
	Toolchain *DispatchToolchain
	// AgentID is the id of the resolved agent definition the dispatch
	// runs (#433); the builder renders its prompt and model. Empty
	// means the worker definition.
	AgentID string
	// ModelType selects which selected model the dispatched agent runs
	// on. The default (empty) is the agent definition's slot (#432).
	ModelType config.SelectedModelType
	// Skills restricts the rendered available-skills set; empty means
	// every skill discovered in the workspace.
	Skills []string
	// TodoKill is the wander-kill escalation's supervisor hook (#316):
	// invoked when the dispatched run ignores its nudges past the kill
	// threshold. The observer owns the kill (#348): it records the
	// reason against this dispatch and routes the kill through the
	// served run's tasks/cancel, with the direct cancel as the fallback.
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
	// done is closed by the first kill (#352), so a wait on the user —
	// a parked question — ends with the run. Made on first use.
	done chan struct{}
}

func (k *dispatchKill) kill(reason string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	// An empty reason records nothing, as before; the first real one
	// wins and closes done exactly once.
	if k.reason == "" && reason != "" {
		k.reason = reason
		close(k.doneLocked())
	}
}

// killed returns a channel closed by the first kill; a nil kill's never
// closes.
func (k *dispatchKill) killed() <-chan struct{} {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.doneLocked()
}

// doneLocked returns the kill's done channel, making it on first use.
// k.mu must be held.
func (k *dispatchKill) doneLocked() chan struct{} {
	if k.done == nil {
		k.done = make(chan struct{})
	}
	return k.done
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

// dispatchFindings is one dispatched run's turn-text record (#397): the
// work turn's findings and the replies of steers that arrived as
// follow-up turns, kept apart so a steer accepted while the final step
// was streaming cannot replace the findings. The mutex guards both:
// turn observers fire from the run's turns while the terminal assembly
// reads the record.
type dispatchFindings struct {
	mu       sync.Mutex
	workText string
	replies  []string
}

// setWork records the work turn's text, last write wins: a summarize
// continuation re-queues the same call, so the continuation's finished
// text replaces the cut turn's.
func (f *dispatchFindings) setWork(text string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.workText = text
	f.mu.Unlock()
}

// addSteerReply appends one steer turn's reply, in turn order.
func (f *dispatchFindings) addSteerReply(text string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.replies = append(f.replies, text)
	f.mu.Unlock()
}

// work returns the last recorded work-turn text, empty when no work turn
// finished (a nil receiver is an unrecorded run).
func (f *dispatchFindings) work() string {
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workText
}

// steerReplies returns a copy of the recorded steer replies, nil when
// none landed (a nil receiver is an unrecorded run).
func (f *dispatchFindings) steerReplies() []string {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.replies...)
}

// dispatchRun carries one backgrounded dispatch from the tool call that
// started it to the goroutine that runs it.
type dispatchRun struct {
	reg         *dispatch.AgentRegistry
	provider    *dispatch.GitWorktreeProvider
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
	// cancel ends the dispatch's root context (#371). The watchdog's
	// kill fires it so a kill landing before the dispatched agent's Run
	// registered the session still ends the run: agent.Cancel alone is
	// a no-op for an unregistered session (#430). Nil where a run is
	// driven without a root (tests that assemble or observe only).
	cancel context.CancelFunc
	// holdsSlot reports that the run holds the dispatch concurrency slot
	// its tool call reserved (#390): the run's teardown releases it when
	// the dispatch reaches a terminal state. Runs driven without a
	// reservation (tests that call runDispatch directly) do not.
	holdsSlot bool
	// killSettings are the resolved wander-kill thresholds for this
	// dispatch: nudges-before-kill, todos stall window, hard timeout.
	killSettings config.TodoEnforcementSettings
	// findings records the run's work-turn text and steer replies
	// (#397): a pointer, because the dispatchRun value is copied into
	// the background run and both copies must name the same record.
	// Nil where a run is driven without the record (tests that assemble
	// only); the accessors are nil-safe.
	findings *dispatchFindings
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

// cancelDispatchesForShutdown is CancelAll's dispatch phase (#372): the
// agent cancel before it reaches only the main agent, and every
// dispatched agent runs on its own detached context, so quitting Crush
// must cancel them here. Each live dispatch records the shutdown kill
// reason and takes the same graceful agent.Cancel path the watchdog
// uses, then the phase waits one shared bound for every run's teardown
// to close its done channel. A dispatch still running when the bound
// expires gets its root cancel, which also unblocks a transport-path
// client stream, and one more shared bound; anything that outlives that
// is logged with its dispatch ID and left to its run goroutine.
func (c *coordinator) cancelDispatchesForShutdown() {
	// Raise the flag before touching any dispatch: from here on a
	// finishing dispatch records its terminal state but starts no
	// parent delivery turn.
	c.shuttingDown.Store(true)

	type shutdownTarget struct {
		id   string
		live *liveDispatch
	}
	c.dispatchMu.Lock()
	targets := make([]shutdownTarget, 0, len(c.liveDispatches))
	for id, live := range c.liveDispatches {
		targets = append(targets, shutdownTarget{id: id, live: live})
	}
	c.dispatchMu.Unlock()
	if len(targets) == 0 {
		return
	}

	cancelWait, rootWait := c.dispatchShutdownWait, c.dispatchShutdownRootWait
	if cancelWait <= 0 {
		cancelWait = 5 * time.Second
	}
	if rootWait <= 0 {
		rootWait = time.Second
	}

	for _, target := range targets {
		if target.live.kill != nil {
			target.live.kill.kill(dispatch.ReasonShutdown)
		}
		target.live.agent.Cancel(target.live.sessionID)
	}

	deadline := time.NewTimer(cancelWait)
	defer deadline.Stop()
	var remaining []shutdownTarget
waitLoop:
	for i, target := range targets {
		select {
		case <-target.live.done:
		case <-deadline.C:
			remaining = targets[i:]
			break waitLoop
		}
	}
	if len(remaining) == 0 {
		return
	}

	rootDeadline := time.NewTimer(rootWait)
	defer rootDeadline.Stop()
	for _, target := range remaining {
		if target.live.cancel != nil {
			target.live.cancel()
		}
	}
	var stuck []shutdownTarget
	for _, target := range remaining {
		select {
		case <-target.live.done:
		case <-rootDeadline.C:
			stuck = append(stuck, target)
		}
	}
	for _, target := range stuck {
		slog.Warn("Dispatch still running after shutdown cancel", "dispatch_id", target.id, "session_id", target.live.sessionID)
	}
}

// reserveDispatchSlot checks the resolved dispatch.max_concurrent cap
// (#390) against the dispatches currently holding a slot — live
// dispatches plus setups still in flight — and reserves one more under
// the same lock, so parallel tool calls within one step cannot overshoot
// the cap. It reports whether the slot was reserved, and how many
// dispatches hold slots when the cap refused.
func (c *coordinator) reserveDispatchSlot() (bool, int) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	limit := c.cfg.Config().Options.GetDispatchMaxConcurrent()
	if c.dispatchSlots >= limit {
		return false, c.dispatchSlots
	}
	c.dispatchSlots++
	return true, 0
}

// releaseDispatchSlot frees a dispatch slot held by a setup that failed
// before its run took ownership, or by a run that reached a terminal
// state (#390). Each reservation is released exactly once; the clamp
// keeps a spurious release from drifting the counter negative and
// silently raising the cap.
func (c *coordinator) releaseDispatchSlot() {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if c.dispatchSlots > 0 {
		c.dispatchSlots--
	}
}

// call builds the full SessionAgentCall a dispatch's turns run with:
// the chosen model's shaping, the parent turn's content width, and the
// non-interactive flag. Built per consumer — the server stamps its
// executor's template with it at start (#71), and the transport and the
// injection queue clone it at run time.
func (r dispatchRun) call(c *coordinator) SessionAgentCall {
	maxTokens := r.model.CatwalkCfg.DefaultMaxTokens
	if r.model.ModelCfg.MaxTokens != 0 {
		maxTokens = r.model.ModelCfg.MaxTokens
	}
	call := SessionAgentCall{
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
	// The per-turn observer (#397), routed by the turn's own call: the
	// work turn reports into the findings; a steer turn — folded or the
	// follow-up — records its reply in the steers' record instead of
	// replacing the work's. The route rides the call template so the
	// steering path (#351) needs no per-injection registration.
	call.turnText = func(turned SessionAgentCall, text string) {
		if turned.Steer {
			r.findings.addSteerReply(text)
			return
		}
		r.findings.setWork(text)
	}
	return call
}

// dispatchAgentParam is the DispatchAgent tool parameter that names the
// agent definition to run (#433).
const dispatchAgentParam = "agent"

// dispatchAgentTool wraps the DispatchAgent tool so Info can inject the
// live agent enum (#433): fantasy derives the parameter schema from the
// params struct's static tags, so the dispatchable agent ids — resolved
// from the current config at Info time — are stamped onto the agent
// parameter here. Everything else delegates to the inner tool.
type dispatchAgentTool struct {
	inner fantasy.AgentTool
	c     *coordinator
}

func (t *dispatchAgentTool) Info() fantasy.ToolInfo {
	info := t.inner.Info()
	if param, ok := info.Parameters[dispatchAgentParam].(map[string]any); ok {
		param["enum"] = t.c.dispatchableAgentIDs()
		param["description"] = fmt.Sprintf(
			"Agent definition to run the dispatch on: one of the dispatch agent ids in the enum. Omit to use the configured default agent (%s)",
			t.c.cfg.Config().Options.GetDispatchDefaultAgent(),
		)
	}
	return info
}

func (t *dispatchAgentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.inner.Run(ctx, call)
}

func (t *dispatchAgentTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *dispatchAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

// dispatchableAgentIDs returns the sorted ids of the resolved agents a
// dispatch may run: role dispatch, not disabled, builtin runtime. a2a
// agents join once #434 serves remote agents.
func (c *coordinator) dispatchableAgentIDs() []string {
	ids := make([]string, 0, 4)
	for id, agentCfg := range c.cfg.Config().Agents {
		if agentCfg.Role != config.AgentRoleDispatch || agentCfg.Disabled || agentCfg.Runtime != config.AgentRuntimeBuiltin {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// resolveDispatchAgent maps the tool call's agent parameter onto the
// resolved agent definition it names (#433): an empty parameter selects
// options.dispatch.default_agent, and an unknown, disabled, non-dispatch,
// or non-builtin id is refused with the valid ids listed.
func (c *coordinator) resolveDispatchAgent(id string) (config.Agent, string) {
	if id == "" {
		id = c.cfg.Config().Options.GetDispatchDefaultAgent()
	}
	valid := c.dispatchableAgentIDs()
	refuse := func(format string, args ...any) (config.Agent, string) {
		return config.Agent{}, fmt.Sprintf("%s; available dispatch agents: %s",
			fmt.Sprintf(format, args...), strings.Join(valid, ", "))
	}
	agentCfg, ok := c.cfg.Config().Agents[id]
	if !ok {
		return refuse("unknown agent %q", id)
	}
	if agentCfg.Disabled {
		return refuse("agent %q is disabled", id)
	}
	if agentCfg.Role != config.AgentRoleDispatch {
		return refuse("agent %q is a %q agent, not a dispatch agent", id, agentCfg.Role)
	}
	if agentCfg.Runtime != config.AgentRuntimeBuiltin {
		return refuse("agent %q runs on the %q runtime, which dispatch_agent cannot serve yet", id, agentCfg.Runtime)
	}
	return agentCfg, ""
}

// dispatchTool builds the DispatchAgent tool (#64): provision a clean
// workspace (#63), bootstrap the dispatched agent's toolchain rooted at
// it (#62), run a backgrounded SessionAgent on an ephemeral session
// (#48/#50), and return a running handle immediately so the main agent
// keeps working. The tool is wrapped so Info can stamp the dispatchable
// agent ids onto its schema (#433).
func (c *coordinator) dispatchTool() fantasy.AgentTool {
	inner := fantasy.NewParallelAgentTool(
		DispatchAgentToolName,
		dispatchToolDescription,
		func(ctx context.Context, params DispatchAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			// Every dispatch runs behind the A2A host (#347): without one
			// wired there is no execution path, so refuse before
			// provisioning anything.
			if c.a2aHost() == nil {
				return fantasy.NewTextErrorResponse("dispatch unavailable: no A2A host"), nil
			}

			// The chosen agent definition (#433): the call's agent
			// parameter, else the configured default. Unknown, disabled,
			// non-dispatch, and non-builtin ids are refused before
			// anything is provisioned.
			agentCfg, refusal := c.resolveDispatchAgent(params.Agent)
			if refusal != "" {
				return fantasy.NewTextErrorResponse(refusal), nil
			}

			modelType := config.SelectedModelType(params.Model)
			switch modelType {
			case "", config.SelectedModelTypeLarge, config.SelectedModelTypeSmall:
				// Empty defaults to the selected definition's slot.
			default:
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid model %q: must be \"large\" or \"small\"", params.Model)), nil
			}
			// A pinned definition runs on its own provider and model, so
			// a slot choice has nothing to apply to: refuse it rather
			// than silently ignoring it (#433).
			if modelType != "" && agentCfg.ModelRef != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"agent %q pins model %s/%s; omit model",
					agentCfg.ID, agentCfg.ModelRef.Provider, agentCfg.ModelRef.Model)), nil
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			// The parent's deny list decides what a dispatch may run
			// with (#376): the worktree's own config must not widen it
			// (#374). When the deny list removes every write tool there
			// is nothing a dispatch can do, so refuse before provisioning
			// a workspace. The check is on user policy, not the worker's
			// palette: a definition that is read-only (#432) still
			// dispatches.
			disabled := c.cfg.Config().Options.DisabledTools
			if !slices.ContainsFunc(dispatchCapabilityTools, func(name string) bool {
				return !slices.Contains(disabled, name)
			}) {
				return fantasy.NewTextErrorResponse("dispatch unavailable: bash/edit/write are disabled by your configuration (disabled_tools / permissions deny)"), nil
			}

			provider, err := c.dispatchWorkspaceProvider()
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("dispatch unavailable: %s", err)), nil
			}
			reg := c.dispatchRegistry()

			// The concurrency cap (#390): count the slots in flight —
			// live dispatches plus setups still provisioning — and
			// reserve one more under the same lock, so parallel tool
			// calls within one step cannot overshoot the cap. At the
			// cap, refuse before provisioning anything. The slot is
			// released by the setup-failure paths below, or by the run's
			// teardown once the dispatch reaches a terminal state.
			reserved, running := c.reserveDispatchSlot()
			if !reserved {
				limit := c.cfg.Config().Options.GetDispatchMaxConcurrent()
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"dispatch at capacity: %d agents are already running (dispatch.max_concurrent=%d); wait for one to finish or cancel one",
					running, limit)), nil
			}

			// Provision, bootstrap, and build while the tool call is
			// still open: every step is local and fast, and failures
			// here are actionable tool errors rather than silent
			// background failures. The provider owns the directory; the
			// registry owns the entry, registered here so every setup
			// failure below tears both down.
			id := uuid.NewString()
			placement, err := provider.Provision(ctx, id, dispatch.ProvisionOptions{Base: params.Branch})
			if err != nil {
				c.releaseDispatchSlot()
				return fantasy.NewTextErrorResponse(fmt.Sprintf("provision dispatch workspace: %s", err)), nil
			}
			entry := dispatch.Entry{
				ID:      id,
				Path:    placement.Path,
				Branch:  placement.Branch,
				Base:    placement.Base,
				BaseSHA: placement.BaseSHA,
				Agent:   agentCfg.ID,
				Status:  dispatch.StatusProvisioned,
			}
			reg.Register(entry)

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
			toolchain, err := c.BuildDispatchToolchain(rootCtx, DispatchToolchainOptions{WorkingDir: entry.Path, Agent: agentCfg.ID})
			if err != nil {
				rootCancel()
				c.releaseDispatchSlot()
				c.removeDispatch(ctx, reg, provider, entry, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("build dispatch toolchain: %s", err)), nil
			}

			if unknown := missingSkills(toolchain.Config(), params.Skills); len(unknown) > 0 {
				rootCancel()
				c.releaseDispatchSlot()
				c.removeDispatch(ctx, reg, provider, entry, toolchain)
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
			killSettings := c.dispatchEnforcement(agentCfg)
			dispatched, err := builder(ctx, dispatchAgentOptions{
				Toolchain: toolchain,
				AgentID:   agentCfg.ID,
				ModelType: modelType,
				Skills:    params.Skills,
				// The ladder's kill rung (#316) reports here; the
				// supervisor owns the kill (#348): the reason rides a
				// tasks/cancel to the served dispatch, with the direct
				// cancel as the fallback while the task ID is still
				// unknown. The dispatched agent's ladder closure no
				// longer cancels itself.
				TodoKill: func(sessionID string, reason string) {
					kill.kill(reason)
					c.killDispatch(reg, entry.ID, reason, nil)
				},
				LoopStop: func(sessionID string) {
					kill.kill(dispatch.ReasonToolLoop)
				},
			})
			if err != nil {
				rootCancel()
				c.releaseDispatchSlot()
				c.removeDispatch(ctx, reg, provider, entry, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("build dispatched agent: %s", err)), nil
			}

			// One dispatch = one ephemeral task session (#48), keyed by
			// the tool call like the agent tool's sub-sessions.
			taskSessionID := c.sessions.CreateAgentToolSessionID(agentMessageID, call.ID)
			taskSession, err := c.sessions.CreateTaskSession(ctx, taskSessionID, sessionID, "Dispatched Agent")
			if err != nil {
				rootCancel()
				c.releaseDispatchSlot()
				c.removeDispatch(ctx, reg, provider, entry, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("create session: %s", err)), nil
			}

			// Keep the registry current from here on: #65's status card
			// and #313's handles read these entries. The handle is assigned
			// after the session so the assignment event carries the complete
			// entry — handle, role, session, running state.
			reg.SetSession(entry.ID, taskSession.ID)
			reg.SetParentSessionID(entry.ID, sessionID)
			reg.SetStatus(entry.ID, dispatch.StatusRunning)
			assignedHandle, ok := reg.AssignHandle(entry.ID, params.Handle, params.Role)
			if !ok {
				rootCancel()
				c.releaseDispatchSlot()
				c.removeDispatch(ctx, reg, provider, entry, toolchain)
				return fantasy.NewTextErrorResponse("assign dispatch handle: registry entry vanished"), nil
			}
			// Stamp the identity onto the owner marker too (#369): the
			// registry dies with the process, the marker outlives it,
			// and list and salvage tooling read the marker. Best-effort:
			// a failed stamp costs a blank column, not a dispatch.
			if err := provider.UpdateOwnerIdentity(entry.ID, assignedHandle, params.Role); err != nil {
				slog.Warn("Failed to stamp dispatch handle on owner marker", "dispatch_id", entry.ID, "error", err)
			}

			run := dispatchRun{
				reg:             reg,
				provider:        provider,
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
				cancel:          rootCancel,
				findings:        &dispatchFindings{},
				holdsSlot:       true,
			}

			// Stand up the dispatch's in-process A2A server (#70) and stamp
			// its endpoint and card on the registry entry — the in-memory
			// discovery surface. The executor's served turns run with the
			// dispatch's own call shaping (#71); the server dies with the
			// run, and runDispatch owns the stop. The inactivity backstop
			// (#360) is armed with the dispatch's resolved enforcement
			// settings, and the kill's reason (#316) rides along (#342):
			// an out-of-band kill surfaces on the A2A task as a Canceled
			// status carrying it. The usage closure (#364) reads the
			// dispatched session's final totals once the run ends, so the
			// terminal status carries them and the parent can account for
			// the run without sharing a database row. A start failure is a
			// dispatch failure (#347): tear the dispatch down and report
			// the tool error — nothing runs unserved.
			usage := func(ctx context.Context) (Usage, error) {
				sess, err := c.sessions.Get(ctx, taskSession.ID)
				if err != nil {
					return Usage{}, fmt.Errorf("get dispatch session: %w", err)
				}
				return Usage{
					Model:            dispatched.model.ModelCfg.Model,
					Provider:         dispatched.model.ModelCfg.Provider,
					PromptTokens:     sess.PromptTokens,
					CompletionTokens: sess.CompletionTokens,
					Cost:             sess.Cost,
				}, nil
			}
			stopServer, err := c.startDispatchServer(ctx, provider, reg, entry.ID, taskSession.ID, assignedHandle, params.Role, dispatched.agent, resolvedSkills(toolchain.Config(), params.Skills), run.call(c), run.killSettings.InactivityTimeout, run.kill.current, usage, toolchain.Questions())
			if err != nil {
				rootCancel()
				c.releaseDispatchSlot()
				c.removeDispatch(ctx, reg, provider, entry, toolchain)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("start A2A server: %s", err)), nil
			}
			run.stopServer = stopServer

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

			// The running handle is the start of the durable record
			// (#355): the row exists before the run starts, so a crash
			// at any later point leaves something to reconcile.
			// Best-effort — a failed write never breaks a live dispatch.
			handle := dispatch.DispatchResult{
				DispatchID:    entry.ID,
				Handle:        assignedHandle,
				Agent:         entry.Agent,
				Branch:        entry.Branch,
				WorkspacePath: entry.Path,
				SessionID:     taskSession.ID,
				Status:        dispatch.StatusRunning,
			}
			if c.dispatchRecords != nil {
				if err := c.dispatchRecords.RecordStarted(handle, sessionID); err != nil {
					slog.Warn("Failed to record dispatch start", "dispatch_id", entry.ID, "error", err)
				}
			}
			c.startDispatchRun(func() { c.runDispatch(rootCtx, run) })

			return fantasy.NewTextResponse(handle.Render()), nil
		},
	)
	return &dispatchAgentTool{inner: inner, c: c}
}

// dispatchEnforcement resolves the enforcement ladder a dispatched
// agent runs (#402): the chosen dispatch agent definition's todos and
// kill blocks (#433) layered over the global options, with the built-in
// defaults underneath. The run's watchdog killSettings and the agent's
// own ladder read these same resolved settings, so a kill threshold
// configured on the definition trips in both places or neither.
func (c *coordinator) dispatchEnforcement(agentCfg config.Agent) config.TodoEnforcementSettings {
	return agentCfg.ResolvedTodoEnforcement(c.cfg.Config().Options.TodoEnforcement)
}

// buildDispatchedAgent constructs the agent a dispatch runs: the chosen
// selected model (the definition's slot by default) — an explicit pin
// wins (#432) — a system prompt rendered at dispatch time from the
// definition's prompt against the workspace's scoped store (template +
// dispatch context: git status, context files, skills), and the
// workspace-rooted toolchain's tools. The definition is the one the
// dispatch chose (#433): opts.AgentID, else the worker.
func (c *coordinator) buildDispatchedAgent(ctx context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
	large, small, err := c.buildAgentModels(ctx, true)
	if err != nil {
		return nil, err
	}

	agentID := opts.AgentID
	if agentID == "" {
		agentID = config.AgentWorker
	}
	agentCfg, ok := c.cfg.Config().Agents[agentID]
	if !ok {
		return nil, fmt.Errorf("agent %q not configured", agentID)
	}
	// The session agent runs on its "large" slot, so the chosen model
	// goes there; the small model stays available for auxiliary work.
	// The dispatch's model parameter picks the slot, defaulting to the
	// definition's slot. An explicit pin on the definition (#432) wins
	// over both: a pinned agent runs every dispatch on its own provider
	// and model. The tool refuses the parameter on a pin (#433); this
	// fallback keeps the builder safe when it is driven directly.
	if opts.ModelType != "" && agentCfg.ModelRef == nil {
		agentCfg.Model = opts.ModelType
	}
	model, err := c.agentModel(ctx, agentCfg, large, small, true)
	if err != nil {
		return nil, err
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	// The dispatched prompt renders from the chosen definition (#433):
	// builtin:dispatch by default, the definition's file: template when
	// it names one, against the scoped workspace store. The dispatch's
	// requested skills narrow the definition's set — a dispatch asks
	// for fewer skills, never more.
	promptOpts := []prompt.Option{prompt.WithWorkingDir(opts.Toolchain.WorkingDir())}
	promptOpts = append(promptOpts, agentPromptOptions(agentCfg)...)
	requestedSkills := opts.Skills
	if len(agentCfg.Skills) > 0 {
		if len(requestedSkills) == 0 {
			requestedSkills = agentCfg.Skills
		} else {
			requestedSkills = intersectSkills(requestedSkills, agentCfg.Skills)
		}
	}
	if len(requestedSkills) > 0 {
		promptOpts = append(promptOpts, prompt.WithSkills(requestedSkills))
	}
	systemPromptTemplate, err := agentPrompt(agentCfg, c.cfg.WorkingDir(), promptOpts...)
	if err != nil {
		return nil, err
	}
	rendered, err := agentSystemPrompt(ctx, systemPromptTemplate, agentCfg, c.cfg.WorkingDir(), model.Model.Provider(), model.Model.Model(), opts.Toolchain.Config())
	if err != nil {
		return nil, fmt.Errorf("render dispatch system prompt: %w", err)
	}

	// Prompt and tools are known at construction, so the agent's
	// readiness latch is satisfied immediately (newSessionAgent) — no
	// build-time goroutines to wait for. The todo enforcement ladder
	// (#315) resolves from the chosen dispatch agent definition (#402):
	// its todos and kill blocks over the global options.
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
		TodoEnforcement:      c.dispatchEnforcement(agentCfg),
		// The dispatched agent is the one agent whose run may be killed
		// (#316): the observers hand the reasons to the coordinator's kill
		// state so the terminal result carries them — the ladder's
		// escalation and the loop-detection stop alike (#343).
		TodoKill: opts.TodoKill,
		LoopStop: opts.LoopStop,
	})

	return &dispatchedAgent{agent: agent, model: model, providerCfg: providerCfg}, nil
}

// startDispatchRun starts one dispatch's background run. Tests install
// spawnDispatch to track every run they start, so none can be racing the
// test's cleanup (#422); production starts the goroutine directly.
func (c *coordinator) startDispatchRun(run func()) {
	if c.spawnDispatch != nil {
		c.spawnDispatch(run)
		return
	}
	go run()
}

// runDispatch runs one dispatched agent to completion in the background
// (#64): it drives the ephemeral session's turn, keeps the registry's
// status current, propagates the dispatched session's cost to the parent,
// and delivers the terminal DispatchResult back to the main agent (#66).
// The wander-kill watchdog (#316) runs alongside the turn and cancels it
// deterministically when a kill threshold trips.
func (c *coordinator) runDispatch(ctx context.Context, run dispatchRun) {
	defer func() {
		// Backstop for the kill after the run returns (#385): a run that
		// ends before its terminal result is assembled still must not
		// leave its background jobs running in the workspace.
		c.killDispatchSessionJobs(ctx, run)
		// The A2A server dies with the run (#70): teardown clears the
		// registry's endpoint and card too, so discovery never hands out
		// a dead endpoint. It stops before the toolchain closes, which
		// ends the shared process-wide resources underneath it.
		if run.stopServer != nil {
			c.stopDispatchServer(run.reg, run.entry.ID, run.stopServer)
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
		// The dispatch's concurrency slot (#390) is held from the tool
		// call's reservation to here, when the dispatch has reached its
		// terminal state.
		if run.holdsSlot {
			c.releaseDispatchSlot()
		}
	}()

	watchStop := c.startDispatchKillWatch(ctx, run)
	defer watchStop()

	// One execution path (#347): the served dispatch is driven through
	// the A2A client, its SSE stream consumed to the terminal state.
	// Steering rides the same path (#351): the server's executor resolves
	// the run's binding from the task's A2A context and enqueues the
	// message on this very agent.
	terminal, servedUsage := c.runDispatchOverTransport(ctx, run)
	// The kill watch ends with the run, before terminal assembly fires
	// the escalation hook against a finished dispatch.
	watchStop()

	// Record the terminal payload before the terminal status so the
	// terminal entry event carries it: the completed agent block (#65)
	// renders its durable record from the registry.
	run.reg.SetResult(run.entry.ID, terminal)
	run.reg.SetStatus(run.entry.ID, terminal.Status)
	// The durable record (#355): the terminal payload lands in SQLite so
	// a restart can re-deliver it if the parent's delivery turn never
	// runs. A row reconcile already failed keeps that outcome; the write
	// is best-effort.
	if c.dispatchRecords != nil {
		if err := c.dispatchRecords.RecordTerminal(terminal); err != nil {
			slog.Warn("Failed to record dispatch terminal result", "dispatch_id", terminal.DispatchID, "error", err)
		}
	}

	// Stamp the terminal result onto the parent's persisted dispatch_agent
	// tool result (#410): the card's durable record after a restart or in
	// client/server mode, where the in-memory registry is unreachable.
	c.persistDispatchTerminalResult(ctx, run, terminal)

	// Cost propagation (#364): a served dispatch reports its usage on
	// the wire, and the parent applies it with one atomic UPDATE — no
	// child-row copy, which is what lets an out-of-process or remote
	// dispatched agent (#72/#73) report cost at all. A dispatch whose
	// usage is missing or undecodable leaves the parent's cost
	// untouched. This is best-effort, mirroring runSubAgent: a failure
	// here must not lose the run's outcome.
	if servedUsage != nil {
		if err := c.sessions.AddSessionUsage(ctx, run.parentSessionID, servedUsage.PromptTokens, servedUsage.CompletionTokens, servedUsage.Cost); err != nil {
			slog.Warn("Failed to add dispatched usage to parent session", "child_session", run.sessionID, "parent_session", run.parentSessionID, "error", err)
		}
	} else {
		slog.Warn("Served dispatch carried no usage; parent cost unchanged", "child_session", run.sessionID, "parent_session", run.parentSessionID)
	}

	c.deliverDispatchResult(ctx, run.parentSessionID, terminal)
}

// persistDispatchTerminalResult records the terminal DispatchResult on
// the parent session's persisted dispatch_agent tool result (#410):
// the result lands in ToolResult.Metadata as JSON, Content stays the
// running handle the model already saw, so the tool result the model
// reads is byte-identical before and after completion. The card parses
// Metadata in preference to Content, which is what makes a finished
// dispatch render its terminal state after a restart and in
// client/server mode, where the in-memory registry is unreachable.
//
// The parent turn persists the tool result after dispatchTool returns,
// and the run is launched before that, so a run that ends almost
// instantly can race the write. The wait is bounded: on giveup the
// terminal record stays only in the registry and the delivery turn,
// logged at Warn.
func (c *coordinator) persistDispatchTerminalResult(ctx context.Context, run dispatchRun, terminal dispatch.DispatchResult) {
	_, toolCallID, ok := c.sessions.ParseAgentToolSessionID(run.sessionID)
	if !ok {
		slog.Warn("Cannot persist dispatch terminal result: session is not an agent tool session", "session_id", run.sessionID, "dispatch_id", run.entry.ID)
		return
	}
	b, err := json.Marshal(terminal)
	if err != nil {
		slog.Warn("Failed to encode dispatch terminal result", "dispatch_id", run.entry.ID, "error", err)
		return
	}
	metadata := string(b)

	deadline := time.Now().Add(dispatchResultPersistWindow)
	for {
		if ctx.Err() != nil {
			return
		}
		msgs, err := c.messages.List(ctx, run.parentSessionID)
		if err != nil {
			slog.Warn("Failed to list parent session for dispatch terminal result", "parent_session", run.parentSessionID, "dispatch_id", run.entry.ID, "error", err)
			return
		}
		for i, msg := range msgs {
			if msg.Role != message.Tool {
				continue
			}
			stamped := false
			for j, part := range msg.Parts {
				tr, ok := part.(message.ToolResult)
				if !ok || tr.ToolCallID != toolCallID || tr.Metadata == metadata {
					continue
				}
				tr.Metadata = metadata
				msg.Parts[j] = tr
				stamped = true
			}
			if !stamped {
				continue
			}
			if err := c.messages.Update(ctx, msgs[i]); err != nil {
				slog.Warn("Failed to persist dispatch terminal result", "parent_session", run.parentSessionID, "dispatch_id", run.entry.ID, "error", err)
				return
			}
			// The service may debounce updates; flush so any later read
			// (a session reload, the client's event stream) sees it now.
			if err := c.messages.Flush(ctx, msg.ID); err != nil {
				slog.Debug("Failed to flush dispatch terminal result update", "message_id", msg.ID, "error", err)
			}
			return
		}
		if !time.Now().Before(deadline) {
			slog.Warn("Gave up waiting for the parent's dispatch tool result", "parent_session", run.parentSessionID, "dispatch_id", run.entry.ID, "tool_call_id", toolCallID, "waited", dispatchResultPersistWindow)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(dispatchResultPersistPoll):
		}
	}
}

// dispatchNaturalOutcome is the natural outcome of one dispatched run
// (#343): what the run did, before the kill and loop rules are applied.
// The transport fills it via dispatchNaturalOutcomeFromTransport — the
// only execution path since #347.
type dispatchNaturalOutcome struct {
	// completed reports whether the run finished its turn naturally —
	// false for failed, canceled, and runs that never started a turn.
	completed bool
	// findings is the run's final assistant text. A steer's follow-up
	// turn text may arrive here as the last turn's; assembleDispatchResult
	// prefers the run's findings record over it (#397).
	findings string
	// runErr is the run's error; nil when the turn ran to a natural end.
	runErr error
	// diff resolves the run's diff-vs-base on demand: the transport
	// prefers the diff that arrived on the wire and falls back to an
	// in-process capture when none did, so a capture error surfaces as
	// "(diff unavailable: ...)" instead of "(no changes)" (#361 puts the
	// error on the wire and deletes this).
	diff func(ctx context.Context) (string, error)
	// killReason is a kill reason the transport outcome carried (#348):
	// a terminal Canceled whose status message is one of the kill
	// reasons. The in-process kill state is the first witness and wins;
	// this is the mapping for a kill this process never recorded — an
	// out-of-process supervisor (#72/#73) that killed the served task
	// directly. Empty unless the wire said killed.
	killReason string
}

// assembleTerminalDispatchResult maps a finished dispatched run onto its
// terminal DispatchResult, preferring the kill outcome (#316) over the
// natural one: a run the ladder or watchdog killed — or one the served
// agent's loop observer stopped — is killed, and everything else falls
// through to the completed/failed mapping. A run that completed
// naturally before the kill's cancel took effect delivers its natural
// completion (the late kill is discarded).
func (c *coordinator) assembleTerminalDispatchResult(ctx context.Context, run dispatchRun, natural dispatchNaturalOutcome) dispatch.DispatchResult {
	if reason := run.kill.current(); reason != "" {
		// The kill's cancel either ended the run (error) or stopped it on
		// loop detection — the loop reason is recorded in-process by the
		// served agent's observer.
		if natural.runErr != nil || reason == dispatch.ReasonToolLoop {
			return c.assembleKilledDispatchResult(ctx, run, reason)
		}
	} else if natural.killReason != "" {
		// A kill this process never recorded, reported by the wire
		// (#348): the served task's Canceled status message is a kill
		// reason, so the parent reads killed with it.
		return c.assembleKilledDispatchResult(ctx, run, natural.killReason)
	}
	return c.assembleDispatchResult(ctx, run, natural)
}

// killDispatchSessionJobs kills the run's background jobs — the ones its
// bash calls tagged with the dispatch session's ID — and logs how many
// it killed at Debug when non-zero. The main agent's jobs carry the main
// session's ID, as do other dispatches', so only this run's jobs are
// touched (#385).
func (c *coordinator) killDispatchSessionJobs(ctx context.Context, run dispatchRun) {
	killed := shell.GetBackgroundShellManager().KillSession(ctx, run.sessionID)
	if killed > 0 {
		slog.Debug("Killed dispatch background jobs", "dispatch_id", run.entry.ID, "session_id", run.sessionID, "killed", killed)
	}
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
		// The kill travels the protocol (#348): a tasks/cancel carrying
		// the reason ends the served task, so the terminal Canceled
		// status says why the run stopped. The fallback keeps #430's
		// guarantee for a kill landing before the dispatched agent's Run
		// registered the session — or before the stream named the task —
		// where agent.Cancel alone is a no-op: it also cancels the run's
		// detached root.
		c.killDispatch(run.reg, run.entry.ID, reason, func() {
			if run.cancel != nil {
				run.cancel()
			}
			run.agent.Cancel(run.sessionID)
		})
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
// run once the list has gone untouched for the stall window ("stalled
// todos"). The dispatch session starts empty, so a missing list is not a
// stall: the watcher keeps polling until the first todo write, arms from
// there, and a run that never writes todos is never stall-killed (an
// absent list is the nudge ladder's problem, not a stall). The poll
// cadence is a quarter of the window, clamped so tiny windows still poll
// and huge ones do not hammer the DB.
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

	// An empty fingerprint cannot equal a real list: the first todo
	// write is a "change" that arms the window.
	fingerprint := ""
	stalledSince := time.Time{}
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
		Agent:         run.entry.Agent,
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
	if entry, ok := run.reg.Get(run.entry.ID); ok {
		terminal.Handle = entry.Handle
	}
	diff, diffErr := run.provider.Diff(ctx, run.entry)
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
		Agent:         run.entry.Agent,
		Branch:        run.entry.Branch,
		WorkspacePath: run.entry.Path,
		SessionID:     run.sessionID,
	}
	// The handle was assigned after the dispatchRun's entry snapshot was
	// taken, so read it back from the registry: the terminal payload is
	// the model's addressable record of the run (#313).
	if entry, ok := run.reg.Get(run.entry.ID); ok {
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
		// The work-turn findings reader (#397): on both paths the result
		// carried here is the last turn's — a steer accepted while the
		// final step was streaming runs as the follow-up turn, and its
		// reply would replace the work's report. The run's record keeps
		// the work turn's text apart; when it holds nothing (a run
		// driven without the record, or no finished turn) the reader's
		// text stands, exactly as before.
		if work := run.findings.work(); work != "" {
			terminal.KeyFindings = work
		}
		terminal.SteerReplies = run.findings.steerReplies()
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

// deliverDispatchResult records the terminal payload for delivery to
// the main agent (#66) and triggers the flush. The result joins the
// parent session's pending set (#388) rather than the prompt queue:
// a delivery that lands while the parent is busy waits there — outside
// the queue the user's Esc, Cancel, and ClearQueue tear through — and
// is delivered on the next idle, so the parent always sees the
// findings and the diff. This is the in-process Phase 1 stand-in for
// #71's A2A terminal status message; the payload shape is identical
// either way.
//
// The ctx is deliberately not carried into the delivery turn: it still
// carries the dispatch tool call's RunID, which the delivery must not
// echo (a queued delivery under that RunID suppresses or duplicates
// the tool call's terminal RunComplete in `crush run`).
func (c *coordinator) deliverDispatchResult(ctx context.Context, parentSessionID string, terminal dispatch.DispatchResult) {
	// Shutdown starts no parent delivery turn (#372): the run above
	// already recorded its terminal result and status, and a turn here
	// would run against a coordinator that is canceling everything.
	if c.shuttingDown.Load() {
		slog.Debug("Dispatch result delivery skipped: shutting down", "parent_session", parentSessionID, "dispatch_id", terminal.DispatchID)
		return
	}
	c.dispatchMu.Lock()
	if c.pendingResults == nil {
		c.pendingResults = make(map[string][]dispatch.DispatchResult)
	}
	c.pendingResults[parentSessionID] = append(c.pendingResults[parentSessionID], terminal)
	c.dispatchMu.Unlock()
	c.flushPendingResults(parentSessionID)
}

// flushPendingResults delivers every pending dispatch result for
// parentSessionID in one hidden turn (#388). It is called when a result
// lands in the pending set and every time a run on the parent session
// ends; while the parent is busy it leaves the results pending, and a
// parent session that no longer exists drops them, logged.
//
// The turn is attempted on a detached goroutine: the pending results
// are handed to it up front, and only a run that returns a non-nil
// result with no error counts as delivered — an error puts them back
// into the pending set and re-arms the flush, while a nil result with
// no error means the turn was queued behind a busy session, in which
// case the queued (system-delivery) call now owns the delivery and
// nothing is re-pended.
func (c *coordinator) flushPendingResults(parentSessionID string) {
	agent := c.currentAgent()
	if agent == nil {
		return
	}
	if c.shuttingDown.Load() {
		// Shutdown is canceling everything (#372): a delivery turn
		// must not start here either. The results stay pending, and
		// the process exit disposes of them (#355 owns persistence).
		return
	}
	if agent.IsSessionBusy(parentSessionID) {
		// Busy: the next run end on this session re-arms the flush.
		return
	}
	if _, err := c.sessions.Get(context.Background(), parentSessionID); err != nil {
		c.dispatchMu.Lock()
		pending := c.pendingResults[parentSessionID]
		delete(c.pendingResults, parentSessionID)
		c.dispatchMu.Unlock()
		for _, terminal := range pending {
			slog.Debug("Pending dispatch result dropped: parent session is gone", "parent_session", parentSessionID, "dispatch_id", terminal.DispatchID)
		}
		return
	}

	c.dispatchMu.Lock()
	pending := c.pendingResults[parentSessionID]
	delete(c.pendingResults, parentSessionID)
	c.dispatchMu.Unlock()
	if len(pending) == 0 {
		return
	}

	// One turn for every result that stacked up while the parent was
	// busy: each payload's terminal message in order.
	var prompt strings.Builder
	for _, terminal := range pending {
		prompt.WriteString(terminal.TerminalMessage())
		prompt.WriteString("\n\n")
	}

	c.startDispatchRun(func() {
		// Detached: the flush caller (a dispatch goroutine or a run-end
		// hook) must not block on the delivery turn. Riding the spawn seam
		// (#422) keeps it a plain goroutine in production while letting a
		// test's reaper join it, so no delivery turn races the test's
		// TempDir removal. WithoutCancel: the
		// dispatch goroutine's context ends when deliverDispatchResult
		// returns, and the delivered turn must outlive it. The hidden
		// marker keeps the injected prompt out of the chat UI — the
		// agent block (#65) is the visible surface for dispatch state —
		// and WithRunID("") strips the tool call's RunID so the
		// delivery's terminal event is never mistaken for the tool
		// call's. The system-delivery marker keeps the queued call (if
		// the parent turned busy in the window before Run) alive across
		// queue clears.
		runCtx := message.WithHiddenUserMessage(
			WithRunID(WithSystemDelivery(context.WithoutCancel(context.Background())), ""),
		)
		result, err := c.run(runCtx, nil, parentSessionID, prompt.String())
		switch {
		case err == nil && result != nil:
			// Delivered: stamp the durable records (#355) so a restart
			// never re-delivers a payload the parent has already seen.
			if c.dispatchRecords != nil {
				for _, terminal := range pending {
					if err := c.dispatchRecords.RecordDelivered(terminal.DispatchID); err != nil {
						slog.Warn("Failed to stamp dispatch result delivered", "dispatch_id", terminal.DispatchID, "error", err)
					}
				}
			}
		case err == nil && result == nil:
			// The turn was queued behind a busy session; the queued
			// system-delivery call owns the delivery now.
		default:
			slog.Error("Dispatch result delivery failed; pended for retry", "parent_session", parentSessionID, "error", err)
			c.dispatchMu.Lock()
			if c.pendingResults == nil {
				c.pendingResults = make(map[string][]dispatch.DispatchResult)
			}
			c.pendingResults[parentSessionID] = append(c.pendingResults[parentSessionID], pending...)
			c.dispatchMu.Unlock()
			// The run-end hook that fires inside c.run raced this
			// re-pend; arm the flush again so the retry is not lost
			// until the next unrelated run end. A busy parent no-ops.
			// The delay keeps a deterministic failure (a provider that
			// is not configured, a model that will not resolve) from
			// spinning the retry chain hot: every failed attempt
			// re-pends and re-arms, so an instantly-failing run with no
			// pause loops at full tilt, an Error line per turn.
			time.AfterFunc(dispatchRetryBackoff, func() {
				c.flushPendingResults(parentSessionID)
			})
		}
	})
}

// dispatchRegistry returns the coordinator's dispatch registry,
// creating it on first use. Unlike the workspace provider it never
// fails: the registry is pure in-memory state, so callers can resolve
// handles and sessions whether or not any git-backed dispatch ever
// ran. NewCoordinator creates it eagerly; the nil branch covers tests
// that construct the coordinator struct directly.
func (c *coordinator) dispatchRegistry() *dispatch.AgentRegistry {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if c.dispatchReg == nil {
		c.dispatchReg = dispatch.NewAgentRegistry()
	}
	return c.dispatchReg
}

// dispatchWorkspaceProvider returns the coordinator's git worktree
// provider, creating it on first use. Creation can fail — the working
// directory may not be a git repository — and the failure is cached so
// every later dispatch reports it instead of retrying, while
// coordinator construction stays git-agnostic. The first successful
// creation also starts the todo collector (#65): one subscription to
// the session event stream and the registry's own transitions, reduced
// once and sunk to every configured sink (the agent block now, #174's
// A2A bridge later).
func (c *coordinator) dispatchWorkspaceProvider() (*dispatch.GitWorktreeProvider, error) {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	if c.dispatchProvider == nil && c.dispatchProviderErr == nil {
		// Worktrees live under the data directory, never beside the
		// working directory: a launch from a repo subdirectory must not
		// create a new <cwd>/.crush that git picks up and that later
		// launches treat as their data directory (#383).
		worktreesDir, err := dispatch.WorktreesDir(c.cfg.Config().Options.DataDirectory, c.cfg.WorkingDir())
		if err != nil {
			c.dispatchProviderErr = err
		} else {
			// The registry is created inline, not via dispatchRegistry():
			// that accessor takes dispatchMu, and this method already
			// holds it. NewCoordinator creates the registry eagerly; the
			// nil branch covers tests that build the struct directly.
			if c.dispatchReg == nil {
				c.dispatchReg = dispatch.NewAgentRegistry()
			}
			c.dispatchProvider, c.dispatchProviderErr = dispatch.NewGitWorktreeProvider(c.cfg.WorkingDir(), worktreesDir, c.dispatchReg)
		}
		if c.dispatchProvider != nil {
			c.dispatchCollector = dispatch.NewTodoCollector(c.dispatchReg, c.sessions, c.dispatchSinks...)
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
	return c.dispatchProvider, c.dispatchProviderErr
}

// DispatchStatus returns the current progress snapshot for the
// dispatched agent running on sessionID (#65). It backs the UI's
// seed-on-load path: a reloaded session's persisted dispatch_agent
// result is the running handle, and this is how the block learns the
// dispatch's real state without waiting for the next event.
//
// When the collector has no snapshot — a restart emptied the in-memory
// registry — the durable record (#355) answers instead, so a block
// whose run reached a terminal state renders that state instead of a
// forever-working stale handle.
func (c *coordinator) DispatchStatus(sessionID string) (dispatch.TodoSnapshot, bool) {
	c.dispatchMu.Lock()
	collector := c.dispatchCollector
	records := c.dispatchRecords
	c.dispatchMu.Unlock()
	if collector != nil {
		if snapshot, ok := collector.Snapshot(sessionID); ok {
			return snapshot, true
		}
	}
	if records == nil {
		return dispatch.TodoSnapshot{}, false
	}
	result, status, ok := records.SnapshotRecord(sessionID)
	if !ok {
		return dispatch.TodoSnapshot{}, false
	}
	entry := dispatch.Entry{
		ID:        result.DispatchID,
		Path:      result.WorkspacePath,
		Branch:    result.Branch,
		SessionID: sessionID,
		Handle:    result.Handle,
		Status:    status,
	}
	if status.IsTerminal() {
		result.Status = status
		entry.Result = &result
	}
	return dispatch.TodoSnapshot{Entry: entry}, true
}

// ReconcileDispatchDeliveries re-delivers the terminal dispatch
// payloads a previous process finished but never delivered (#355).
// Every terminal record without a delivered stamp whose parent session
// still exists goes through deliverDispatchResult — #388's
// pending-delivery machinery, so the delivered stamp lands only when
// the delivery turn succeeds. A parent session that no longer exists
// drops the payload permanently: nobody can ever receive it, and the
// row's terminal state still seeds the UI. Called from the app at
// startup, after a2a.ReconcileOrphanedTasks and before the UI loads
// sessions.
func (c *coordinator) ReconcileDispatchDeliveries(ctx context.Context) {
	c.dispatchMu.Lock()
	records := c.dispatchRecords
	c.dispatchMu.Unlock()
	if records == nil {
		return
	}
	undelivered, err := records.UndeliveredTerminal()
	if err != nil {
		slog.Warn("Dispatch delivery reconcile could not list undelivered results", "error", err)
		return
	}
	for _, u := range undelivered {
		if _, err := c.sessions.Get(ctx, u.ParentSessionID); err != nil {
			slog.Debug("Undelivered dispatch result dropped: parent session is gone", "parent_session", u.ParentSessionID, "dispatch_id", u.Result.DispatchID)
			if err := records.RecordDelivered(u.Result.DispatchID); err != nil {
				slog.Warn("Failed to close undelivered dispatch record", "dispatch_id", u.Result.DispatchID, "error", err)
			}
			continue
		}
		slog.Info("Re-delivering dispatch result left undelivered by a restart", "parent_session", u.ParentSessionID, "dispatch_id", u.Result.DispatchID)
		c.deliverDispatchResult(ctx, u.ParentSessionID, u.Result)
	}
}

// ReleaseDispatches is the synchronous session-end cleanup (#367): it
// releases this session's dispatch workspaces that are safe to remove —
// the ones that never produced work — and leaves completed or killed
// work with commits or uncommitted changes on disk for salvage. It is
// called from App.Shutdown on the live coordinator context, bounded by
// dispatchSweepTimeout so a wedged worktree cannot hang shutdown.
func (c *coordinator) ReleaseDispatches(ctx context.Context) {
	c.dispatchMu.Lock()
	provider := c.dispatchProvider
	c.dispatchMu.Unlock()
	if provider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, dispatchSweepTimeout)
	defer cancel()
	if err := provider.ReleaseUnworked(ctx); err != nil {
		slog.Error("Dispatch workspace release failed", "error", err)
	}
}

// sweepDispatchOnDone is the session-end backstop for the synchronous
// ReleaseDispatches App.Shutdown runs: when the coordinator's context
// ends without it — a crash path — the same selective release still
// runs. A workspace with salvageable work stays on disk; only workless
// ones are removed. In-flight runs fail against a removed workspace;
// wander kill in #316 owns deterministic cancellation of live runs.
func (c *coordinator) sweepDispatchOnDone(ctx context.Context) {
	<-ctx.Done()
	c.sweepDispatch()
}

// sweepDispatch runs the session-end release on a fresh, bounded
// context: the coordinator's own is already done.
func (c *coordinator) sweepDispatch() {
	c.dispatchMu.Lock()
	provider := c.dispatchProvider
	c.dispatchMu.Unlock()
	if provider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchSweepTimeout)
	defer cancel()
	if err := provider.ReleaseUnworked(ctx); err != nil {
		slog.Error("Dispatch workspace release failed", "error", err)
	}
}

// removeDispatch tears down a dispatch that failed before its background
// run started: the registry entry, the workspace, and the toolchain. Best
// effort — the tool error being returned to the model matters more.
func (c *coordinator) removeDispatch(ctx context.Context, reg *dispatch.AgentRegistry, provider *dispatch.GitWorktreeProvider, entry dispatch.Entry, toolchain *DispatchToolchain) {
	if toolchain != nil {
		toolchain.Close(ctx)
	}
	reg.Remove(entry.ID)
	if err := provider.Release(ctx, entry); err != nil {
		slog.Warn("Failed to remove failed dispatch workspace", "dispatch_id", entry.ID, "error", err)
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
