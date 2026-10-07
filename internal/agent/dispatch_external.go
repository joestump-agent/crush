package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/google/uuid"
)

// External agents (#434): a runtime a2a agent definition replaces the
// in-process dispatched agent with an A2A Agent Card hosted elsewhere.
// The coordinator resolves the definition's bearer token, hands it to
// the A2A host with the card URL, and drives the returned ExternalAgent
// through the same client path as a served dispatch. The host owns the
// card checks, the origin pin, and the credential; the registry never
// sees the token.

// redactedSecret is what a Secret renders as, wherever it is printed.
const redactedSecret = "[REDACTED]"

// Secret is a credential value that never prints itself (#434): fmt,
// slog and encoding/json all render it as [REDACTED], so a stray log
// line or a %v of a struct carrying it cannot leak the value. Reveal
// returns it for the one place that must send it.
type Secret struct {
	value string
}

// NewSecret wraps a resolved credential.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the credential itself. Only the code that puts it on
// the wire, or scrubs it from text, calls it.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret holds no credential.
func (s Secret) IsZero() bool { return s.value == "" }

// String implements fmt.Stringer with the redacted form.
func (Secret) String() string { return redactedSecret }

// GoString implements fmt.GoStringer with the redacted form.
func (Secret) GoString() string { return redactedSecret }

// LogValue implements slog.LogValuer with the redacted form.
func (Secret) LogValue() slog.Value { return slog.StringValue(redactedSecret) }

// MarshalJSON renders the redacted form, never the credential.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redactedSecret) }

// ExternalAgentParams names one external agent to resolve (#434).
type ExternalAgentParams struct {
	// CardURL is the definition's Agent Card URL.
	CardURL string
	// Token is the resolved bearer credential; zero when the definition
	// sets no auth. A non-zero token requires the card to declare an
	// HTTP bearer security scheme, and rides only requests to the card's
	// own origin.
	Token Secret
}

// ExternalAgentResolver is the external-card half of the A2A host
// (#434): resolve a runtime a2a definition's card at dispatch time and
// return the pinned client that drives it. Implemented by the
// production a2a.ServerFactory; coordinators assert to it the way they
// do to [DispatchCanceler], and refuse external dispatches when the
// wired host does not implement it.
type ExternalAgentResolver interface {
	// ResolveExternalAgent fetches the card, checks it — https, a
	// supported transport on the card's own origin, a bearer scheme when
	// a token is set — and returns the agent. Every refusal is an error
	// that names the card and never the token.
	ResolveExternalAgent(ctx context.Context, params ExternalAgentParams) (ExternalAgent, error)
}

// ExternalAgent is one resolved external Agent Card and the client that
// talks to it (#434). The client is pinned to the card's origin and
// carries the credential the resolver was given; nothing outside the
// implementation can read the credential back.
type ExternalAgent interface {
	// Source is the card URL the agent was resolved from: the
	// DispatchResult's source.
	Source() string
	// Endpoint is the JSON-RPC service URL the card named, on the
	// card's own origin.
	Endpoint() string
	// Stream sends the prompt as a new task and consumes it to its end:
	// a terminal state, a refused pause, a kill, or a silent stream.
	// The outcome's text is the remote agent's untrusted output,
	// sanitized and capped; it never carries diffs, usage, or todos.
	Stream(ctx context.Context, params ExternalDispatchParams) (DispatchTransportOutcome, error)
	// Close releases the client's idle connections once the dispatch
	// has ended.
	Close()
}

// ExternalDispatchParams is one external dispatch's slice of the A2A
// client (#434).
type ExternalDispatchParams struct {
	// Prompt is the task text sent to the external agent.
	Prompt string
	// OnTask, when set, receives the remote task's ID once the stream's
	// first event names it, exactly like a served dispatch (#349).
	OnTask func(taskID string)
	// IdleTimeout ends a stream that delivers no event for this long:
	// the run is killed with dispatch.ReasonIdleTimeout and its task is
	// canceled. 0 disables it.
	IdleTimeout time.Duration
	// Kill is the run's kill switch. The transport watches it: a kill
	// ends the stream and cancels the remote task with the kill's
	// reason, and the idle timeout trips it. Nil means the run cannot
	// be killed from outside.
	Kill RunKill
}

