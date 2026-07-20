package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
)

//go:embed templates/dispatch_agent.md
var dispatchAgentDescription string

// DispatchAgentToolName is the model-facing name of the dispatch tool.
const DispatchAgentToolName = "dispatch_agent"

var errDispatchUnavailable = errors.New("agent dispatch is unavailable (no workspace registry configured)")

// Model-facing terminal statuses, aligned to the A2A task states a
// dispatch maps onto (#66/#71) — kept distinct from the registry's
// internal lifecycle vocabulary (dispatch.Status).
const (
	dispatchStatusCompleted = "completed"
	dispatchStatusFailed    = "failed"
)

// DispatchAgentParams are the arguments to the dispatch_agent tool.
type DispatchAgentParams struct {
	Prompt string `json:"prompt" description:"The task for the dispatched agent to perform, self-contained enough to act on without the current conversation."`
	Model  string `json:"model,omitempty" description:"Model to run the dispatched agent with: \"large\" or \"small\". Defaults to the small model."`
	Branch string `json:"branch,omitempty" description:"Base ref to fork the workspace from. Defaults to the current tree."`
}

// DispatchResult is the terminal payload the dispatch_agent tool returns
// to the main agent. Its fields are pinned to the A2A task mapping so the
// #71 transport swap is invisible to the model: Status maps to task
// state, Findings to the terminal status-message text, and Diff to the
// completion artifact.
type DispatchResult struct {
	// DispatchID identifies the dispatch and its workspace in the
	// registry, for follow-up queries and cleanup.
	DispatchID string `json:"dispatch_id"`
	// SessionID is the ephemeral session the dispatched agent ran on.
	SessionID string `json:"session_id,omitempty"`
	// WorkspacePath is the isolated workspace the agent worked in.
	WorkspacePath string `json:"workspace_path"`
	// Status is the terminal dispatch state: "completed" or "failed".
	Status string `json:"status"`
	// Findings is the dispatched agent's final text output — its summary
	// of what it did.
	Findings string `json:"findings,omitempty"`
	// Diff is the unified diff of the workspace's work product (committed
	// plus uncommitted) against the fork point. Empty when the agent
	// changed nothing or the run failed before producing work.
	Diff string `json:"diff,omitempty"`
	// Error carries the failure reason when Status is "failed".
	Error string `json:"error,omitempty"`
}

// dispatchAgentTool builds the dispatch_agent tool. It is a parallel
// tool: the model can fire several dispatches in one turn and they run
// concurrently, each in its own workspace, each blocking until its
// dispatched agent finishes and returns a DispatchResult.
func (c *coordinator) dispatchAgentTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		DispatchAgentToolName,
		dispatchAgentDescription,
		func(ctx context.Context, params DispatchAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			result, err := c.runDispatch(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("dispatch failed: %s", err)), nil
			}

			payload, err := json.Marshal(result)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("marshal dispatch result: %w", err)
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(string(payload)), result), nil
		},
	)
}

// runDispatch provisions an isolated workspace, builds a dispatched agent
// rooted at it, runs the task to completion on an ephemeral session, and
// assembles the terminal DispatchResult (status, findings, diff). The
// error return is reserved for failures that prevent a dispatch from
// starting at all (no registry, workspace provisioning); once a run
// begins, its outcome is reported inside the DispatchResult.
func (c *coordinator) runDispatch(ctx context.Context, params DispatchAgentParams) (DispatchResult, error) {
	if c.dispatchRegistry == nil {
		return DispatchResult{}, errDispatchUnavailable
	}

	ws, err := c.dispatchRegistry.Create(ctx, params.Branch)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("provision workspace: %w", err)
	}

	agent, model, err := c.buildDispatchAgent(ctx, ws.Path, params.Model)
	if err != nil {
		c.dispatchRegistry.SetStatus(ws.ID, dispatch.StatusFailed)
		return c.failedResult(ws, "", fmt.Sprintf("build dispatched agent: %s", err)), nil
	}

	sess, err := agent.Sessions.Create(ctx, "Dispatch")
	if err != nil {
		c.dispatchRegistry.SetStatus(ws.ID, dispatch.StatusFailed)
		return c.failedResult(ws, "", fmt.Sprintf("create dispatch session: %s", err)), nil
	}
	c.dispatchRegistry.SetSessionID(ws.ID, sess.ID)
	c.dispatchRegistry.SetStatus(ws.ID, dispatch.StatusRunning)

	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}
	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		c.dispatchRegistry.SetStatus(ws.ID, dispatch.StatusFailed)
		return c.failedResult(ws, sess.ID, errModelProviderNotConfigured.Error()), nil
	}
	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)

	var result *fantasy.AgentResult
	err = c.runWithUnauthorizedRetry(ctx, providerCfg, func() error {
		var runErr error
		result, runErr = agent.Run(ctx, SessionAgentCall{
			SessionID:        sess.ID,
			Prompt:           params.Prompt,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  mergedOptions,
			Temperature:      temp,
			TopP:             topP,
			TopK:             topK,
			FrequencyPenalty: freqPenalty,
			PresencePenalty:  presPenalty,
			NonInteractive:   true,
		})
		return runErr
	})

	return c.assembleResult(ctx, ws, sess.ID, result, err), nil
}

