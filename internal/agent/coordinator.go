package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/agent/notify"
	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/event"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	openaioauth "github.com/charmbracelet/crush/internal/oauth/openai"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/question"
	"github.com/charmbracelet/crush/internal/scheduler"
	"github.com/charmbracelet/crush/internal/semantic"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/symbols"
	"golang.org/x/sync/errgroup"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/charmbracelet/openai-go/option"
	"github.com/qjebbs/go-jsons"
)

// Coordinator errors.
var (
	errCoderAgentNotConfigured    = errors.New("coder agent not configured")
	errMainAgentNotFound          = errors.New("main agent not found")
	errModelProviderNotConfigured = errors.New("model provider not configured")
	errModelNotFound              = errors.New("model not found in provider config")
	errLargeModelNotSelected      = errors.New("large model not selected")
	errSmallModelNotSelected      = errors.New("small model not selected")
)

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
	"gpt-5.6-luna":  true,
	"gpt-5.6-terra": true,
	"gpt-5.6-sol":   true,
	"gpt-6-astra":   true,
	"grok-4.5":      true,
	"grok-4.6":      true,
}

// OpenCode models that use the Anthropic Messages API instead of Chat
// Completions. Which endpoint serves each model differs per provider, see
// https://opencode.ai/docs/zen and https://opencode.ai/docs/go.
func isOpenCodeMessagesModel(providerID, modelID string) bool {
	switch providerID {
	case string(catwalk.InferenceProviderOpenCodeGo):
		return strings.HasPrefix(modelID, "minimax-") ||
			strings.HasPrefix(modelID, "qwen3.6-") ||
			strings.HasPrefix(modelID, "qwen3.7-") ||
			strings.HasPrefix(modelID, "qwen3.8-")
	case string(catwalk.InferenceProviderOpenCodeZen):
		return strings.HasPrefix(modelID, "claude-") ||
			strings.HasPrefix(modelID, "qwen3.5-") ||
			strings.HasPrefix(modelID, "qwen3.6-") ||
			strings.HasPrefix(modelID, "qwen3.7-") ||
			strings.HasPrefix(modelID, "qwen3.8-")
	}
	return false
}

// OpenCode models that use the OpenAI Responses API instead of Chat
// Completions. See https://opencode.ai/docs/zen and https://opencode.ai/docs/go.
func isOpenCodeResponsesModel(modelID string) bool {
	return strings.HasPrefix(modelID, "gpt-") ||
		strings.HasPrefix(modelID, "grok-") ||
		strings.HasPrefix(modelID, "muse-spark-")
}

type Coordinator interface {
	SetMainAgent(agentName string) error
	// SetInteractive flips the coordinator between the interactive and
	// non-interactive tool palettes in place (#420): the question,
	// dispatch, message_agent and cancel tools follow the flag in
	// buildTools. A re-init after a new client attach calls it instead
	// of rebuilding the coordinator, so the dispatch registry, the
	// injection targets, the cron scheduler and the sweep survive.
	// Same-mode calls are a no-op; the last attach decides the mode.
	SetInteractive(ctx context.Context, interactive bool) error
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunAccepted runs a call that was already accepted via
	// BeginAccepted on the fire-and-forget dispatch path. The handle is
	// the only carrier of accept-state across the backend.runAgent /
	// Coordinator / sessionAgent.Run layers: it reaches
	// sessionAgent.Run as SessionAgentCall.Accepted, where it is
	// consumed under dispatchMu once the accepted -> (cancel-on-entry |
	// queued | active) transition is chosen.
	RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	ListCronTasks(sessionID string) []scheduler.Task
	Summarize(context.Context, string) error
	Model() Model
	UpdateModels(ctx context.Context) error
	GenerateTitle(ctx context.Context, sessionID, prompt string)
	// DeliverAgentMessage delivers a message to the dispatched agent
	// running on the message's session as its next input (#312) — the
	// transport-agnostic injection seam shared by the message_agent tool,
	// the editor's @handle routing (#313), and #71's A2A follow-up
	// messages. Addressing a finished session returns a refusal; so does
	// a message whose FromSessionID does not own the dispatch (#399).
	DeliverAgentMessage(ctx context.Context, msg AgentMessage) error
	// DispatchLive returns the snapshots of every non-terminal dispatch
	// created from sessionID (#313/#399): a session with one still
	// running cannot be deleted. Finished handles and other sessions'
	// dispatches never appear.
	DispatchLive(sessionID string) []dispatch.TodoSnapshot
}

// liveDispatch is the coordinator's record of one running dispatch
// (#371): the cancel for the dispatch's root context, the task session
// and agent behind the run, the wander-kill state, and a done channel
// closed when the run's teardown drops the record. #372 (cancel at
// shutdown) and #373 (user-initiated cancel) build on it.
type liveDispatch struct {
	cancel    context.CancelFunc
	sessionID string
	agent     SessionAgent
	kill      *dispatchKill
	done      chan struct{}
}

type coordinator struct {
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	questions   question.Service
	history     history.Service
	filetracker filetracker.Service
	lspManager  *lsp.Manager
	notify      pubsub.Publisher[notify.Notification]
	runComplete pubsub.Publisher[notify.RunComplete]
	interactive bool

	// agentMu guards mainAgent, mainAgentName and interactive:
	// SetMainAgent and SetInteractive run on HTTP handler goroutines
	// while runs, cancels, and probes read them from their own
	// goroutines.
	agentMu       sync.RWMutex
	mainAgent     SessionAgent
	mainAgentName string
	agents        map[string]SessionAgent

	// buildsMu guards builds: the readiness latch of every agent build
	// whose background work has not finished (#515). That work renders
	// the system prompt, which runs git in the working directory, and
	// nothing else joins it: every run rebuilds the tool palette, and the
	// task sub-agent built there is only waited on if it ever runs.
	// Teardown waits on these latches instead (waitAgentBuilds). Lazily
	// created because tests construct the coordinator struct directly.
	buildsMu sync.Mutex
	builds   map[*readiness]struct{}
	// promptBuildHook runs on an agent build's system-prompt goroutine
	// just before the render; a test-only seam, nil in production.
	promptBuildHook func()

	cronStore *scheduler.Store

	// Worktree dispatch (#61): the agent registry is created eagerly (it
	// is pure in-memory state and never fails), while the git worktree
	// provider is created lazily on first dispatch because a non-git
	// working directory must not fail coordinator construction, and the
	// failure is cached so later dispatches report it instead of
	// retrying. dispatchAgentBuilder is the dispatched-agent constructor
	// the DispatchAgent tool uses; nil means the real one, and tests
	// substitute a fake through it.
	dispatchMu           sync.Mutex
	dispatchReg          *dispatch.AgentRegistry
	dispatchProvider     *dispatch.GitWorktreeProvider
	dispatchProviderErr  error
	dispatchAgentBuilder func(context.Context, dispatchAgentOptions) (*dispatchedAgent, error)
	// spawnDispatch starts a dispatch's background run; nil means the
	// production `go f()`. Tests install a WaitGroup-backed spawner so
	// a test can never end while a runDispatch it started is still
	// running (#422).
	spawnDispatch func(func())
	// pendingResults holds each parent session's dispatch results whose
	// delivery turn has not succeeded yet (#388): a result arrives while
	// the parent is busy, so it waits here — outside the prompt queue
	// the user's Esc clears — and flushPendingResults delivers it on the
	// next idle. Guarded by dispatchMu; lazily created because tests
	// construct the coordinator struct directly.
	pendingResults map[string][]dispatch.DispatchResult
	// dispatchCollector reduces dispatched-session state into
	// per-dispatch snapshots for the configured sinks (#65); created
	// with the provider and run on the coordinator's lifetime context.
	dispatchCollector *dispatch.TodoCollector
	// liveDispatches maps each running dispatch's ID to its live record
	// (#371): the per-dispatch root cancel, the task session and agent
	// behind the run, the wander-kill, and a done channel closed at
	// teardown. Guarded by dispatchMu; #372 (cancel at shutdown) and
	// #373 (user-initiated cancel) build on it.
	liveDispatches map[string]*liveDispatch
	// shuttingDown is set when CancelAll begins (#372): while it is set,
	// a finishing dispatch still records its terminal result and status
	// in the registry, but neither deliverDispatchResult nor the
	// run-end flush starts a parent turn.
	shuttingDown atomic.Bool
	// dispatchShutdownWait bounds the shared wait CancelAll gives the
	// live dispatches' graceful agent.Cancel to finish each run (#372);
	// dispatchShutdownRootWait is the extra shared bound the root-cancel
	// fallback gets. Zero keeps the defaults, 5s and 1s, matching
	// sessionAgent.CancelAll's own bound; tests shorten them.
	dispatchShutdownWait     time.Duration
	dispatchShutdownRootWait time.Duration
	// dispatchSlots counts the dispatch slots currently held (#390):
	// setups in flight plus live dispatches, capped by the resolved
	// dispatch.max_concurrent. Guarded by dispatchMu.
	dispatchSlots int
	// dispatchCtx is the NewCoordinator context the collector's
	// subscriptions run on; nil-safe (tests construct the coordinator
	// struct directly) — dispatchWorkspaceProvider falls back to
	// context.Background.
	dispatchCtx   context.Context
	dispatchSinks []dispatch.TodoSink
	// dispatchHost is the A2A protocol boundary every dispatch runs
	// behind (#70, #71); nil until the app wires a2a.ServerFactory, and
	// nil means dispatches are refused at the tool — nothing runs
	// unserved (#347).
	dispatchHost DispatchHost
	// dispatchQuestions is the one-slot semaphore dispatched agents'
	// questions take turns on (#352): the question service holds one
	// pending question at a time. Made on first use under dispatchMu.
	dispatchQuestions chan struct{}
	// dispatchPersistWindow bounds how long a finished run waits for the
	// parent turn to persist its dispatch_agent tool result (#410).
	// NewCoordinator sets dispatchResultPersistWindow; a coordinator
	// built as a struct literal (tests) leaves it zero and makes a single
	// attempt, so a test that never writes the parent result does not sit
	// out the window.
	dispatchPersistWindow time.Duration

	// dispatchRecords is the durable a2a_dispatches store (#355); nil
	// until the app wires it, and nil means dispatches leave no restart
	// record — every record write and the startup reconcile delivery
	// are nil-safe no-ops.
	dispatchRecords dispatch.DispatchRecords

	// semanticStore and semanticClient back the semantic_search and
	// semantic_index tools. Both are nil unless an embedding provider is
	// configured and the store initialised, in which case neither tool
	// is registered.
	semanticStore   *semantic.Store
	semanticClient  *semantic.Client
	semanticSymbols *symbols.Extractor

	// Skills discovery results (session-start snapshot).
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker
}