// RunKill is a dispatched run's kill switch as a transport sees it
// (#434): the first reason wins, and Killed closes with it.
type RunKill interface {
	// Kill records reason as the run's kill reason unless one is
	// already recorded.
	Kill(reason string)
	// Killed returns a channel closed by the first kill.
	Killed() <-chan struct{}
	// Reason returns the recorded kill reason, empty until a kill.
	Reason() string
}

// ErrSteerExternal refuses a steer addressed to an external agent
// (#434): see DeliverAgentMessage for why.
var ErrSteerExternal = errors.New("steering external agents is not supported yet")

// dispatchExternal starts one dispatch on a runtime a2a agent (#434).
// It shares the built-in dispatch's front half — the tool's agent
// resolution, the concurrency cap, the registry entry with its handle
// and role, the task session, the durable record — and skips the rest:
// the none workspace provider stands in for the worktree, and there is
// no toolchain, no local agent, and no A2A server. The card is resolved
// here, while the tool call is open, so an unreachable card, a refused
// one, or a token that does not resolve is the tool's error.
func (c *coordinator) dispatchExternal(ctx context.Context, params DispatchAgentParams, call fantasy.ToolCall, agentCfg config.Agent) (fantasy.ToolResponse, error) {
	// Local knobs mean nothing to a remote agent: refuse them rather
	// than silently dropping them, like a pinned model (#433).
	var local []string
	if params.Model != "" {
		local = append(local, "model")
	}
	if len(params.Skills) > 0 {
		local = append(local, "skills")
	}
	if params.Branch != "" {
		local = append(local, "branch")
	}
	if len(local) > 0 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"agent %q is an external A2A agent; omit %s, which only apply to agents that run locally",
			agentCfg.ID, strings.Join(local, ", "))), nil
	}

	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, errors.New("session id missing from context")
	}
	agentMessageID := tools.GetMessageFromContext(ctx)
	if agentMessageID == "" {
		return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
	}

	resolver, ok := c.a2aHost().(ExternalAgentResolver)
	if !ok || resolver == nil {
		return fantasy.NewTextErrorResponse("dispatch unavailable: the A2A host cannot reach external agents"), nil
	}
	token, err := c.resolveExternalToken(agentCfg)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	reserved, running := c.reserveDispatchSlot()
	if !reserved {
		limit := c.cfg.Config().Options.GetDispatchMaxConcurrent()
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"dispatch at capacity: %d agents are already running (dispatch.max_concurrent=%d); wait for one to finish or cancel one",
			running, limit)), nil
	}

	ext, err := resolver.ResolveExternalAgent(ctx, ExternalAgentParams{CardURL: agentCfg.Card, Token: token})
	if err != nil {
		c.releaseDispatchSlot()
		slog.Warn("External agent card refused", "agent", agentCfg.ID, "card", agentCfg.Card, "error", err)
		return fantasy.NewTextErrorResponse(fmt.Sprintf("dispatch to external agent %q failed: %s", agentCfg.ID, err)), nil
	}

	// The collector renders the agent block (#65); an external dispatch
	// never creates the git provider that would otherwise start it.
	c.startDispatchCollector()
	reg := c.dispatchRegistry()

	// The none workspace (#391): no directory, branch, or base, so the
	// entry carries none and nothing is ever created on disk.
	var provider dispatch.WorkspaceProvider = dispatch.NoneProvider{}
	id := uuid.NewString()
	placement, err := provider.Provision(ctx, id, dispatch.ProvisionOptions{})
	if err != nil {
		c.releaseDispatchSlot()
		ext.Close()
		return fantasy.NewTextErrorResponse(fmt.Sprintf("provision dispatch workspace: %s", err)), nil
	}
	entry := dispatch.Entry{
		ID:      id,
		Path:    placement.Path,
		Branch:  placement.Branch,
		Base:    placement.Base,
		BaseSHA: placement.BaseSHA,
		Agent:   agentCfg.ID,
		Source:  ext.Source(),
		Status:  dispatch.StatusProvisioned,
	}
	reg.Register(entry)

	// Detached from the tool call, like a built-in dispatch's root
	// (#371): the run outlives the turn that started it.
	rootCtx, rootCancel := context.WithCancel(context.WithoutCancel(ctx))
	fail := func(msg string) (fantasy.ToolResponse, error) {
		rootCancel()
		c.releaseDispatchSlot()
		ext.Close()
		reg.Remove(entry.ID)
		return fantasy.NewTextErrorResponse(msg), nil
	}

	taskSessionID := c.sessions.CreateAgentToolSessionID(agentMessageID, call.ID)
	taskSession, err := c.sessions.CreateTaskSession(ctx, taskSessionID, sessionID, "External Agent")
	if err != nil {
		return fail(fmt.Sprintf("create session: %s", err))
	}
	reg.SetSession(entry.ID, taskSession.ID)
	reg.SetParentSessionID(entry.ID, sessionID)
	reg.SetStatus(entry.ID, dispatch.StatusRunning)
	assignedHandle, ok := reg.AssignHandle(entry.ID, params.Handle, params.Role)
	if !ok {
		return fail("assign dispatch handle: registry entry vanished")
	}

	kill := &dispatchKill{}
	run := dispatchRun{
		reg:             reg,
		entry:           entry,
		prompt:          params.Prompt,
		sessionID:       taskSession.ID,
		parentSessionID: sessionID,
		contentWidth:    tools.GetContentWidthFromContext(ctx),
		kill:            kill,
		killSettings:    c.externalKillSettings(agentCfg),
		cancel:          rootCancel,
		findings:        &dispatchFindings{},
		holdsSlot:       true,
		external:        ext,
	}
	c.registerLiveDispatch(entry.ID, &liveDispatch{
		cancel:    rootCancel,
		sessionID: taskSession.ID,
		kill:      kill,
		done:      make(chan struct{}),
	})

	handle := dispatch.DispatchResult{
		DispatchID: entry.ID,
		Handle:     assignedHandle,
		Agent:      entry.Agent,
		SessionID:  taskSession.ID,
		Source:     ext.Source(),
		Status:     dispatch.StatusRunning,
	}
	if c.dispatchRecords != nil {
		if err := c.dispatchRecords.RecordStarted(handle, sessionID); err != nil {
			slog.Warn("Failed to record dispatch start", "dispatch_id", entry.ID, "error", err)
		}
	}
	slog.Debug("External dispatch started", "dispatch_id", entry.ID, "agent", agentCfg.ID, "source", ext.Source())
	c.startDispatchRun(func() { c.runDispatch(rootCtx, run) })
	return fantasy.NewTextResponse(handle.Render()), nil
}

