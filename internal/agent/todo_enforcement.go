package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// The todo enforcement ladder (#315). Agents forgetting their todos is an
// enforcement problem: every downstream surface (the dispatch block's
// current-todo line, the @ type-ahead status, the A2A event bridge) reads
// the session's todo list, and a run that never creates one starves them.
// The ladder runs coordinator-side, inside the session agent's run loop:
//
//  1. The dispatch prompt template mandates a todos call before the first
//     mutating tool (cheap, ignorable; see templates/dispatch.md.tpl).
//  2. Nudge injection (default on): after N tool calls, or the first
//     mutating tool call, with no todos activity, a context message is
//     injected into the run forcing a todos update. Repeat offenders get
//     an escalating nudge within the same run.
//  3. The opt-in hard gate: a pre-tool wrapper rejecting mutating tools
//     until a todo list exists. Deterministic but brittle, so it is
//     config-gated and off by default.
//
// It applies to dispatched sub-agents and the main agent alike, because
// every agent runs through sessionAgent.Run. The same shape as
// loop_detection.go: watch the run, act when it misbehaves.

// maxNudgesPerRun caps the ladder at a nudge and an escalating nudge per
// run; the follow-up (wander kill, #316) escalates past this.
const maxNudgesPerRun = 2

// todoNudgeMessage is the first-rung nudge, injected as the run's next
// input once the threshold trips with no todos activity.
const todoNudgeMessage = "You have made tool calls without recording a todo list. Call the todos tool now to write down your plan and keep it current as you work; your progress surfaces depend on it."

// todoEscalatingNudgeMessage is the second-rung nudge for repeat
// offenders within a run: stronger wording, and the warning that
// continued ignoring escalates (wander kill, #316).
const todoEscalatingNudgeMessage = "You are still working without a todo list after being reminded. Call the todos tool immediately, before any further tool calls. Ignoring this again will escalate."

// mutatingToolNames are the tools the ladder treats as mutating: they
// change state, so a run using them without a plan is exactly the run the
// ladder exists for. Mirrors the design doc's write/edit/bash set plus
// multiedit, which is edit's multi-application form.
var mutatingToolNames = []string{
	tools.WriteToolName,
	tools.EditToolName,
	tools.MultiEditToolName,
	tools.BashToolName,
}

// isMutatingTool reports whether name is one of the ladder's mutating
// tools.
func isMutatingTool(name string) bool {
	return slices.Contains(mutatingToolNames, name)
}

// hasTodosTool reports whether ts carries the todos tool. Every rung of
// the ladder asks the model to call it, so an agent without it (the tool
// in options.disabled_tools, or an agent whose allowed tools leave it
// out) gets no ladder at all: the nudges would demand a call the model
// cannot make, and the gate would refuse every mutating tool for good.
func hasTodosTool(ts []fantasy.AgentTool) bool {
	return slices.ContainsFunc(ts, func(t fantasy.AgentTool) bool {
		return t.Info().Name == tools.TodosToolName
	})
}

// todoEnforcement carries the resolved ladder settings for one session
// agent. It is constructed by the coordinator's builders (buildAgent per
// agent type, buildDispatchedAgent from the global options) and shared by
// every run that agent serves; per-run state lives in todoEnforcementRun.
type todoEnforcement struct {
	settings config.TodoEnforcementSettings
	sessions session.Service
}

// newTodoEnforcement returns the enforcement for the resolved settings,
// or nil when the whole ladder is off (no nudging and no hard gate, or no
// session service to read todos from) -- a nil enforcer is the no-op fast
// path.
func newTodoEnforcement(settings config.TodoEnforcementSettings, sessions session.Service) *todoEnforcement {
	if (!settings.Enabled && !settings.HardGate) || sessions == nil {
		return nil
	}
	return &todoEnforcement{settings: settings, sessions: sessions}
}

// todoEnforcementRun is the ladder's state for one Run: tool calls since
// the last nudge, whether todos activity has been seen, and how many
// nudges the run has injected. It is written only from the run's
// streaming callbacks (OnToolCall, PrepareStep), which the fantasy loop
// executes sequentially, and read nowhere else; the mutex guards the
// future where #316's kill path observes escalation from another
// goroutine.
type todoEnforcementRun struct {
	enforcement *todoEnforcement
	// onKill is the wander-kill escalation (#316), wired only for
	// dispatched agents: invoked once, when the run ignores its nudges
	// past the kill threshold. Nil elsewhere, so the ladder tops out at
	// nudging for the main agent and agent-tool sub-agents.
	onKill func(reason string)

	mu sync.Mutex
	// todosSatisfied records that the run's todos are alive: the session
	// carried a todo list at run start, or the todos tool ran during it.
	todosSatisfied bool
	// toolCalls counts tool calls since the last nudge (or run start).
	toolCalls int
	// mutatingCall records that a mutating tool was called since the
	// last nudge (or run start).
	mutatingCall bool
	// nudges counts nudges injected this run.
	nudges int
	// killed records that the kill already fired, so later windows do
	// not re-invoke the hook.
	killed bool
}

// newRun starts the ladder for one run, seeded with whether the session
// already carries a todo list. A nil enforcement yields a nil run, the
// no-op fast path every method also guards for.
func (e *todoEnforcement) newRun(sessionHasTodos bool, onKill func(reason string)) *todoEnforcementRun {
	if e == nil {
		return nil
	}
	return &todoEnforcementRun{
		enforcement:    e,
		onKill:         onKill,
		todosSatisfied: sessionHasTodos,
	}
}