// CoordinatorOptions holds the dependencies for NewCoordinator. Using a
// struct keeps the constructor self-documenting and avoids a long
// positional parameter list.
type CoordinatorOptions struct {
	Config      *config.ConfigStore
	Sessions    session.Service
	Messages    message.Service
	Permissions permission.Service
	Questions   question.Service
	History     history.Service
	FileTracker filetracker.Service
	LSPManager  *lsp.Manager
	Notify      pubsub.Publisher[notify.Notification]
	RunComplete pubsub.Publisher[notify.RunComplete]
	Skills      *skills.Manager
	Interactive bool

	// SemanticStore/SemanticClient enable the semantic_search and
	// semantic_index tools. Both must be non-nil for the tools to be
	// registered; the caller (app) is responsible for constructing them
	// from the configured embedding provider.
	SemanticStore  *semantic.Store
	SemanticClient *semantic.Client

	// DispatchSinks receive the per-dispatch todo snapshots the
	// collector reduces (#65). The app passes the broker sink that
	// feeds the agent block; #174's A2A TaskStatusUpdateEvent bridge
	// will attach as a second sink over the same reduction. Empty in
	// callers that do not observe progress.
	DispatchSinks []dispatch.TodoSink

	// DispatchHost is the A2A protocol boundary every dispatch runs
	// behind (#70, #71): the app passes a2a.ServerFactory. Nil (tests
	// that opt out, or any caller without the factory wired) means
	// dispatches are refused at the tool — there is no unserved run
	// (#347).
	DispatchHost DispatchHost

	// DispatchRecords persists the durable dispatch records (#355); the
	// app passes dispatch.NewSQLiteRecordStore. Nil (tests, or any
	// caller without the database wired) means dispatches keep no
	// restart record, which is exactly the pre-#355 behavior.
	DispatchRecords dispatch.DispatchRecords
}

func NewCoordinator(ctx context.Context, opts CoordinatorOptions) (Coordinator, error) {
	// Skills are pre-discovered by the caller (see app.New /
	// backend.CreateWorkspace) and passed in via the manager. If no
	// manager was provided (legacy callers), fall back to an in-line
	// discovery so the coordinator still works.
	var allSkills, activeSkills []*skills.Skill
	if opts.Skills != nil {
		allSkills = opts.Skills.AllSkills()
		activeSkills = opts.Skills.ActiveSkills()
	} else {
		allSkills, activeSkills = discoverSkills(opts.Config)
	}
	skillTracker := skills.NewTracker(activeSkills)

	cronStore := scheduler.NewStore(filepath.Join(opts.Config.Config().Options.DataDirectory, "scheduled_tasks.json"))
	if err := cronStore.Load(); err != nil {
		slog.Error("Failed to load scheduled tasks", "error", err)
	}

	c := &coordinator{
		cfg:             opts.Config,
		sessions:        opts.Sessions,
		messages:        opts.Messages,
		permissions:     opts.Permissions,
		questions:       opts.Questions,
		history:         opts.History,
		filetracker:     opts.FileTracker,
		lspManager:      opts.LSPManager,
		notify:          opts.Notify,
		runComplete:     opts.RunComplete,
		agents:          make(map[string]SessionAgent),
		cronStore:       cronStore,
		allSkills:       allSkills,
		activeSkills:    activeSkills,
		skillTracker:    skillTracker,
		interactive:     opts.Interactive,
		semanticStore:   opts.SemanticStore,
		semanticClient:  opts.SemanticClient,
		semanticSymbols: symbols.NewExtractor(),
		dispatchReg:     dispatch.NewAgentRegistry(),
		dispatchCtx:     ctx,
		dispatchSinks:   opts.DispatchSinks,
		dispatchHost:    opts.DispatchHost,
		dispatchRecords: opts.DispatchRecords,

		dispatchPersistWindow: dispatchResultPersistWindow,
	}

	agentCfg, ok := opts.Config.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// A2UI is on unless the user disables it; see prompt.WithA2UI for the
	// host-capability trade-off. The cron tools are registered
	// unconditionally in buildAgent, so the matching prompt guidance is
	// always on for the coder agent. These coder-only sections ride the
	// coder agent's build (#432): the definition's prompt — builtin or
	// file — renders with them, and a file-based template that does not
	// reference the fields is unaffected.
	coderOpts := []prompt.Option{prompt.WithScheduling()}
	if !c.cfg.Config().Options.DisableA2UI {
		coderOpts = append(coderOpts, prompt.WithA2UI())
	}

	agent, err := c.buildAgent(ctx, agentCfg, false, coderOpts...)
	if err != nil {
		return nil, err
	}
	c.agents[config.AgentCoder] = agent

	planCfg, ok := c.cfg.Config().Agents[config.AgentPlan]
	if ok && !planCfg.Disabled {
		planAgent, err := c.buildAgent(ctx, planCfg, false)
		if err != nil {
			return nil, err
		}
		c.agents[config.AgentPlan] = planAgent
	}

	// #392: every agent definition is published on the per-process A2A
	// host as one stable card, whether or not its entry point routes
	// its turns through the runtime yet — the definitions are the
	// host's card listing, and the per-definition routes are live
	// protocol surfaces that reject runs until an entry point serves a
	// turn on them. Publishing is best-effort: a host that cannot
	// listen fails the runs that need it, not the whole coordinator.
	if catalog, ok := opts.DispatchHost.(AgentCatalog); ok {
		for id, ag := range opts.Config.Config().Agents {
			if ag.Disabled {
				continue
			}
			if err := catalog.PublishAgentDefinition(ctx, AgentDefinitionCard{
				ID:          id,
				Name:        ag.Name,
				Description: ag.Description,
			}); err != nil {
				slog.Error("Failed to publish agent definition card", "agent", id, "error", err)
			}
		}
	}

	cronScheduler := scheduler.NewScheduler(c.cronStore, c.fireScheduledTask)
	go cronScheduler.Run(ctx)

	// Dispatch session-end backstop: release the workspaces that are
	// safe to remove when the coordinator's context ends (#63's Sweep,
	// wired here per #64; #367 makes the pass selective, keeping work
	// worth salvaging on disk).
	go c.sweepDispatchOnDone(ctx)

	c.mainAgent = agent
	c.mainAgentName = config.AgentCoder
	return c, nil
}

// activeAgent returns the coordinator's current main agent and its config
// name as one snapshot. Callers use the snapshot for the whole operation,
// so a SetMainAgent racing mid-flight never splits a run, model refresh, or
// summarize across two agents.
func (c *coordinator) activeAgent() (SessionAgent, string) {
	c.agentMu.RLock()
	defer c.agentMu.RUnlock()
	return c.mainAgent, c.mainAgentName
}

// currentAgent returns the current main agent.
func (c *coordinator) currentAgent() SessionAgent {
	agent, _ := c.activeAgent()
	return agent
}

func (c *coordinator) SetMainAgent(agentName string) error {
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	agent, ok := c.agents[agentName]
	if !ok {
		if c.cfg != nil {
			if cfg, ok := c.cfg.Config().Agents[agentName]; ok && cfg.Disabled {
				return fmt.Errorf("agent %q is disabled", agentName)
			}
		}
		return fmt.Errorf("%w: %s", errMainAgentNotFound, agentName)
	}
	c.mainAgent = agent
	c.mainAgentName = agentName
	return nil
}

// isInteractive reports the current tool-palette mode. Reads take the
// lock because SetInteractive flips the flag from attach goroutines.
func (c *coordinator) isInteractive() bool {
	c.agentMu.RLock()
	defer c.agentMu.RUnlock()
	return c.interactive
}

