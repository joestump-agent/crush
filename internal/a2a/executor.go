package a2a

import (
	"context"
	"iter"
	"os/exec"
	"strings"

	"charm.land/fantasy"
	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/charmbracelet/crush/internal/agent"
)

// Runner is the slice of [agent.SessionAgent] the [Executor] drives. The full
// SessionAgent interface satisfies it; the narrow interface documents exactly
// what A2A execution depends on and keeps the executor unit-testable with a
// fake instead of the whole agent surface.
type Runner interface {
	Run(context.Context, agent.SessionAgentCall) (*fantasy.AgentResult, error)
	Cancel(sessionID string)
}

// Compile-time proof that a real SessionAgent can be used as a Runner.
var _ Runner = agent.SessionAgent(nil)

// DiffFunc returns the git diff produced by a dispatched run, emitted as the
// task's completion artifact. It is called after a successful run. An empty
// string or an error yields no artifact (the run still completes).
type DiffFunc func(ctx context.Context) (string, error)

// Executor adapts a Crush [agent.SessionAgent] to the [a2asrv.AgentExecutor]
// interface: it runs one dispatched agent turn, maps the run lifecycle onto
// A2A task states (submitted -> working -> completed/failed), and emits the git
// diff as the terminal artifact.
//
// Phase 1 emits a single Working status before the run and a terminal status
// after it; richer per-todo progress streaming into the Sidekick dashboard is
// wired in #71, where the SessionAgent's progress broker is bridged to SSE.
type Executor struct {
	runner    Runner
	sessionID string
	diff      DiffFunc
}

// Option configures an [Executor].
type Option func(*Executor)

// WithDiff sets the function used to collect the completion artifact — the git
// diff of the dispatched worktree. Without it, runs complete with their text
// output and no artifact. See [GitDiff] for the default production collector.
func WithDiff(fn DiffFunc) Option {
	return func(e *Executor) { e.diff = fn }
}

// NewExecutor builds an Executor that drives runner against sessionID — the
// (ephemeral) session backing the dispatched agent.
func NewExecutor(runner Runner, sessionID string, opts ...Option) *Executor {
	e := &Executor{runner: runner, sessionID: sessionID}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// Execute runs one dispatched agent turn. It announces the task submitted (for
// a new task), emits Working, invokes the SessionAgent, then emits the diff
// artifact (if any) and a terminal Completed status carrying the agent's text
// output. A run error maps to a Failed status with the error surfaced; per the
// AgentExecutor contract, failures after work has begun are reported as events,
// not as a returned error.
func (e *Executor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	return func(yield func(a2aspec.Event, error) bool) {
		// A message that referenced no existing task starts a new one:
		// announce it submitted before transitioning to working.
		if execCtx.StoredTask == nil {
			if !yield(a2aspec.NewSubmittedTask(execCtx, execCtx.Message), nil) {
				return
			}
		}

		if !yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateWorking, nil), nil) {
			return
		}

		result, err := e.runner.Run(ctx, agent.SessionAgentCall{
			SessionID: e.sessionID,
			Prompt:    messageText(execCtx.Message),
		})
		if err != nil {
			yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateFailed, errorMessage(err)), nil)
			return
		}

		if e.diff != nil {
			if diff, derr := e.diff(ctx); derr == nil && diff != "" {
				if !yield(a2aspec.NewArtifactEvent(execCtx, a2aspec.NewTextPart(diff)), nil) {
					return
				}
			}
		}

		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCompleted, resultMessage(result)), nil)
	}
}

// Cancel stops the in-flight dispatched run for this executor's session and
// reports the task canceled.
func (e *Executor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2aspec.Event, error] {
	return func(yield func(a2aspec.Event, error) bool) {
		e.runner.Cancel(e.sessionID)
		yield(a2aspec.NewStatusUpdateEvent(execCtx, a2aspec.TaskStateCanceled, nil), nil)
	}
}

// GitDiff returns a [DiffFunc] that captures the uncommitted diff of the git
// worktree rooted at dir (`git -C dir diff`), the dispatched agent's work
// product. A non-zero git exit or missing repo surfaces as an error, which the
// executor treats as "no artifact" rather than a run failure.
func GitDiff(dir string) DiffFunc {
	return func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff").Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

// messageText concatenates the text parts of an incoming A2A message into a
// single prompt. Non-text parts are ignored in Phase 1.
func messageText(msg *a2aspec.Message) string {
	if msg == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		if t := part.Text(); t != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
	}
	return b.String()
}

// resultMessage wraps a run's text output as an agent-role A2A message for the
// terminal status update. A nil result yields an empty message.
func resultMessage(result *fantasy.AgentResult) *a2aspec.Message {
	var text string
	if result != nil {
		text = result.Response.Content.Text()
	}
	return a2aspec.NewMessage(a2aspec.MessageRoleAgent, a2aspec.NewTextPart(text))
}

// errorMessage wraps a run error as an agent-role A2A message for the failed
// status update.
func errorMessage(err error) *a2aspec.Message {
	return a2aspec.NewMessage(a2aspec.MessageRoleAgent, a2aspec.NewTextPart(err.Error()))
}