// recordToolCall observes one tool call from the model. A todos tool call
// is todos activity; anything else advances the since-nudge counters.
func (r *todoEnforcementRun) recordToolCall(toolName string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if toolName == tools.TodosToolName {
		r.todosSatisfied = true
		return
	}
	r.toolCalls++
	if isMutatingTool(toolName) {
		r.mutatingCall = true
	}
}

// pendingNudge decides whether the run's next model call should carry a
// nudge, and returns its text when so. Trips when todos activity is
// absent and either the tool-call threshold is past or a mutating tool
// was called; escalating on the second nudge, capped at maxNudgesPerRun.
// Counters reset on injection so escalation needs another full window of
// ignoring, not the next tool call.
//
// This is also the kill rung (#316): when the configured number of
// nudges has been given and the window after the last one completes with
// still no todos activity, the wander-kill hook fires (ignored nudges)
// and the run is canceled. With no hook wired the ladder stays capped at
// nudging, which is the main agent's and sub-agents' contract.
func (r *todoEnforcementRun) pendingNudge() string {
	if r == nil || !r.enforcement.settings.Enabled {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.todosSatisfied || r.killed {
		return ""
	}
	if r.toolCalls < r.enforcement.settings.NudgeThreshold && !r.mutatingCall {
		return ""
	}
	r.toolCalls = 0
	r.mutatingCall = false
	// The kill rung (#316) comes first: if the ladder has already
	// delivered killAfterNudges nudges, this window is the ignored-nudges
	// kill, not another nudge. Clamp the threshold to the ladder's cap,
	// since no further nudges are coming past it.
	if r.onKill != nil && r.enforcement.settings.KillAfterNudges > 0 &&
		r.nudges >= min(r.enforcement.settings.KillAfterNudges, maxNudgesPerRun) {
		r.killed = true
		r.onKill(dispatch.ReasonIgnoredNudges)
		return ""
	}
	if r.nudges < maxNudgesPerRun {
		r.nudges++
		if r.nudges == 1 {
			return todoNudgeMessage
		}
		return todoEscalatingNudgeMessage
	}
	return ""
}

// gate check: the hard gate rejects mutating tool calls until the session
// has a todo list. Called from the wrapped tool, so it reads the session
// live: once the agent records todos mid-run the gate opens.
func (e *todoEnforcement) gate(ctx context.Context, sessionID string) error {
	if e == nil || !e.settings.HardGate {
		return nil
	}
	if sessionID == "" {
		return nil
	}
	sess, err := e.sessions.Get(ctx, sessionID)
	if err != nil {
		// The gate must not turn a lookup failure into a tool failure;
		// let the tool run and report its own errors.
		return nil
	}
	if len(sess.Todos) > 0 {
		return nil
	}
	return fmt.Errorf("no todo list exists for this session; call the todos tool to record your plan before mutating anything (todo enforcement hard gate)")
}

// wrapTodoGate wraps the mutating tools in ts with the hard-gate wrapper
// when the gate is on. Other tools pass through untouched, and a nil
// enforcement, a disabled gate or a tool set without the todos tool
// returns the slice unchanged.
func wrapTodoGate(ts []fantasy.AgentTool, e *todoEnforcement) []fantasy.AgentTool {
	if e == nil || !e.settings.HardGate || !hasTodosTool(ts) {
		return ts
	}
	out := make([]fantasy.AgentTool, len(ts))
	for i, tool := range ts {
		if isMutatingTool(tool.Info().Name) {
			out[i] = &todoGateTool{inner: tool, enforcement: e}
			continue
		}
		out[i] = tool
	}
	return out
}

// todoGateTool is the ladder's third rung: a pre-tool wrapper rejecting
// the mutating tool until the session has a todo list. Same shape as
// hookedTool, but in-process rather than a shell hook.
type todoGateTool struct {
	inner       fantasy.AgentTool
	enforcement *todoEnforcement
}

func (g *todoGateTool) Info() fantasy.ToolInfo { return g.inner.Info() }

func (g *todoGateTool) ProviderOptions() fantasy.ProviderOptions {
	return g.inner.ProviderOptions()
}

func (g *todoGateTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	g.inner.SetProviderOptions(opts)
}

func (g *todoGateTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if err := g.enforcement.gate(ctx, tools.GetSessionFromContext(ctx)); err != nil {
		resp := fantasy.NewTextErrorResponse(err.Error())
		resp.Metadata = `{"todo_gate":true}`
		return resp, nil
	}
	return g.inner.Run(ctx, call)
}

// createTodoNudgeMessage persists a nudge as a user message on the
// session, so the nudge is visible in the transcript and survives into
// later turns' history. It reads as a message from the harness, not the
// human, so the text says who is talking.
func (a *sessionAgent) createTodoNudgeMessage(ctx context.Context, sessionID, nudge string) (message.Message, error) {
	return a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: nudge}},
	})
}

// injectTodoNudge appends the pending nudge, if any, to the step's input
// messages and persists it on the session. Called from PrepareStep, after
// queued follow-ups are folded in, so the nudge is the most recent thing
// the model reads. Persisting is best-effort: a failure is logged and the
// nudge skipped; the ladder re-decides on the next step, because a
// nudged model beats a broken run.
func (a *sessionAgent) injectTodoNudge(ctx context.Context, sessionID string, run *todoEnforcementRun, msgs []fantasy.Message) []fantasy.Message {
	nudge := run.pendingNudge()
	if nudge == "" {
		return msgs
	}
	userMessage, err := a.createTodoNudgeMessage(ctx, sessionID, nudge)
	if err != nil {
		slog.Warn("Failed to persist todo nudge", "session_id", sessionID, "error", err)
		return msgs
	}
	return append(msgs, userMessage.ToAIMessage()...)
}