// SetInteractive implements Coordinator.
func (c *coordinator) SetInteractive(ctx context.Context, interactive bool) error {
	c.agentMu.Lock()
	if c.interactive == interactive {
		c.agentMu.Unlock()
		return nil
	}
	c.interactive = interactive
	c.agentMu.Unlock()

	// The flag flips before the rebuild so a run interleaving mid-flight
	// picks up the new palette on its own updateAgentModels pass, and so
	// a partially-failed rebuild self-heals on the next run. Rebuild
	// without agentMu held (run and UpdateModels call it the same way),
	// and cover both main agents so a SetMainAgent'd plan agent is
	// rebuilt too. The dispatch registry, the injection targets, the
	// cron scheduler and the sweep are deliberately untouched: they
	// belong to the coordinator, not to either palette (#420).
	for _, name := range []string{config.AgentCoder, config.AgentPlan} {
		c.agentMu.RLock()
		agent, ok := c.agents[name]
		c.agentMu.RUnlock()
		if !ok || agent == nil {
			continue
		}
		if err := c.updateAgentModels(ctx, agent, name); err != nil {
			return err
		}
	}
	return nil
}

// fireScheduledTask runs a due scheduled task's prompt against its
// session. The prompt is injected as a normal user turn so it respects
// the session's busy queue: it fires between turns, never mid-response,
// matching Claude Code's scheduler semantics.
func (c *coordinator) fireScheduledTask(ctx context.Context, task scheduler.Task) error {
	if _, err := c.sessions.Get(ctx, task.SessionID); err != nil {
		// The session is gone (deleted or never persisted), so nothing
		// this task fires can ever land. Drop the whole session's tasks,
		// durable ones included: leaving them behind means the scheduler
		// retries a dead session on every fire, forever.
		c.cronStore.DropSession(task.SessionID)
		return fmt.Errorf("session %s for scheduled task %s no longer exists", task.SessionID, task.ID)
	}

	prompt := task.Prompt
	go func() {
		if _, err := c.run(ctx, nil, task.SessionID, prompt); err != nil {
			slog.Error("Scheduled task run failed", "id", task.ID, "session_id", task.SessionID, "error", err)
		}
	}()
	return nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, nil, sessionID, prompt, attachments...)
}

// RunAccepted implements Coordinator.
func (c *coordinator) RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, accept, sessionID, prompt, attachments...)
}

// run is the shared implementation behind Run and RunAccepted. When
// accept is non-nil it is threaded onto the SessionAgentCall as
// Accepted so sessionAgent.Run can consume the accept reservation under
// dispatchMu; when nil (the in-process/local path) no accept tracking
// applies.
func (c *coordinator) run(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// MCP servers connect asynchronously (see mcp.Initialize).
	//
	// Interactive runs never wait for that to finish: the tool list below
	// is built from whatever is registered right now, servers still
	// connecting are simply absent from this run's palette, and they are
	// picked up by later runs once they register and publish
	// EventToolsListChanged. Blocking here froze the TUI for the duration
	// of the slowest server's connect timeout whenever a prompt was sent
	// before initialization finished — most visibly on the first message.
	//
	// Non-interactive runs get a single shot at the tool palette, so they
	// do wait for initialization to settle — but bounded by InitWaitBudget
	// rather than each server's connect timeout, so a server wedged
	// mid-handshake cannot stall a headless run for minutes. Past the
	// budget the turn proceeds without the stragglers; their tools simply
	// stay absent from this run.
	if !c.isInteractive() {
		if err := mcp.WaitForInitBudget(ctx, mcp.InitWaitBudget); err != nil {
			return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
		}
	}

	// refresh models before each run. Snapshot the agent first: the run,
	// its model settings, and the model refresh below must all target the
	// same agent even if SetMainAgent swaps the main agent mid-flight.
	agent, agentName := c.activeAgent()

	// Wait for this agent's own build-time setup (system prompt, initial
	// tool list) rather than for coordinator-wide state: a latch belonging
	// to the agent cannot be rearmed by a later build while this run is
	// parked on it. Waiting on the snapshot keeps the wait and the run on
	// the same agent. See readiness and joestump-agent/crush#298.
	if err := agent.WaitReady(); err != nil {
		return nil, err
	}

	if err := c.updateAgentModels(ctx, agent, agentName); err != nil {
		return nil, fmt.Errorf("failed to update models: %w", err)
	}

	model := agent.Model()
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", err)
	}

	// Coalesce per-attempt RunComplete payloads so only the final
	// outcome reaches subscribers. Without this, the first attempt's
	// failed RunComplete (unauthorized) would race ahead of the
	// retry's success, and `crush run` would exit on the stale error
	// before ever seeing the retry result. Each attempt's
	// SessionAgentCall.OnComplete hook overwrites latest; we publish
	// exactly once after retries resolve, via PublishMustDeliver, so
	// a momentarily-full subscriber buffer can't silently drop the
	// terminal event.
	var (
		latest    notify.RunComplete
		hasLatest bool
	)
	onComplete := func(rc notify.RunComplete) {
		latest = rc
		hasLatest = true
	}
	// Propagate the caller-supplied RunID (set via agent.WithRunID
	// at the HTTP boundary in backend.SendMessage) onto the
	// SessionAgentCall so the terminal RunComplete event echoes it
	// back. Both attempts in the retry chain reuse the same RunID;
	// the coalesce closure publishes the final outcome under that
	// same correlator.
	runID := RunIDFromContext(ctx)
	systemDelivery := SystemDeliveryFromContext(ctx)
	// A delivery turn's queue verdict (#355): set only on the dispatch
	// delivery path, it stamps the durable records once a queued
	// delivery reaches the parent. Nil everywhere else.
	onConsumed := deliveryConsumedFromContext(ctx)
	run := func() (*fantasy.AgentResult, error) {
		return agent.Run(ctx, SessionAgentCall{
			SessionID:         sessionID,
			RunID:             runID,
			HiddenUserMessage: message.HiddenUserMessage(ctx),
			Channel:           ChannelFromContext(ctx),
			ContentWidth:      ContentWidthFromContext(ctx),
			Prompt:            prompt,
			Attachments:       attachments,
			MaxOutputTokens:   maxTokens,
			ProviderOptions:   mergedOptions,
			Temperature:       temp,
			TopP:              topP,
			TopK:              callTopK(providerCfg, topK),
			FrequencyPenalty:  freqPenalty,
			PresencePenalty:   presPenalty,
			systemDelivery:    systemDelivery,
			OnComplete:        onComplete,
			OnConsumed:        onConsumed,
			Accepted:          accept,
			OnAuthRefresh:     c.makeAuthRefreshCallback(providerCfg),
		})
	}
	beforeLoaded := c.skillTracker.LoadedNames()
	result, originalErr := run()
	logTurnSkillUsage(sessionID, prompt, c.activeSkills, c.skillTracker, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate. AWS SSO is
	// handled transparently inside OnAuthRefresh, so it needs no post-run
	// notification here.
	if originalErr != nil && isUnauthorized(originalErr) && c.notify != nil && model.ModelCfg.Provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: model.ModelCfg.Provider,
		})
	}

	if hasLatest && c.runComplete != nil {
		c.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, latest)
		// Signal to the dispatcher (backend.runAgent) that the
		// authoritative terminal RunComplete for this run was already
		// emitted, so it does not publish a duplicate fallback for the
		// error it is about to receive.
		MarkRunCompletePublished(ctx)
	}
	// A run on this session just ended (#388): dispatch results that
	// pended while it was busy can deliver now. Detached: the flush
	// runs its own turn.
	go c.flushPendingResults(sessionID)
	return result, originalErr
}

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model Model) string {
	if !model.CatwalkCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatwalkCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatwalkCfg.ReasoningLevels) > 0 {
		return model.CatwalkCfg.ReasoningLevels[0]
	}
	return ""
}

func getProviderOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatwalkCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)

	switch providerCfg.Type {
	case openai.Name, azure.Name:
		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
		if openai.IsResponsesModel(model.CatwalkCfg.ID) {
			if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
				mergedOptions["reasoning_summary"] = "auto"
				mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				options[openai.Name] = parsed
			}
		}

	case anthropic.Name, bedrock.Name:
		var (
			_, hasEffort = mergedOptions["effort"]
			_, hasThink  = mergedOptions["thinking"]
			extraBody    = make(map[string]any)
		)

		switch providerCfg.ID {
		case string(catwalk.InferenceProviderAlibabaSingapore), string(catwalk.InferenceProviderAlibabaUS):
			switch {
			case !hasEffort && shouldSetEffort:
				extraBody["reasoning_effort"] = reasoningEffort
			case !hasThink && model.CatwalkCfg.CanReason:
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}
			mergedOptions["extra_body"] = extraBody

		default:
			switch {
			case !hasEffort && shouldSetEffort:
				mergedOptions["effort"] = reasoningEffort
			case !hasThink && model.ModelCfg.Think:
				mergedOptions["thinking"] = map[string]any{"budget_tokens": 2000}
			}
		}

		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		}

	case openrouter.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		}

	case vercel.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		}

	case google.Name:
		_, hasReasoning := mergedOptions["thinking_config"]
		if !hasReasoning {
			if strings.HasPrefix(model.CatwalkCfg.ID, "gemini-2") {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_budget":  2000,
					"include_thoughts": true,
				}
			} else {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_level":   reasoningEffort,
					"include_thoughts": true,
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		}

	case openaicompat.Name, hyper.Name:
		extraBody := make(map[string]any)

		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			switch providerCfg.ID {
			case string(catwalk.InferenceProviderIoNet):
				extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
			case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
				// MiniMax models use the "thinking" parameter instead of
				// "reasoning_effort". Other models on these providers still
				// use the standard field.
				if !strings.HasPrefix(strings.ToLower(model.CatwalkCfg.ID), "minimax") {
					mergedOptions["reasoning_effort"] = reasoningEffort
				}
			default:
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		}

		// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
		// Setting it in the right way for each provider.
		// TODO: Abstract this in Fantasy somehow?
		// TODO: Allow custom providers to specify how to set this?
		switch providerCfg.ID {
		case hyper.Name:
			extraBody["thinking"] = model.ModelCfg.Think
		case string(catwalk.InferenceProviderIoNet):
			if _, ok := extraBody["reasoning"]; !ok && model.CatwalkCfg.CanReason {
				if model.ModelCfg.Think {
					extraBody["reasoning"] = map[string]string{"effort": "medium"}
				} else {
					extraBody["reasoning"] = map[string]string{"effort": "none"}
				}
			}

		case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
			if model.ModelCfg.Think || reasoningEffort != "" {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}

		case string(catwalk.InferenceProviderFireworks):
			// NOTE: Fireworks break if we set both `reasoning_effort` and `thinking`.
			if reasoningEffort == "" {
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}

		case string(catwalk.InferenceProviderBaseten):
			extraBody["chat_template_args"] = map[string]any{
				"enable_thinking": model.ModelCfg.Think || reasoningEffort != "" && reasoningEffort != "none",
			}

		case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
			// MiniMax M3 uses the "thinking" parameter to control reasoning.
			// "reasoning_split" must be true so thinking content is returned
			// in the "reasoning_content" field instead of inline in "content".
			if strings.HasPrefix(strings.ToLower(model.CatwalkCfg.ID), "minimax") {
				if model.CatwalkCfg.CanReason && (model.ModelCfg.Think || reasoningEffort != "") {
					extraBody["thinking"] = map[string]any{"type": "adaptive"}
					extraBody["reasoning_split"] = true
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}

		case string(catwalk.InferenceProviderAlibabaSingapore), string(catwalk.InferenceProviderAlibabaUS):
			if model.CatwalkCfg.CanReason && !shouldSetEffort {
				extraBody["enable_thinking"] = model.ModelCfg.Think
			}
		}

		mergedOptions["extra_body"] = extraBody

		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			options[openaicompat.Name] = parsed
		}

	default:
		// Known custom providers (litellm, llamacpp, lmstudio, ollama,
		// omlx) are openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			// Set "top_k" under "extra_body", as it is not part of the OpenAI protocol
			// and will be explicitly omitted by Fantasy downstream.
			topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
			if topK != nil {
				extraBody, hasExtraBody := mergedOptions["extra_body"].(map[string]any)
				if !hasExtraBody {
					extraBody = make(map[string]any)
					mergedOptions["extra_body"] = extraBody
				}
				if _, hasTopK := extraBody["top_k"]; !hasTopK {
					extraBody["top_k"] = *topK
				}
			}

			_, hasReasoningEffort := mergedOptions["reasoning_effort"]
			if !hasReasoningEffort && shouldSetEffort {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}

			parsed, err := openaicompat.ParseOptions(mergedOptions)
			if err == nil {
				options[openaicompat.Name] = parsed
			} else {
				if topK != nil {
					slog.Warn(
						"Failed to parse provider_options, falling back to top_k only",
						"provider", providerCfg.ID,
						"error", err,
					)

					fallbackMergeOptions := map[string]any{
						"extra_body": map[string]any{"top_k": *topK},
					}
					parsed, err := openaicompat.ParseOptions(fallbackMergeOptions)
					if err == nil {
						options[openaicompat.Name] = parsed
					} else {
						slog.Warn(
							"Failed to parse fallback provider options, this should never happen",
							"provider", providerCfg.ID,
							"error", err,
						)
					}
				}
			}
		}
	}

	return options
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

// buildAgent builds one agent from its resolved definition (#432): the
// model comes from the definition (slot ref or explicit pin) over the
// configured slots, the prompt renders from the definition's prompt
// reference with the definition's context paths and skills, and the
// tools build from the definition's allow list. extraOpts carry
// caller-owned prompt sections — the coder-only scheduling guidance and
// A2UI; a file-based template that does not reference those fields is
// unaffected by them.
func (c *coordinator) buildAgent(ctx context.Context, agent config.Agent, isSubAgent bool, extraOpts ...prompt.Option) (SessionAgent, error) {
	large, small, err := c.buildAgentModels(ctx, isSubAgent)
	if err != nil {
		return nil, err
	}

	large, err = c.agentModel(ctx, agent, large, small, isSubAgent)
	if err != nil {
		return nil, err
	}

	largeProviderCfg, _ := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)

	promptOpts := append([]prompt.Option{
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
	}, extraOpts...)
	promptOpts = append(promptOpts, agentPromptOptions(agent)...)
	systemPromptTemplate, err := agentPrompt(agent, c.cfg.WorkingDir(), promptOpts...)
	if err != nil {
		return nil, err
	}

	result := newSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           small,
		SystemPromptPrefix:   largeProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Cfg:                  c.cfg,
		Tools:                nil,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
		// The todo enforcement ladder (#315) applies to every agent the
		// coordinator builds (main, plan, and agent-tool sub-agents
		// alike) with this agent type's overrides layered over the
		// global options.
		TodoEnforcement: agent.ResolvedTodoEnforcement(c.cfg.Config().Options.TodoEnforcement),
	})

	// The readiness goroutines below perform one-time setup — building the
	// system prompt and the initial tool list — whose results the
	// coordinator needs for its whole lifetime, so they must survive the
	// caller's context being canceled. Several entry points build an agent
	// from a short-lived HTTP request context: the server's
	// InitAgent/UpdateAgent handlers, and UpdateModels -> buildTools ->
	// agentTool -> buildAgent for the sub-agent. The tool-list build reads
	// the MCP registry as it stands; servers still connecting are picked up
	// by later runs. WithoutCancel drops cancellation while keeping context
	// values; the work is local and always completes.
	initCtx := context.WithoutCancel(ctx)

	// The group is local to this build and the latch belongs to the agent it
	// builds, so a later buildAgent never touches either. Sharing one group
	// across builds and runs is what crash-looped channel mode: a build's
	// Add landed while a run's Wait was outstanding, which Go 1.27's
	// sync.WaitGroup panics on (joestump-agent/crush#298).
	ready := newReadiness()
	result.ready = ready
	// The coordinator tracks the latch until the work below is done, so
	// teardown can wait for it (#515): the prompt render runs git in the
	// working directory, and an agent nobody runs is never waited on.
	c.trackBuild(ready)

	var build errgroup.Group
	build.Go(func() error {
		if c.promptBuildHook != nil {
			c.promptBuildHook()
		}
		systemPrompt, err := agentSystemPrompt(initCtx, systemPromptTemplate, agent, c.cfg.WorkingDir(), large.Model.Provider(), large.Model.Model(), c.cfg)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	})

	build.Go(func() error {
		tools, err := c.buildTools(initCtx, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	})

	go func() {
		err := build.Wait()
		// Untracked before the settle: the work is done, and a teardown
		// that already snapshotted the latch still returns on the settle.
		c.untrackBuild(ready)
		ready.settle(err)
	}()

	return result, nil
}

// trackBuild records an agent build's readiness latch as in flight
// (#515).
func (c *coordinator) trackBuild(r *readiness) {
	c.buildsMu.Lock()
	defer c.buildsMu.Unlock()
	if c.builds == nil {
		c.builds = make(map[*readiness]struct{})
	}
	c.builds[r] = struct{}{}
}

// untrackBuild drops a finished build's latch (#515).
func (c *coordinator) untrackBuild(r *readiness) {
	c.buildsMu.Lock()
	defer c.buildsMu.Unlock()
	delete(c.builds, r)
}