// resolveExternalToken resolves a runtime a2a definition's auth.token at
// dispatch time (#434), through the config's variable resolver: the
// same $VAR and $(cmd) expansion every other config credential gets,
// rerun per dispatch so a rotated token is picked up. The value lives
// only in the returned Secret; errors name the agent and, from the
// resolver, the reference as written, never a resolved value.
func (c *coordinator) resolveExternalToken(agentCfg config.Agent) (Secret, error) {
	if agentCfg.Auth == nil || agentCfg.Auth.Token == nil {
		return Secret{}, nil
	}
	resolver := c.cfg.Resolver()
	if resolver == nil {
		return Secret{}, fmt.Errorf("agent %q: no variable resolver to resolve auth.token", agentCfg.ID)
	}
	value, err := resolver.ResolveValue(*agentCfg.Auth.Token)
	if err != nil {
		return Secret{}, fmt.Errorf("agent %q: resolve auth.token: %w", agentCfg.ID, err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return Secret{}, fmt.Errorf("agent %q: auth.token resolved to an empty value; set the variable or command it names", agentCfg.ID)
	}
	return NewSecret(value), nil
}

// externalKillSettings resolves the thresholds an external dispatch runs
// with (#434). Two apply: the hard timeout — the definition's
// kill.timeout over options.todo_enforcement.hard_timeout — and the idle
// timeout, carried as InactivityTimeout: transport.idle_timeout over
// options.todo_enforcement.inactivity_timeout. The nudge ladder and the
// todos stall window watch a local session the remote never writes to,
// so they stay off.
func (c *coordinator) externalKillSettings(agentCfg config.Agent) config.TodoEnforcementSettings {
	resolved := c.dispatchEnforcement(agentCfg)
	settings := config.TodoEnforcementSettings{
		HardTimeout:       resolved.HardTimeout,
		InactivityTimeout: resolved.InactivityTimeout,
	}
	if agentCfg.Transport != nil && agentCfg.Transport.IdleTimeout != nil {
		settings.InactivityTimeout = max(time.Duration(*agentCfg.Transport.IdleTimeout), 0)
	}
	return settings
}

// runExternalDispatch drives one external dispatch (#434): the prompt
// goes to the remote as a new task, the stream is folded by the same
// client machinery a served dispatch uses — the task ID stamped on the
// registry and the durable record, resubscribe on a dropped stream —
// and the outcome assembles into the terminal result. The run's kill
// switch rides along: the hard timeout, a user cancel, and shutdown all
// end the stream through it, and the idle timeout trips it.
func (c *coordinator) runExternalDispatch(ctx context.Context, run dispatchRun) dispatch.DispatchResult {
	outcome, err := run.external.Stream(ctx, ExternalDispatchParams{
		Prompt: run.prompt,
		OnTask: func(taskID string) {
			c.recordDispatchTask(run, taskID)
			slog.Debug("External dispatch task started", "dispatch_id", run.entry.ID, "source", run.external.Source(), "task_id", taskID)
		},
		IdleTimeout: run.killSettings.InactivityTimeout,
		Kill:        run.kill,
	})
	if err != nil {
		slog.Warn("External dispatch stream failed", "dispatch_id", run.entry.ID, "source", run.external.Source(), "error", err)
		outcome = DispatchTransportOutcome{Status: transportStatusFailed, Text: err.Error()}
	}
	natural := dispatchNaturalOutcomeFromTransport(outcome)
	// Kill reasons come from this process's own witness, the run's kill
	// state. A remote's Canceled text is its own account, not a kill.
	natural.killReason = ""
	return assembleExternalDispatchResult(run, natural)
}

// assembleExternalDispatchResult maps an external run's outcome onto its
// terminal DispatchResult (#434). It carries the card URL as the source
// and nothing workspace-shaped: no branch, path, or diff. A kill that
// ended the run wins; a run that finished before the kill landed keeps
// its natural outcome, as for a built-in dispatch.
func assembleExternalDispatchResult(run dispatchRun, natural dispatchNaturalOutcome) dispatch.DispatchResult {
	terminal := dispatch.DispatchResult{
		DispatchID: run.entry.ID,
		Agent:      run.entry.Agent,
		SessionID:  run.sessionID,
		Source:     run.external.Source(),
	}
	if entry, ok := run.reg.Get(run.entry.ID); ok {
		terminal.Handle = entry.Handle
	}
	if reason := run.kill.current(); reason != "" && natural.runErr != nil {
		terminal.Status = dispatch.StatusKilled
		terminal.KilledReason = reason
		return terminal
	}
	switch {
	case natural.runErr != nil:
		terminal.Status = dispatch.StatusFailed
		terminal.Error = natural.runErr.Error()
	case !natural.completed:
		terminal.Status = dispatch.StatusFailed
		terminal.Error = "the external agent never started a task"
	default:
		terminal.Status = dispatch.StatusCompleted
		terminal.KeyFindings = natural.findings
	}
	return terminal
}