// assembleResult maps a dispatched run's outcome onto a DispatchResult
// and records the terminal status in the registry. A run error, or a nil
// result (Run returning (nil, nil) means no turn ran — busy session or a
// cancel during dispatch, never success), is a failure. A successful run
// captures the workspace diff as the work-product artifact.
func (c *coordinator) assembleResult(ctx context.Context, ws dispatch.Workspace, sessionID string, result *fantasy.AgentResult, runErr error) DispatchResult {
	switch {
	case runErr != nil:
		c.dispatchRegistry.SetStatus(ws.ID, dispatch.StatusFailed)
		return c.failedResult(ws, sessionID, runErr.Error())
	case result == nil:
		c.dispatchRegistry.SetStatus(ws.ID, dispatch.StatusFailed)
		return c.failedResult(ws, sessionID, "dispatched agent did not start a turn (session busy or canceled)")
	}

	c.dispatchRegistry.SetStatus(ws.ID, dispatch.StatusComplete)
	res := DispatchResult{
		DispatchID:    ws.ID,
		SessionID:     sessionID,
		WorkspacePath: ws.Path,
		Status:        dispatchStatusCompleted,
		Findings:      result.Response.Content.Text(),
	}

	// The diff is the work product. A capture failure (e.g. an unborn
	// repo) is not a run failure — the agent's findings still stand — so
	// it is logged inside Diff-collection and simply omitted here.
	if diff, err := c.dispatchRegistry.Diff(ctx, ws.ID); err == nil {
		res.Diff = diff
	}
	return res
}

// failedResult builds a failed DispatchResult carrying reason.
func (c *coordinator) failedResult(ws dispatch.Workspace, sessionID, reason string) DispatchResult {
	return DispatchResult{
		DispatchID:    ws.ID,
		SessionID:     sessionID,
		WorkspacePath: ws.Path,
		Status:        dispatchStatusFailed,
		Error:         reason,
	}
}

// buildDispatchAgent constructs the ephemeral, write-capable agent for a
// dispatch: the coder's toolchain rooted at workspacePath (#62), minus
// the tools that would let a dispatch fan out further or push to the
// Sidekick dashboard (agent, dispatch_agent, sidekick_update), on a
// private in-memory store so nothing persists. modelName selects "large"
// or "small" (default small).
func (c *coordinator) buildDispatchAgent(ctx context.Context, workspacePath, modelName string) (*EphemeralAgent, Model, error) {
	coderCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, Model{}, errCoderAgentNotConfigured
	}

	// A dispatched agent must not recurse into more sub-agents/dispatches
	// or push dashboard surfaces; it is otherwise the full coder toolset.
	dispatchCfg := coderCfg
	dispatchCfg.AllowedTools = slices.DeleteFunc(slices.Clone(coderCfg.AllowedTools), func(name string) bool {
		return name == AgentToolName || name == DispatchAgentToolName || name == tools.SidekickUpdateToolName
	})

	large, small, err := c.buildAgentModels(ctx, true)
	if err != nil {
		return nil, Model{}, err
	}
	model := small
	if modelName == string(config.SelectedModelTypeLarge) {
		model = large
	}

	agentTools, err := c.buildTools(ctx, dispatchCfg, true, workspacePath)
	if err != nil {
		return nil, Model{}, err
	}

	p, err := coderPrompt(prompt.WithWorkingDir(workspacePath))
	if err != nil {
		return nil, Model{}, err
	}
	systemPrompt, err := p.Build(ctx, model.Model.Provider(), model.Model.Model(), c.cfg)
	if err != nil {
		return nil, Model{}, err
	}

	providerCfg, _ := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	agent := NewEphemeralAgent(SessionAgentOptions{
		LargeModel:         model,
		SmallModel:         model,
		SystemPromptPrefix: providerCfg.SystemPromptPrefix,
		SystemPrompt:       systemPrompt,
		IsSubAgent:         true,
		IsYolo:             c.permissions.SkipRequests(),
		Cfg:                c.cfg,
		Tools:              agentTools,
	})
	return agent, model, nil
}

// CleanupDispatches tears down every workspace this coordinator
// provisioned. It is safe to call when no registry was configured.
func (c *coordinator) CleanupDispatches(ctx context.Context) error {
	if c.dispatchRegistry == nil {
		return nil
	}
	return c.dispatchRegistry.Close(ctx)
}