// waitAgentBuilds blocks until no agent build's background work is in
// flight, or ctx ends (#515). A build that starts while it waits — a run
// that was mid-rebuild when teardown began — is waited on too. Each
// latch is a one-shot channel, never a shared WaitGroup, so a build
// starting during the wait cannot trip Go 1.27's reuse panic (#298).
func (c *coordinator) waitAgentBuilds(ctx context.Context) error {
	for {
		c.buildsMu.Lock()
		pending := make([]*readiness, 0, len(c.builds))
		for r := range c.builds {
			pending = append(pending, r)
		}
		c.buildsMu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		for _, r := range pending {
			select {
			case <-r.done:
			case <-ctx.Done():
				return fmt.Errorf("%d agent builds still running: %w", len(pending), ctx.Err())
			}
		}
	}
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	// One snapshot for the whole palette: SetInteractive may flip the
	// mode mid-build, and a torn palette (question without dispatch) is
	// worse than either mode alone.
	interactive := c.isInteractive()

	// Effective palette: the definition's allow list (#432) minus the
	// user's disabled tools, applied last so a definition can never
	// widen user policy. A disabled task agent takes the `agent` tool
	// off the main agents' palettes — there is no sub-agent to run —
	// and with no enabled dispatch agent the dispatch, message, cancel,
	// apply, and dismiss tools have nothing to act on.
	allowedTools := effectiveToolNames(agent.AllowedTools, c.cfg.Config().Options.DisabledTools)
	if taskCfg, ok := c.cfg.Config().Agents[config.AgentTask]; ok && taskCfg.Disabled {
		allowedTools = slices.DeleteFunc(allowedTools, func(name string) bool {
			return name == AgentToolName
		})
	}
	if !hasEnabledDispatchAgent(c.cfg.Config()) {
		allowedTools = slices.DeleteFunc(allowedTools, func(name string) bool {
			return name == DispatchAgentToolName || name == MessageAgentToolName || name == CancelDispatchToolName ||
				name == ApplyDispatchToolName || name == DismissDispatchToolName
		})
	}

	var allTools []fantasy.AgentTool
	if slices.Contains(allowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agentTool)
	}

	if slices.Contains(allowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	// Worktree dispatch (#64) is a coder-level tool. Sub-agents never
	// get it: dispatching from inside a dispatch would recurse across
	// the isolation boundary the dispatch toolchain enforces. It is also
	// interactive-only (#387): a non-interactive `crush run` exits when
	// the parent's turn ends, so a dispatch would die mid-run with its
	// result undelivered.
	if !isSubAgent && interactive && slices.Contains(allowedTools, DispatchAgentToolName) {
		allTools = append(allTools, c.dispatchTool())
	}

	// Mid-run message injection (#312) is the model-facing front door of
	// the injection queue. Main agents only: a dispatched agent steering
	// a sibling would cross the same isolation boundary the dispatch
	// toolchain enforces, and worker-to-worker routing is deliberately
	// deferred (A2A epic #67). Interactive-only for the same reason
	// dispatch is (#387): a non-interactive run ends with the parent's
	// turn, so there is no live run left to inject into.
	if !isSubAgent && interactive && slices.Contains(allowedTools, MessageAgentToolName) {
		allTools = append(allTools, c.messageAgentTool())
	}

	// On-demand cancel (#373) is the model-facing front door for stopping
	// one dispatched agent. It rides the same gate as dispatch and
	// message: main agents only, interactive only — the same reasons
	// apply, and a tool with nothing running behind it is dead weight.
	if !isSubAgent && interactive && slices.Contains(allowedTools, CancelDispatchToolName) {
		allTools = append(allTools, c.cancelDispatchTool())
	}

	// Apply and dismiss (#368) are the model-facing front door for the
	// review decision a finished dispatch's terminal message asks for.
	// They ride the same gate as cancel: main agents only, interactive
	// only — a non-interactive run exits with the parent's turn, so
	// nothing is left to apply or dismiss.
	if !isSubAgent && interactive && slices.Contains(allowedTools, ApplyDispatchToolName) {
		allTools = append(allTools, c.applyDispatchTool())
	}
	if !isSubAgent && interactive && slices.Contains(allowedTools, DismissDispatchToolName) {
		allTools = append(allTools, c.dismissDispatchTool())
	}

	// Get the model name for the agent: an explicit pin (#432) names it,
	// otherwise the agent's slot does.
	modelID := ""
	if agent.ModelRef != nil {
		if model := c.cfg.Config().GetModel(agent.ModelRef.Provider, agent.ModelRef.Model); model != nil {
			modelID = model.ID
		}
	} else if modelCfg, ok := c.cfg.Config().Models[agent.Model]; ok {
		if model := c.cfg.Config().GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	logFile := filepath.Join(c.cfg.Config().Options.DataDirectory, "logs", "crush.log")

	// Effective bash allow-list: config-file values plus any runtime overrides
	// from --allow-commands / --allow-all-commands (or their env vars). The
	// overrides live on the store rather than in Options so they survive the
	// config reloads triggered by MCP reconnects and model changes.
	bashOpts := c.cfg.Config().Options
	bashOverrides := c.cfg.Overrides()
	allowedCommands := append(append([]string{}, bashOpts.AllowedCommands...), bashOverrides.AllowedCommands...)
	allowAllCommands := bashOpts.AllowAllCommands || bashOverrides.AllowAllCommands

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	allTools = append(
		allTools,
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.DataDirectory, c.cfg.Config().Options.Attribution, modelID, allowedCommands, allowAllCommands),
		tools.NewCrushInfoTool(c.cfg, c.lspManager, c.allSkills, c.activeSkills, c.skillTracker, c.semanticStore),
		tools.NewCrushLogsTool(logFile),
		tools.NewCronCreateTool(c.cronStore),
		tools.NewCronListTool(c.cronStore),
		tools.NewCronDeleteTool(c.cronStore),
		tools.NewJobOutputTool(c.cfg.Config().Options.DataDirectory),
		tools.NewJobKillTool(),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewGlobTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Glob),
		tools.NewGrepTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls),
		tools.NewSourcegraphTool(nil),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, c.skillTracker, c.cfg.WorkingDir(), c.cfg.Config().Options.SkillsPaths...),
		tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
	)

	// Question tool is interactive-only and not available to sub-agents.
	if !isSubAgent && interactive {
		allTools = append(allTools, tools.NewQuestionTool(c.questions))
	}

	// Add LSP tools if user has configured LSPs or auto_lsp is enabled (nil or true).
	if len(c.cfg.Config().LSP) > 0 || c.cfg.Config().Options.AutoLSP == nil || *c.cfg.Config().Options.AutoLSP {
		allTools = append(
			allTools,
			tools.NewDiagnosticsTool(c.lspManager),
			tools.NewReferencesTool(c.lspManager),
			tools.NewLSPRestartTool(c.lspManager),
			tools.NewSymbolsTool(c.lspManager),
			tools.NewDefinitionTool(c.lspManager),
			tools.NewCallHierarchyTool(c.lspManager),
			tools.NewRenameTool(c.lspManager, c.permissions, c.history, c.filetracker),
			tools.NewReplaceSymbolTool(c.lspManager, c.permissions, c.history, c.filetracker),
		)
	}

	// Semantic search tools are registered only when an embedding
	// provider is configured and the store initialised — the same
	// conditional-registration style as the LSP and MCP blocks above.
	if c.semanticStore != nil && c.semanticClient != nil {
		allTools = append(
			allTools,
			tools.NewSemanticSearchTool(c.cfg, c.semanticStore, c.semanticClient),
			tools.NewSemanticIndexTool(c.cfg, c.semanticStore, c.semanticSymbols, c.cfg.WorkingDir()),
		)
	}

	if len(c.cfg.Config().MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.permissions),
			tools.NewReadMCPResourceTool(c.cfg, c.permissions),
			tools.NewListMCPPromptsTool(c.cfg, c.permissions),
			tools.NewCallMCPPromptTool(c.cfg, c.permissions),
		)
	}

	var filteredTools []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(allowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	filteredTools = append(filteredTools, filterMCPTools(agent, tools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir()))...)
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// Wrap tools with hook interception for the top-level agent only.
	// Sub-agents (the `agent` task tool, `agentic_fetch`, etc.) run
	// without hook interception to avoid firing the user's hook N times
	// per delegated turn. The top-level invocation of the sub-agent tool
	// itself is still wrapped from the coder's side.
	filteredTools = wrapToolsWithHooks(filteredTools, hookRunner, isSubAgent)

	return filteredTools, nil
}

// effectiveToolNames computes an agent's effective tool list: the
// definition's resolved allow list minus the user's disabled tools,
// applied last so a definition can never widen user policy (#432).
// The returned slice is a fresh copy; the definition's list is shared.
func effectiveToolNames(allowed, disabled []string) []string {
	return slices.DeleteFunc(slices.Clone(allowed), func(name string) bool {
		return slices.Contains(disabled, name)
	})
}

// hasEnabledDispatchAgent reports whether any builtin dispatch-role
// agent is enabled (#432). With none, the dispatch and message tools
// have nothing to act on and stay off the palette.
func hasEnabledDispatchAgent(c *config.Config) bool {
	for _, a := range c.Agents {
		if a.Role == config.AgentRoleDispatch && a.Runtime == config.AgentRuntimeBuiltin && !a.Disabled {
			return true
		}
	}
	return false
}

// mcpAgentTool is the slice of *tools.Tool filterMCPTools reads: the
// tool itself plus the server and tool names it is filtered by.
type mcpAgentTool interface {
	fantasy.AgentTool
	Name() string
	MCP() string
	MCPToolName() string
}

// filterMCPTools keeps the MCP tools an agent's definition allows
// (#432): a nil AllowedMCP means no restriction, an empty one allows
// no MCP tools, and entries allow a whole server or named tools on it
// (the config resolves server:tool pairs on load).
func filterMCPTools[T mcpAgentTool](agent config.Agent, mcpTools []T) []fantasy.AgentTool {
	var filtered []fantasy.AgentTool
	for _, tool := range mcpTools {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filtered = append(filtered, tool)
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, names := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(names) == 0 || slices.Contains(names, tool.MCPToolName()) {
				filtered = append(filtered, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	return filtered
}

// agentModel returns the model an agent's session runs on, given the
// configured slots (#432). The session agent runs on its large slot, so
// an agent whose definition names the small slot gets the small model
// there, and an agent pinned to an explicit provider and model gets the
// pin. The small slot stays the user's small model for auxiliary work
// either way.
func (c *coordinator) agentModel(ctx context.Context, agent config.Agent, large, small Model, isSubAgent bool) (Model, error) {
	switch {
	case agent.ModelRef != nil:
		return c.buildModelFromSelected(ctx, config.SelectedModel{
			Provider: agent.ModelRef.Provider,
			Model:    agent.ModelRef.Model,
		}, isSubAgent)
	case agent.Model == config.SelectedModelTypeSmall:
		return small, nil
	default:
		return large, nil
	}
}

// buildAgentModels resolves the configured large and small model slots.
// agentModel picks which one an agent runs on, or its explicit pin
// (#432).
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	largeModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeLarge]
	if !ok {
		return Model{}, Model{}, errLargeModelNotSelected
	}
	smallModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	large, err := c.buildModelFromSelected(ctx, largeModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}
	small, err := c.buildModelFromSelected(ctx, smallModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}
	return large, small, nil
}

// buildModelFromSelected builds one Model from a selected model: the
// provider is constructed, the model resolved against the provider's
// catalog, and the request-timeout and Hyper-credits wrappers applied.
func (c *coordinator) buildModelFromSelected(ctx context.Context, selected config.SelectedModel, isSubAgent bool) (Model, error) {
	providerCfg, ok := c.cfg.Config().Providers.Get(selected.Provider)
	if !ok {
		return Model{}, fmt.Errorf("%w: %q", errModelProviderNotConfigured, selected.Provider)
	}

	provider, err := c.buildProvider(providerCfg, selected, isSubAgent)
	if err != nil {
		return Model{}, err
	}

	var catwalkModel *catwalk.Model
	for _, m := range providerCfg.Models {
		if m.ID == selected.Model {
			catwalkModel = &m
		}
	}
	if catwalkModel == nil {
		return Model{}, fmt.Errorf("%w: %q in provider %q", errModelNotFound, selected.Model, selected.Provider)
	}

	modelID := selected.Model
	if selected.Provider == openrouter.Name && isExactoSupported(modelID) {
		modelID += ":exacto"
	}

	model, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}

	// Bound each request with the configured timeout so unreachable or hung
	// providers fail instead of blocking a session forever. The wrapper is
	// applied per request, so retries get a fresh budget each attempt.
	model = newRequestTimeoutModel(model, c.cfg.Config().Options.GetRequestTimeout())

	// Hyper completions no longer report the hypercredit balance, so wrap
	// the Hyper models to fetch it from /v1/credits on every request.
	if selected.Provider == hyper.Name {
		model = newHyperCreditsModel(model, c.hyperAPIKey)
	}

	return Model{
		Model:      model,
		CatwalkCfg: *catwalkModel,
		ModelCfg:   selected,
		FlatRate:   providerCfg.FlatRate,
	}, nil
}

// hyperAPIKey resolves the Hyper API key from the live config, so an
// OAuth token refreshed after the models were built is picked up by the
// next credits fetch.
func (c *coordinator) hyperAPIKey() string {
	return config.ResolveHyperAPIKey(c.cfg.Config())
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string) (fantasy.Provider, error) {
	var opts []anthropic.Option

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = apiKey
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		// NOTE: Prevent the SDK from picking up the API key from env.
		os.Setenv("ANTHROPIC_API_KEY", "")
		headers["Authorization"] = "Bearer " + apiKey
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, anthropic.WithHTTPClient(httpClient))
	}
	return anthropic.New(opts...)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, token *oauth.Token) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	var httpClient *http.Client
	if c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	if token != nil {
		// ChatGPT OAuth: requests go through the Codex backend, which
		// expects account headers and rejects some request fields, so
		// they pass through the Codex transport.
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		httpClient.Transport = &openaioauth.Transport{
			Base:  httpClient.Transport,
			Token: token,
		}
	}
	if httpClient != nil {
		opts = append(opts, openai.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenrouterProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug)

	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(isOpenCodeResponsesModel),
		)

	case hyper.Name:
		// Hyper may route requests through a Prism model; capture the
		// router headers so the UI can show which model answered.
		opts = append(
			opts,
			openaicompat.WithLanguageModelOptions(
				openai.WithLanguageModelHeaderFunc(hyper.HeaderFunc),
			),
		)
	}
	if httpClient == nil && c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	if httpClient != nil {
		opts = append(opts, openaicompat.WithHTTPClient(httpClient))
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, azure.WithHTTPClient(httpClient))
	}
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string, providerID string) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}

	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}

	switch providerID {
	case string(catwalk.InferenceProviderBedrockEurope):
		opts = append(opts, bedrock.WithRegion("eu-west-1"))
	default:
		opts = append(opts, bedrock.WithRegion("us-east-1"))
	}

	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string) (fantasy.Provider, error) {
	opts := []google.Option{}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (c *coordinator) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && c.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if isOpenCodeMessagesModel(providerCfg.ID, model.Model) {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
		}
	}

	switch providerCfg.Type {
	case openai.Name:
		// A ChatGPT login is the provider's single credential: every
		// request goes through the Codex backend with the OAuth token.
		token := providerCfg.OAuthToken
		if token != nil {
			baseURL = openaioauth.CodexBaseURL
			apiKey = token.AccessToken
			headers["originator"] = "crush"
			if token.AccountID != "" {
				headers["chatgpt-account-id"] = token.AccountID
			}
		}
		return c.buildOpenaiProvider(baseURL, apiKey, headers, token)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers, providerCfg.ID)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams)
	case openaicompat.Name, hyper.Name:
		switch providerCfg.ID {
		case hyper.Name:
			baseURL = hyper.BaseURL() + "/v1"
			headers["x-crush-id"] = event.GetID()
		case string(catwalk.InferenceProviderZAI):
			if providerCfg.ExtraBody == nil {
				providerCfg.ExtraBody = map[string]any{}
			}
			providerCfg.ExtraBody["tool_stream"] = true
		}
		return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
	default:
		// Known custom providers (litellm, llamacpp, lmstudio, ollama,
		// omlx) are openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}

// BeginAccepted reserves an accept slot for sessionID on the active
// agent and returns the ownership handle. It is the fire-and-forget
// dispatch path's only way to mark a run as accepted-but-not-yet-active
// so a cancel arriving before the run registers in activeRequests is not
// lost.
func (c *coordinator) BeginAccepted(sessionID string) *AcceptedRun {
	return c.currentAgent().BeginAccepted(sessionID)
}

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent().Cancel(sessionID)
}

func (c *coordinator) CancelAll() {
	// Shutdown begins before any run is canceled (#372). Canceling the
	// main agent ends its runs, every run end fires the pending-result
	// flush, and a dispatch can finish while that cancel waits. Raised
	// any later, the flag lets either one start a delivery turn against
	// a coordinator that is canceling everything.
	c.shuttingDown.Store(true)
	c.currentAgent().CancelAll()
	// Quitting stops dispatched agents too (#372): they run on detached
	// contexts, so the agent cancel above never reaches them.
	c.cancelDispatchesForShutdown()
	// Agent builds outlive the runs that start them (#515): every run
	// rebuilds the tool palette, and the sub-agent built there renders
	// its system prompt — git status in the working directory — on a
	// detached goroutine. The work cannot be canceled without killing git
	// mid-write and leaving its index.lock behind, so shutdown waits for
	// it, bounded like the agent and dispatch waits above.
	ctx, cancel := context.WithTimeout(context.Background(), agentBuildShutdownWait)
	defer cancel()
	if err := c.waitAgentBuilds(ctx); err != nil {
		slog.Warn("Agent build still running after shutdown wait", "error", err)
	}
}

// agentBuildShutdownWait bounds CancelAll's wait for in-flight agent
// builds (#515), matching sessionAgent.CancelAll's own bound.
const agentBuildShutdownWait = 5 * time.Second

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent().ClearQueue(sessionID)
}

// ListCronTasks returns the scheduled tasks belonging to sessionID.
func (c *coordinator) ListCronTasks(sessionID string) []scheduler.Task {
	return c.cronStore.List(sessionID)
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent().IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent().IsSessionBusy(sessionID)
}

func (c *coordinator) Model() Model {
	return c.currentAgent().Model()
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	// A ChatGPT login without its model catalog — the fetch at login
	// failed, or the credentials predate it — would leave the models
	// dialog's ChatGPT section empty. Fill it in lazily; the guard makes
	// this a no-op once the catalog exists.
	c.cfg.RefetchOpenAIChatGPTModels(ctx)

	agent, name := c.activeAgent()
	return c.updateAgentModels(ctx, agent, name)
}

// updateAgentModels rebuilds the model and tool configuration for the
// given agent from the current config.
func (c *coordinator) updateAgentModels(ctx context.Context, agent SessionAgent, name string) error {
	agentCfg, ok := c.cfg.Config().Agents[name]
	if !ok {
		return fmt.Errorf("%w: %s", errMainAgentNotFound, name)
	}

	// build the models again so we make sure we get the latest config,
	// keeping the agent's own slot or pin (#432)
	large, small, err := c.buildAgentModels(ctx, false)
	if err != nil {
		return err
	}
	large, err = c.agentModel(ctx, agentCfg, large, small, false)
	if err != nil {
		return err
	}
	agent.SetModels(large, small)

	tools, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return err
	}
	agent.SetTools(tools)
	return nil
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent().QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent().QueuedPromptsList(sessionID)
}

func (c *coordinator) Summarize(ctx context.Context, sessionID string) error {
	agent := c.currentAgent()
	providerCfg, ok := c.cfg.Config().Providers.Get(agent.Model().ModelCfg.Provider)
	if !ok {
		return errModelProviderNotConfigured
	}

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "error", err)
	}

	// Auth failures during summarize flow through fantasy's OnAuthRefresh,
	// the same path used by regular turns.
	return agent.Summarize(ctx, sessionID, getProviderOptions(agent.Model(), providerCfg), c.makeAuthRefreshCallback(providerCfg))
}

// GenerateTitle generates a session title using the current agent.
func (c *coordinator) GenerateTitle(ctx context.Context, sessionID, prompt string) {
	agent := c.currentAgent()
	if agent == nil {
		return
	}
	agent.GenerateTitle(ctx, sessionID, prompt)
}

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig) error {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return c.refreshOAuth2Token(ctx, providerCfg)
}

// retryAfterUnauthorized attempts to refresh credentials after an auth error
// and returns nil if the request should be retried. For OAuth providers whose
// refresh token is revoked, and for Bedrock providers whose AWS SSO session
// has expired, it triggers interactive re-authentication and blocks until the
// user completes it (or the context is cancelled).
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
			// If the refresh token was revoked, trigger interactive
			// re-auth and wait for the user to complete it.
			var exchangeErr *oauth.TokenExchangeError
			if c.notify != nil && errors.As(err, &exchangeErr) && exchangeErr.IsRefreshTokenRevoked() {
				slog.Info("Refresh token revoked, waiting for re-authentication", "provider", providerCfg.ID)
				c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeReAuthenticate,
					ProviderID: providerCfg.ID,
				})
				return c.waitForInteractiveReauth(ctx, providerCfg.ID)
			}
			return err
		}
		return nil
	case providerCfg.AWSAuthRefresh != "":
		return c.refreshAWSCredentials(ctx, providerCfg)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshApiKeyTemplate(ctx, providerCfg)
	default:
		return nil
	}
}

// errNoInteractiveAuth is returned by an OnAuthRefresh callback when a
// provider needs interactive re-authentication but no notifier is available
// to drive it (e.g. headless runs). Returning it surfaces the original auth
// error rather than retrying.
var errNoInteractiveAuth = errors.New("interactive authentication unavailable")

// waitForInteractiveReauth blocks until interactive re-authentication for the
// provider completes (signalled via SignalAuthComplete) or the context is
// cancelled, then rebuilds models so the next attempt picks up fresh
// credentials. Returns nil when the caller should retry.
func (c *coordinator) waitForInteractiveReauth(ctx context.Context, providerID string) error {
	// Use a detached context with a generous timeout so the wait survives
	// agent run cancellation. The user needs time to complete browser-based
	// authentication.
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer waitCancel()
	slog.Info("Blocking on WaitForTokenChange", "provider", providerID)
	if waitErr := c.cfg.WaitForTokenChange(waitCtx, providerID); waitErr != nil {
		slog.Info("WaitForTokenChange returned error", "provider", providerID, "error", waitErr)
		return waitErr
	}
	// If the original context was cancelled during the wait, fantasy's retry
	// would fail immediately, so surface the cancellation instead.
	if ctx.Err() != nil {
		slog.Warn("Original context cancelled during auth wait, cannot retry",
			"provider", providerID, "ctx_err", ctx.Err())
		return ctx.Err()
	}
	// Rebuild models so ModelProvider picks up the fresh credentials.
	if updateErr := c.UpdateModels(waitCtx); updateErr != nil {
		slog.Error("Failed to update models after re-authentication", "error", updateErr)
		return updateErr
	}
	slog.Info("Models updated, returning nil to retry", "provider", providerID)
	return nil
}

// isUnauthorized reports whether err is an HTTP 401 from a provider.
func isUnauthorized(err error) bool {
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

// makeAuthRefreshCallback returns an OnAuthRefresh callback for fantasy that
// delegates to the coordinator's existing credential refresh logic. Returns
// nil if no refresh mechanism is configured for the provider.
func (c *coordinator) makeAuthRefreshCallback(providerCfg config.ProviderConfig) func(context.Context, *fantasy.ProviderError) error {
	if providerCfg.OAuthToken == nil &&
		!strings.Contains(providerCfg.APIKeyTemplate, "$") &&
		providerCfg.AWSAuthRefresh == "" {
		return nil
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		return c.retryAfterUnauthorized(ctx, providerCfg)
	}
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	if err := c.cfg.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	providerCfg.APIKey = newAPIKey
	c.cfg.Config().Providers.Set(providerCfg.ID, providerCfg)

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
	// AgentName and AgentDescription describe the card of the agent
	// served for the turn when it runs through the A2A runtime (#392);
	// empty falls back to a generic sub-agent identity.
	AgentName        string
	AgentDescription string
}

// callTopK returns topK for use on fantasy.Call.TopK, suppressing it for
// known custom providers: getProviderOptions already carries top_k for
// them via extra_body, and passing it here too makes Fantasy emit a
// spurious "top_k unsupported" warning for every turn.
func callTopK(providerCfg config.ProviderConfig, topK *int64) *int64 {
	if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
		return nil
	}
	return topK
}

// subAgentTurn is one prepared sub-agent invocation (#392): the session
// it runs against and the call template its turn runs with, with the
// model and provider shaping already resolved. SessionID, Prompt and
// RunID are per execution path — set directly for the in-process
// runner, by the A2A executor from the binding and the served task for
// a turn that runs through the runtime.
type subAgentTurn struct {
	sessionID string
	call      SessionAgentCall
	provider  string
}

// prepareSubAgent does the session and call shaping every sub-agent
// execution path shares (#392): wait for the agent's setup, create its
// task session, run the caller's setup hook, and resolve the model and
// provider options the turn runs with.
func (c *coordinator) prepareSubAgent(ctx context.Context, params subAgentParams) (*subAgentTurn, error) {
	// A sub-agent built by buildAgent is handed to its tool before its
	// system prompt and tool list land, so wait for its own setup here.
	// Previously the only wait was coordinator.run's, which covered whichever
	// builds happened to be registered when a run started — so a sub-agent
	// could take a turn with an empty prompt, and a sub-agent build error
	// surfaced on an unrelated later run instead of on the call that needed
	// it.
	if err := params.Agent.WaitReady(); err != nil {
		return nil, fmt.Errorf("sub-agent setup failed: %w", err)
	}

	// Create sub-session
	agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
	session, err := c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(session.ID)
	}

	// Get model configuration
	model := params.Agent.Model()
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	return &subAgentTurn{
		sessionID: session.ID,
		provider:  model.ModelCfg.Provider,
		call: SessionAgentCall{
			// Inherit the parent turn's UI width hint: the sub-agent's
			// PrepareStep stamps call.ContentWidth over the tool-call
			// context unconditionally, so leaving this zero would clobber
			// the value the parent already carries.
			ContentWidth:     tools.GetContentWidthFromContext(ctx),
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  getProviderOptions(model, providerCfg),
			Temperature:      model.ModelCfg.Temperature,
			TopP:             model.ModelCfg.TopP,
			TopK:             callTopK(providerCfg, model.ModelCfg.TopK),
			FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
			PresencePenalty:  model.ModelCfg.PresencePenalty,
			NonInteractive:   true,
			OnAuthRefresh:    c.makeAuthRefreshCallback(providerCfg),
		},
	}, nil
}

// runSubAgent runs a sub-agent in-process and handles session management
// and cost accumulation. It creates a sub-session, runs the agent with the
// given prompt, and propagates the cost to the parent session.
//
// This is the transitional direct path (#392): the agent-tool entry point
// still runs here; every entry point that has switched serves its turn on
// the A2A host instead (runSubAgentOverA2A), and this path is deleted
// once the last one moves.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	turn, err := c.prepareSubAgent(ctx, params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	call := turn.call
	call.SessionID = turn.sessionID
	call.Prompt = params.Prompt
	result, err := params.Agent.Run(ctx, call)
	return c.finishSubAgent(ctx, turn, params.SessionID, subAgentOutput(result), err)
}

// finishSubAgent maps a finished sub-agent turn onto the tool response
// its tool call expects (#392): a provider failure becomes an error
// response carrying the reason, the child session's cost is propagated
// to the parent on a best-effort basis, and empty output is its own
// error response. Shared by every execution path, so the visible
// contract cannot drift between them.
func (c *coordinator) finishSubAgent(ctx context.Context, turn *subAgentTurn, parentSessionID, output string, runErr error) (fantasy.ToolResponse, error) {
	// Notify only if still unauthorized after retry. AWS SSO is handled
	// transparently inside OnAuthRefresh, so it needs no post-run notice.
	if runErr != nil && isUnauthorized(runErr) && c.notify != nil && turn.provider == hyper.Name {
		c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: turn.provider,
		})
	}
	if runErr != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", runErr)), nil
	}

	// Update parent session cost on a best-effort basis. A failure here must
	// not discard the sub-agent output that was already produced.
	if err := c.updateParentSessionCost(ctx, turn.sessionID, parentSessionID); err != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", turn.sessionID,
			"parent_session", parentSessionID,
			"error", err,
		)
	}

	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output."), nil
	}
	return fantasy.NewTextResponse(output), nil
}

// runSubAgentOverA2A runs a sub-agent turn through the A2A runtime
// (#392): the agent is served on the process host for the turn's
// lifetime — one card, one context (the sub-agent's task session, per
// #350), one task — and the coordinator drives it with the A2A client,
// consuming the stream to its terminal state. There is no direct-run
// fallback: a coordinator with no host fails the turn. Transcripts stay
// in the session store; only execution moves to the runtime.
func (c *coordinator) runSubAgentOverA2A(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	turn, err := c.prepareSubAgent(ctx, params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}

	host := c.a2aHost()
	if host == nil {
		return c.finishSubAgent(ctx, turn, params.SessionID, "", errors.New("sub-agent unavailable: no A2A host is wired"))
	}

	// Serve the turn: the agent answers at its own route for exactly the
	// turn's lifetime, its context bound to the task session. Diff, todos
	// and usage stay unset — a sub-agent turn produces text, not a
	// workspace artifact, and carries no workspace toolchain.
	endpoint, card, stop, err := host.StartDispatchServer(ctx, DispatchServerParams{
		DispatchID:  subAgentRouteID(turn.sessionID),
		SessionID:   turn.sessionID,
		Runner:      params.Agent,
		Name:        params.AgentName,
		Description: params.AgentDescription,
		Call:        turn.call,
	})
	if err != nil {
		return c.finishSubAgent(ctx, turn, params.SessionID, "", fmt.Errorf("serve sub-agent: %w", err))
	}
	defer stop()

	if TraceparentFromContext(ctx) == "" {
		if tp, terr := NewTraceparent(); terr == nil {
			ctx = WithTraceparent(ctx, tp)
		}
	}
	traceID := TraceIDFromTraceparent(TraceparentFromContext(ctx))
	slog.Debug("Sub-agent A2A turn starting", "session_id", turn.sessionID, "parent_session_id", params.SessionID, "context_id", turn.sessionID, "trace_id", traceID)

	// The context is the task session (#350): the served executor resolves
	// it to this turn's runner and session, and each turn is a fresh task.
	var taskID string
	outcome, err := host.StreamDispatch(ctx, DispatchTransportParams{
		Endpoint:  endpoint,
		Card:      card,
		Prompt:    params.Prompt,
		ContextID: turn.sessionID,
		OnTask: func(id string) {
			taskID = id
			slog.Debug("Sub-agent A2A task started", "session_id", turn.sessionID, "task_id", id, "trace_id", traceID)
		},
	})
	if err != nil {
		outcome = c.recoverSubAgentOutcome(ctx, host, params, turn.sessionID, endpoint, card, taskID, err)
	}
	output, runErr := subAgentOutcomeFromTransport(outcome)
	return c.finishSubAgent(ctx, turn, params.SessionID, output, runErr)
}

// recoverSubAgentOutcome ends a sub-agent turn whose stream broke before
// a terminal state (#392). The served run is orphaned — the executor
// runs its turn on a detached context — so the parent's cancel must
// reach it through tasks/cancel (#348's protocol path), never by going
// out of scope. The canceled-or-failed task is then read back so the
// caller maps the run's real terminal state; if even that fails, the
// stream error stands as a failure.
func (c *coordinator) recoverSubAgentOutcome(ctx context.Context, host DispatchHost, params subAgentParams, runSessionID string, endpoint string, card any, taskID string, streamErr error) DispatchTransportOutcome {
	slog.Warn("Sub-agent A2A stream failed; recovering the served run", "session_id", runSessionID, "parent_session_id", params.SessionID, "task_id", taskID, "error", streamErr)

	if taskID != "" {
		recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if canceler, ok := host.(DispatchCanceler); ok {
			if cerr := canceler.CancelDispatch(recoverCtx, DispatchCancelParams{
				Endpoint: endpoint,
				Card:     card,
				TaskID:   taskID,
				Reason:   "parent turn canceled",
			}); cerr != nil {
				// The protocol cancel is best-effort here; the direct
				// cancel covers a host that cannot deliver it. The run
				// lives on its task session, so the cancel names that.
				slog.Warn("Sub-agent tasks/cancel failed; canceling the runner directly", "session_id", runSessionID, "task_id", taskID, "error", cerr)
				params.Agent.Cancel(runSessionID)
			}
			if st, gerr := waitSubAgentTerminal(recoverCtx, host, endpoint, card, taskID); gerr == nil {
				return DispatchTransportOutcome{Status: st.Status, Text: st.Text}
			}
		} else {
			// No canceler on the seam (a bare test fake): the direct
			// cancel is the only way the run ends.
			params.Agent.Cancel(runSessionID)
		}
	} else {
		// The stream died before any task was named: the direct cancel
		// is a no-op if the run never started, and reaches it if it did.
		params.Agent.Cancel(runSessionID)
	}
	return DispatchTransportOutcome{Status: transportStatusFailed, Text: streamErr.Error()}
}

// waitSubAgentTerminal polls tasks/get until the task leaves working
// state or the context ends (#392): tasks/cancel is asynchronous — the
// SDK resolves it when the run actually ends — so the terminal state
// lands a beat after the cancel is accepted.
func waitSubAgentTerminal(ctx context.Context, host DispatchHost, endpoint string, card any, taskID string) (DispatchTaskStatus, error) {
	getter, ok := host.(interface {
		GetDispatchTask(ctx context.Context, params GetDispatchTaskParams) (DispatchTaskStatus, error)
	})
	if !ok {
		return DispatchTaskStatus{}, errors.New("host does not expose tasks/get")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := getter.GetDispatchTask(ctx, GetDispatchTaskParams{Endpoint: endpoint, Card: card, TaskID: taskID})
		if err == nil && st.Status != "" && st.Status != "working" {
			return st, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return st, errors.New("timed out waiting for the sub-agent task to end")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// subAgentOutcomeFromTransport maps a served sub-agent turn's terminal
// outcome onto the (output, error) the shared finish expects (#392):
// completed carries the terminal status message as the turn's output;
// every other terminal state is a run error whose text is the served
// side's reason.
func subAgentOutcomeFromTransport(outcome DispatchTransportOutcome) (string, error) {
	switch outcome.Status {
	case transportStatusCompleted:
		return outcome.Text, nil
	case transportStatusCanceled:
		if outcome.Text == "" {
			return "", errors.New("sub-agent run was canceled")
		}
		return "", fmt.Errorf("sub-agent run was canceled: %s", outcome.Text)
	case transportStatusFailed:
		if outcome.Text == "" {
			return "", errors.New("sub-agent run failed")
		}
		return "", errors.New(outcome.Text)
	case "":
		return "", errors.New("sub-agent run ended without a terminal state")
	default:
		if outcome.Text == "" {
			return "", fmt.Errorf("sub-agent run ended with status %q", outcome.Status)
		}
		return "", fmt.Errorf("%s: %s", outcome.Status, outcome.Text)
	}
}

// subAgentRouteID is the route a sub-agent turn is served under
// (#392): unique per turn's task session, namespaced away from dispatch
// entry ids.
func subAgentRouteID(sessionID string) string {
	return "agent-" + sessionID
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}

// updateParentSessionCost accumulates the cost from a child session to its
// parent session. The parent row is read only to confirm it exists. The
// cost is applied with a single atomic increment, so the parent's own
// full-row saves during its turn cannot race the write (#389).
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	childSession, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	if _, err := c.sessions.Get(ctx, parentSessionID); err != nil {
		return fmt.Errorf("get parent session: %w", err)
	}

	if err := c.sessions.AddCost(ctx, parentSessionID, childSession.Cost); err != nil {
		return fmt.Errorf("add cost to parent session: %w", err)
	}

	return nil
}

// discoverSkills is a thin fallback wrapper used only when no
// skills.Manager has been threaded through to the coordinator. All
// production call sites (backend.CreateWorkspace, setupLocalWorkspace)
// run discovery in advance and pass the results via the manager;
// reaching this path means a caller bypassed both. It deliberately does
// NOT publish to the package-level broker — there are no subscribers in
// that case, so doing so would be misleading without delivering the
// snapshot anywhere useful.
func discoverSkills(cfg *config.ConfigStore) (allSkills, activeSkills []*skills.Skill) {
	opts := cfg.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	var resolver func(string) (string, error)
	if r := cfg.Resolver(); r != nil {
		resolver = r.ResolveValue
	}
	allSkills, activeSkills, states := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		WorkingDir:     cfg.WorkingDir(),
		Resolver:       resolver,
	})
	logDiscoveryStats(states, paths, allSkills, activeSkills, disabled)
	return allSkills, activeSkills
}

// logTurnSkillUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func logTurnSkillUsage(
	sessionID string,
	prompt string,
	activeSkills []*skills.Skill,
	tracker *skills.Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start. Builtin vs user counts are derived from the
// SkillState.Path — builtin states use the "builtin/" embed prefix.
func logDiscoveryStats(
	states []*skills.SkillState,
	userPaths []string,
	allSkills, activeSkills []*skills.Skill,
	disabled []string,
) {
	var builtinOK, builtinErr, userOK, userErr int
	for _, s := range states {
		isBuiltin := strings.HasPrefix(s.Path, "builtin/")
		switch {
		case isBuiltin && s.State == skills.StateNormal:
			builtinOK++
		case isBuiltin && s.State == skills.StateError:
			builtinErr++
		case !isBuiltin && s.State == skills.StateNormal:
			userOK++
		case !isBuiltin && s.State == skills.StateError:
			userErr++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := skills.ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", builtinOK,
		"builtin_errors", builtinErr,
		"user_ok", userOK,
		"user_errors", userErr,
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", skills.ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}
