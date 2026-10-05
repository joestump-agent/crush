package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// killRecorder is a thread-safe capture of the wander-kill observer's
// calls, standing in for the coordinator's kill state in unit tests.
type killRecorder struct {
	mu     sync.Mutex
	killID string
	reason string
	calls  int
}

func (k *killRecorder) observe(sessionID, reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls++
	k.killID = sessionID
	k.reason = reason
}

func (k *killRecorder) recorded() (int, string, string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls, k.killID, k.reason
}

// TestTodoKill_FiresAfterIgnoredNudges pins the ladder's kill rung
// (#316): with kill-after-one-nudge, the window after the first nudge
// completes with still no todos activity, the observer fires exactly
// once with the ignored-nudges reason, and the run cancels itself.
func TestTodoKill_FiresAfterIgnoredNudges(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	var killed killRecorder
	sa := newTodoTestAgentOpts(t, env, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 1,
	}, []todoAgentOpt{withTodoKill(killed.observe)}, probeTool())

	// The run is expected to end canceled: the kill is what ends it.
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "do the work",
	})
	require.ErrorIs(t, err, context.Canceled, "the killed run must end canceled")

	calls, killID, reason := killed.recorded()
	require.Equal(t, 1, calls, "the kill must fire exactly once")
	assert.Equal(t, sess.ID, killID)
	assert.Equal(t, dispatch.ReasonIgnoredNudges, reason)
}

// TestTodoKill_KillAfterNudgesHonored pins the kill threshold above the
// old nudge cap (#401): with kill-after-three and a wired kill, the run
// gets the nudge, an escalating nudge, and a second escalating nudge,
// then the kill on the next window.
func TestTodoKill_KillAfterNudgesHonored(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	var killed killRecorder
	sa := newTodoTestAgentOpts(t, env, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 3,
	}, []todoAgentOpt{withTodoKill(killed.observe)}, probeTool())

	// The run is expected to end canceled: the kill is what ends it.
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "do the work",
	})
	require.ErrorIs(t, err, context.Canceled, "the killed run must end canceled")

	calls, killID, reason := killed.recorded()
	require.Equal(t, 1, calls, "the kill must fire exactly once")
	assert.Equal(t, sess.ID, killID)
	assert.Equal(t, dispatch.ReasonIgnoredNudges, reason)

	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage), 1,
		"the first nudge must be injected")
	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoEscalatingNudgeMessage), 2,
		"two escalating nudges must precede the kill")
}

// TestTodoKill_AboveCapWithoutObserver pins the no-kill contract (#401):
// a kill threshold above the nudge cap means nothing without a wired kill
// observer; the ladder still tops out at the nudge and the escalating
// nudge, and the run finishes normally.
func TestTodoKill_AboveCapWithoutObserver(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 5,
	}, probeTool())

	sess := runWithScript(t, env, sa, model)

	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage), 1,
		"the first nudge must still be injected")
	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoEscalatingNudgeMessage), 1,
		"the ladder must cap at the escalating nudge without a kill hook")
}

// TestTodoKill_NotBeforeNudges pins the escalation ordering: with the
// default kill-after-two, a run that gets both nudges and then finishes
// is never killed. The kill needs the window after the last allowed
// nudge, not the nudge itself.
func TestTodoKill_NotBeforeNudges(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	var killed killRecorder
	sa := newTodoTestAgentOpts(t, env, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 2,
	}, []todoAgentOpt{withTodoKill(killed.observe)}, probeTool())

	runWithScript(t, env, sa, model)

	calls, _, reason := killed.recorded()
	require.Zero(t, calls, "two ignored nudges is the ladder's cap; the kill needs a third window")
	assert.Empty(t, reason)
}

// TestTodoKill_DisabledWhenZero pins the kill's off switch: a zero
// threshold leaves the ladder capped at nudging, however long the run
// ignores it.
func TestTodoKill_DisabledWhenZero(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	var killed killRecorder
	sa := newTodoTestAgentOpts(t, env, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 0,
	}, []todoAgentOpt{withTodoKill(killed.observe)}, probeTool())

	runWithScript(t, env, sa, model)

	calls, _, _ := killed.recorded()
	require.Zero(t, calls, "kill_after_nudges=0 must disable the kill")
}

// TestTodoKill_NoTodosToolNeverKilled pins that a dispatched agent
// without the todos tool is never killed for ignoring nudges it could not
// act on: the script that TestTodoKill_FiresAfterIgnoredNudges kills runs
// to completion.
func TestTodoKill_NoTodosToolNeverKilled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	var killed killRecorder
	sa := newTodoTestAgentOpts(t, env, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 1,
	}, []todoAgentOpt{withTodoKill(killed.observe), withoutTodosTool()}, probeTool())

	runWithScript(t, env, sa, model)

	calls, _, _ := killed.recorded()
	require.Zero(t, calls, "an agent without the todos tool must never be killed by the ladder")
}

// TestTodoKill_NonDispatchedAgentNeverCanceled pins the kill's scope
// (#393): an agent built without a TodoKill observer (the main coder,
// plan mode, agent-tool sub-agents, crush run) tops out at the escalating
// nudge under the production defaults, however many tool calls it makes,
// and its run finishes normally. Inputs vary so loop detection stays out
// of the way.
func TestTodoKill_NonDispatchedAgentNeverCanceled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	var steps []scriptedStep
	for i := range 14 {
		steps = append(steps, scriptedStep{toolCalls: []scriptedToolCall{{
			name:  "probe",
			input: fmt.Sprintf(`{"n":%d}`, i),
		}}})
	}
	steps = append(steps, scriptedStep{text: "done"})
	model := &scriptedModel{steps: steps}
	sa := newTodoTestAgent(t, env, model, config.ResolveTodoEnforcement(nil, nil), probeTool())

	sess := runWithScript(t, env, sa, model)

	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoNudgeMessage), 1,
		"the first nudge must still be injected")
	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoEscalatingNudgeMessage), 1,
		"the escalating nudge must still be injected, and nothing past it")
}

// TestTodoKill_NonDispatchedMutatingNeverCanceled is the mutating-tool
// variant: bash trips the ladder on every window, so with the default
// kill-after-two a dispatched agent would be killed on the third call.
// Without an observer the run must still finish.
func TestTodoKill_NonDispatchedMutatingNeverCanceled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "bash", input: `{"n":1}`}}},
		{toolCalls: []scriptedToolCall{{name: "bash", input: `{"n":2}`}}},
		{toolCalls: []scriptedToolCall{{name: "bash", input: `{"n":3}`}}},
		{text: "done"},
	}}
	sa := newTodoTestAgent(t, env, model, config.ResolveTodoEnforcement(nil, nil), mutatingTool("bash"))

	sess := runWithScript(t, env, sa, model)

	require.Len(t, nudgeMessages(t, env, model, sess.ID, todoEscalatingNudgeMessage), 1,
		"the ladder must still escalate before topping out")
}

// blockingScriptedModel holds every Stream call until released (or the
// context ends), which is how the watchdog tests hold a dispatched run
// open while a timer kills it. On context end it returns the
// cancellation, which the fantasy retry treats as an abort.
type blockingScriptedModel struct {
	*scriptedModel
	hold chan struct{}
}

func (m *blockingScriptedModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// The title goroutine is detached from the run's cancel by design
	// (the GenerateTitle wiring in agent.go), so holding its stream
	// would keep a killed run's bubble from ever draining. Titles are
	// served immediately; only the dispatched turn holds.
	if !isTitleCall(call) {
		select {
		case <-m.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.scriptedModel.Stream(ctx, call)
}

// wanderKillFixture wires everything one runDispatch-level kill test
// needs: a git-rooted coordinator, a provisioned workspace with a
// running dispatch entry and task session, the dispatched scripted
// agent, the kill state, and a fake main agent to receive delivery.
type wanderKillFixture struct {
	env        fakeEnv
	c          *coordinator
	ws         *dispatch.Workspace
	entry      dispatch.Entry
	taskSess   session.Session
	parentSess session.Session
	// runModel is the model the dispatchRun carries; blocking fixtures
	// point it at the blocking wrapper.
	runModel fantasy.LanguageModel
	kill     *dispatchKill
	// killSettings ride the dispatchRun, the way the tool resolves them.
	killSettings config.TodoEnforcementSettings

	dispatched      SessionAgent
	killHookInvoked bool
	main            *fakeMainAgent
	// runCtx and runCancel mirror the dispatch root (#371) production
	// wires: buildRun hands the cancel to the run, and the watchdog's
	// kill fires it. Nil when a test drives the run without a root.
	runCtx    context.Context
	runCancel context.CancelFunc
}

func newWanderKillFixture(t *testing.T, model *scriptedModel, dispatchedSettings config.TodoEnforcementSettings) *wanderKillFixture {
	t.Helper()
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)

	// The delivery run resolves the main agent's models from the config
	// store; register the offline test provider the fake main agent
	// reports, mirroring TestDeliverDispatchResultToParentSession.
	const providerID = "test-provider"
	c.cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
	c.cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	c.cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)

	f := &wanderKillFixture{
		env:          env,
		c:            c,
		runModel:     model,
		kill:         &dispatchKill{},
		killSettings: dispatchedSettings,
	}
	f.buildDispatched(t, model, dispatchedSettings, nil)

	ws, err := dispatch.NewWorkspace(env.workingDir, filepath.Join(env.workingDir, "worktrees"))
	require.NoError(t, err)
	f.ws = ws

	entry, err := ws.Provision(t.Context(), dispatch.ProvisionOptions{})
	require.NoError(t, err)
	f.entry = entry

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	f.parentSess = parent
	task, err := env.sessions.CreateTaskSession(t.Context(), "wander-kill-task", parent.ID, "Dispatched Agent")
	require.NoError(t, err)
	f.taskSess = task

	ws.SetSession(entry.ID, task.ID)
	ws.SetStatus(entry.ID, dispatch.StatusRunning)
	if _, ok := ws.AssignHandle(entry.ID, "tester", "dispatch tester"); !ok {
		t.Fatal("assign dispatch handle")
	}

	f.main = &fakeMainAgent{model: dispatchTestModel()}
	c.mainAgent = f.main
	c.mainAgentName = config.AgentCoder
	c.agents = map[string]SessionAgent{config.AgentCoder: f.main}
	return f
}

// buildDispatched constructs the dispatched agent the way the real
// builder would (newSessionAgent with the kill observer recording into
// the fixture's kill state) and installs it as the coordinator's
// dispatchAgentBuilder. extra feeds extra tools.
func (f *wanderKillFixture) buildDispatched(t *testing.T, model fantasy.LanguageModel, settings config.TodoEnforcementSettings, extra []fantasy.AgentTool) {
	t.Helper()
	opts := []todoAgentOpt{
		withTodoKill(func(sessionID, reason string) {
			f.killHookInvoked = true
			f.kill.kill(reason)
		}),
		withLoopStop(func(sessionID string) {
			f.kill.kill(dispatch.ReasonToolLoop)
		}),
	}
	dispatched := newTodoTestAgentOpts(t, f.env, model, settings, opts, extra...)
	f.dispatched = dispatched
	f.c.dispatchAgentBuilder = func(ctx context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		return &dispatchedAgent{
			agent:       dispatched,
			model:       Model{Model: model, CatwalkCfg: catwalkModelCfg()},
			providerCfg: config.ProviderConfig{},
		}, nil
	}
}

// buildRun assembles the dispatchRun the fixture drives, so the
// transport wiring can build the same served call the run carries.
func (f *wanderKillFixture) buildRun() dispatchRun {
	return dispatchRun{
		workspace:       f.ws,
		entry:           f.entry,
		agent:           f.dispatched,
		model:           Model{Model: f.runModel, CatwalkCfg: catwalkModelCfg()},
		prompt:          "do the work",
		sessionID:       f.taskSess.ID,
		parentSessionID: f.parentSess.ID,
		kill:            f.kill,
		killSettings:    f.killSettings,
		cancel:          f.runCancel,
	}
}

// armRunRoot wires the fixture's run the way the dispatch tool does
// (#371): the run rides a root context whose cancel the watchdog's kill
// must fire. Call it inside a synctest bubble so the root's goroutines
// belong to the bubble.
func (f *wanderKillFixture) armRunRoot() {
	f.runCtx, f.runCancel = context.WithCancel(context.WithoutCancel(context.Background()))
}

// runDispatchSync drives the fixture's dispatch to completion.
func (f *wanderKillFixture) runDispatchSync(t *testing.T) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	if f.runCtx != nil {
		ctx = f.runCtx
	}
	f.c.runDispatch(ctx, f.buildRun())
}

// wireTransport serves the fixture's dispatch through rt: the real
// startDispatchServer path records the runner and call on the transport
// and stamps the endpoint and card on the registry entry, so the run
// takes the served transport path instead of the direct one.
func (f *wanderKillFixture) wireTransport(t *testing.T, rt *runnerTransport) {
	t.Helper()
	run := f.buildRun()
	f.c.SetDispatchServerStarter(rt)
	stop := f.c.startDispatchServer(context.Background(), f.ws, f.entry.ID, f.taskSess.ID, "tester", "dispatch tester", run.agent, nil, run.call(f.c), run.killSettings.InactivityTimeout, run.kill.current)
	t.Cleanup(func() {
		if stop != nil {
			stop()
		}
		f.c.stopDispatchServer(f.ws, f.entry.ID, nil)
	})
	entry, ok := f.ws.Get(f.entry.ID)
	require.True(t, ok)
	require.NotEmpty(t, entry.Endpoint, "the dispatch must be served for the transport to drive it")
}

// requireKilled asserts the registry-level kill record and the preserved
// workspace.
func (f *wanderKillFixture) requireKilled(t *testing.T, reason string) dispatch.Entry {
	t.Helper()
	entry, ok := f.ws.Get(f.entry.ID)
	require.True(t, ok, "a killed dispatch's registry entry must survive (cleanup defers to the parent)")
	require.Equal(t, dispatch.StatusKilled, entry.Status)
	require.NotNil(t, entry.Result)
	assert.Equal(t, reason, entry.Result.KilledReason)
	assert.Equal(t, dispatch.StatusKilled, entry.Result.Status)
	assert.Equal(t, "tester", entry.Result.Handle, "the killed payload carries the handle the parent re-dispatches against")
	assert.False(t, entry.FinishedAt.IsZero())
	require.DirExists(t, entry.Path, "a kill never auto-discards work-in-progress")
	return entry
}

// requireParentKilledDelivery waits for the structured failure to reach
// the fake main agent and asserts its shape.
func (f *wanderKillFixture) requireParentKilledDelivery(t *testing.T) SessionAgentCall {
	t.Helper()
	require.Eventually(t, func() bool {
		return f.main.runCount() == 1
	}, 10*time.Second, 50*time.Millisecond, "the parent must receive the killed dispatch's terminal message")
	run := f.main.lastRun()
	require.Equal(t, f.parentSess.ID, run.SessionID)
	require.True(t, run.HiddenUserMessage, "the terminal payload stays out of the chat UI")
	require.Contains(t, run.Prompt, "was killed")
	require.Contains(t, run.Prompt, "preserved", "the payload must promise workspace preservation")
	require.Contains(t, run.Prompt, "re-dispatch", "the payload must pose the re-dispatch decision")
	require.Contains(t, run.Prompt, `"killed_reason": "ignored nudges"`)
	require.Contains(t, run.Prompt, `"status": "killed"`)
	return run
}

// TestWanderKill_IgnoredNudgesEndToEnd is the full-path kill test: a
// dispatched run that ignores its nudges past the threshold is canceled,
// the registry records killed with the reason, the parent receives the
// structured failure, and the workspace is preserved for salvage.
func TestWanderKill_IgnoredNudgesEndToEnd(t *testing.T) {
	t.Parallel()
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
		{text: "done"},
	}}
	f := newWanderKillFixture(t, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 1,
	})

	f.runDispatchSync(t)

	f.requireKilled(t, dispatch.ReasonIgnoredNudges)
	assert.True(t, f.killHookInvoked, "the enforcement hook must have fired")
	f.requireParentKilledDelivery(t)
}

// TestWanderKill_HardTimeout pins the watchdog's hard timeout: a run
// held open past the timeout is canceled and recorded as killed with the
// hard-timeout reason, workspace intact. It runs under synctest on
// production-scale settings (#430): the bubble's fake clock makes a
// 30-minute timeout free, and a lost kill deadlocks the bubble instead
// of hiding behind a real-clock cap.
func TestWanderKill_HardTimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		model := &scriptedModel{steps: []scriptedStep{
			{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
			{text: "done"},
		}}
		blocked := &blockingScriptedModel{scriptedModel: model, hold: make(chan struct{})}
		settings := config.TodoEnforcementSettings{
			Enabled:     false,
			HardTimeout: 30 * time.Minute,
		}
		f := newWanderKillFixture(t, model, settings)
		f.runModel = blocked
		f.buildDispatched(t, blocked, settings, nil)
		f.armRunRoot()

		done := make(chan struct{})
		go func() {
			defer close(done)
			f.runDispatchSync(t)
		}()

		<-done

		f.requireKilled(t, dispatch.ReasonHardTimeout)
		assert.False(t, f.killHookInvoked, "the watchdog kills, not the ladder")
	})
}

// TestWanderKill_StalledTodos pins the stall window: a run whose todo
// list goes untouched for the configured window is killed with the
// stalled-todos reason even though it keeps tool-calling. It runs under
// synctest on production-scale settings (#430): the bubble's fake clock
// advances a 10-minute window without wall time, and a lost kill
// deadlocks the bubble instead of hiding behind a real-clock cap.
func TestWanderKill_StalledTodos(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		model := &scriptedModel{steps: []scriptedStep{
			{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
			{text: "done"},
		}}
		blocked := &blockingScriptedModel{scriptedModel: model, hold: make(chan struct{})}
		settings := config.TodoEnforcementSettings{
			Enabled:     false,
			StallWindow: 10 * time.Minute,
		}
		f := newWanderKillFixture(t, model, settings)
		f.runModel = blocked
		f.buildDispatched(t, blocked, settings, nil)
		f.armRunRoot()

		// The dispatched session starts with todos, then never updates
		// them: the definition of a stall.
		sess, err := f.env.sessions.Get(t.Context(), f.taskSess.ID)
		require.NoError(t, err)
		sess.Todos = []session.Todo{{Content: "stale plan", Status: session.TodoStatusInProgress, ActiveForm: "Stalling"}}
		_, err = f.env.sessions.Save(t.Context(), sess)
		require.NoError(t, err)

		done := make(chan struct{})
		go func() {
			defer close(done)
			f.runDispatchSync(t)
		}()

		<-done

		f.requireKilled(t, dispatch.ReasonStalledTodos)
	})
}

// lateStartAgent delays every Run past a fixed duration: the real agent
// registers its session only after the wrapper's delay elapses. Under
// synctest's fake clock the watchdog's hard timeout can be made to fire
// strictly inside that delay (while the dispatched agent's Cancel is
// still a no-op for the unregistered session), which is exactly the
// kill the old wiring lost (#430).
type lateStartAgent struct {
	*sessionAgent
	delay time.Duration
}

func (a *lateStartAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	timer := time.NewTimer(a.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.sessionAgent.Run(ctx, call)
}

// TestWanderKill_KillsBeforeRunRegisters pins the lost early kill
// (#430): the watchdog fires before the dispatched agent's Run has
// registered the session, so agent.Cancel alone cannot end it. The
// dispatch must still end killed with the hard-timeout reason (only
// the run context's cancel can reach a run that has not started).
func TestWanderKill_KillsBeforeRunRegisters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		model := &scriptedModel{steps: []scriptedStep{
			{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
			{text: "done"},
		}}
		blocked := &blockingScriptedModel{scriptedModel: model, hold: make(chan struct{})}
		settings := config.TodoEnforcementSettings{
			Enabled:     false,
			HardTimeout: 30 * time.Minute,
		}
		f := newWanderKillFixture(t, model, settings)
		f.runModel = blocked
		f.buildDispatched(t, blocked, settings, nil)
		f.dispatched = &lateStartAgent{sessionAgent: f.dispatched.(*sessionAgent), delay: 45 * time.Minute}
		f.armRunRoot()

		done := make(chan struct{})
		go func() {
			defer close(done)
			f.runDispatchSync(t)
		}()

		<-done

		f.requireKilled(t, dispatch.ReasonHardTimeout)
	})
}

// TestWanderKill_ToolLoop pins the tool-loop kill reason: a run that
// ends on the loop-detection stop condition is recorded as killed with
// the tool-loop reason even though it was not canceled.
func TestWanderKill_ToolLoop(t *testing.T) {
	t.Parallel()
	var steps []scriptedStep
	for range 12 {
		steps = append(steps, scriptedStep{toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}})
	}
	steps = append(steps, scriptedStep{text: "done"})
	model := &scriptedModel{steps: steps}
	f := newWanderKillFixture(t, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  50,
		KillAfterNudges: 0,
	})

	f.runDispatchSync(t)

	f.requireKilled(t, dispatch.ReasonToolLoop)
	assert.False(t, f.killHookInvoked, "the tool loop is attributed from the result, not the ladder")
}

// TestWanderKill_CompletionWinsOverLateKill pins the assembly's guard
// against the watchdog racing a natural completion: a killed reason
// recorded after the run finished normally must not turn a healthy
// completion into a kill.
func TestWanderKill_CompletionWinsOverLateKill(t *testing.T) {
	t.Parallel()
	model := &scriptedModel{steps: []scriptedStep{{text: "all done"}}}
	f := newWanderKillFixture(t, model, config.TodoEnforcementSettings{})
	f.kill.kill(dispatch.ReasonHardTimeout)

	run := dispatchRun{
		workspace:    f.ws,
		entry:        f.entry,
		model:        Model{Model: f.runModel, CatwalkCfg: catwalkModelCfg()},
		sessionID:    f.taskSess.ID,
		kill:         f.kill,
		killSettings: config.TodoEnforcementSettings{},
	}
	terminal := f.c.assembleTerminalDispatchResult(t.Context(), run, dispatchNaturalOutcome{
		completed: true,
		findings:  "all done",
		diff: func(ctx context.Context) (string, error) {
			return run.workspace.Diff(ctx, run.entry.ID)
		},
	})

	assert.Equal(t, dispatch.StatusCompleted, terminal.Status)
	assert.Empty(t, terminal.KilledReason)
}

// TestWanderKill_KilledCarriesLastStateAndDiff pins the structured
// failure's content: the killed result carries the run's last assistant
// text as the findings and the salvageable diff-vs-base as the summary.
func TestWanderKill_KilledCarriesLastStateAndDiff(t *testing.T) {
	t.Parallel()
	model := &scriptedModel{steps: []scriptedStep{
		{toolCalls: []scriptedToolCall{{name: "bash", input: "{}"}}},
		{text: "I got as far as reading the layout", toolCalls: []scriptedToolCall{{name: "probe", input: "{}"}}},
	}}
	f := newWanderKillFixture(t, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 1,
	})
	f.buildDispatched(t, model, config.TodoEnforcementSettings{
		Enabled:         true,
		NudgeThreshold:  1,
		KillAfterNudges: 1,
	}, []fantasy.AgentTool{writeMarkerTool(f.ws, f.entry.ID)})

	f.runDispatchSync(t)

	entry := f.requireKilled(t, dispatch.ReasonIgnoredNudges)
	assert.Equal(t, "I got as far as reading the layout", entry.Result.KeyFindings,
		"the last assistant state is the killed run's findings")
	assert.Contains(t, entry.Result.DiffSummary, "marker.txt",
		"the salvageable diff-vs-base is the killed run's summary")
}

// writeMarkerTool is a bash-named tool that writes a marker file into
// the dispatch's workspace, giving the diff something salvageable to
// show. It resolves the workspace path at call time from the registry.
func writeMarkerTool(ws *dispatch.Workspace, id string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"bash",
		"Write the marker.",
		func(ctx context.Context, params struct{}, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			entry, ok := ws.Get(id)
			if !ok {
				return fantasy.NewTextErrorResponse("unknown dispatch"), nil
			}
			if err := os.WriteFile(filepath.Join(entry.Path, "marker.txt"), []byte("salvage me"), 0o644); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse("wrote marker"), nil
		},
	)
}

// TestTerminalMessage_KilledPreservesWorkspace pins the delivered
// payload's wording: a killed dispatch tells the parent the workspace is
// preserved and the decision is re-dispatch or dismiss.
func TestTerminalMessage_KilledPreservesWorkspace(t *testing.T) {
	t.Parallel()
	res := dispatch.DispatchResult{
		DispatchID:   "d-1",
		Branch:       "crush-dispatch-d-1",
		Status:       dispatch.StatusKilled,
		KilledReason: dispatch.ReasonIgnoredNudges,
	}
	msg := res.TerminalMessage()
	assert.Contains(t, msg, "ignored nudges")
	assert.Contains(t, msg, "preserved")
	assert.Contains(t, msg, "re-dispatch")

	done := dispatch.DispatchResult{DispatchID: "d-2", Status: dispatch.StatusCompleted}
	assert.NotContains(t, done.TerminalMessage(), "killed")
}

// TestDispatchResult_KilledRoundTrips pins the wire shape: the kill
// reason rides the JSON payload the parent and the A2A mapping read.
func TestDispatchResult_KilledRoundTrips(t *testing.T) {
	t.Parallel()
	res := dispatch.DispatchResult{
		Status:       dispatch.StatusKilled,
		KilledReason: dispatch.ReasonStalledTodos,
	}
	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"killed_reason":"stalled todos"`)

	var back dispatch.DispatchResult
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, dispatch.ReasonStalledTodos, back.KilledReason)
}

// TestDispatchRunStoppedInLoop pins the loop-stop detector's nil guard;
// the signature match itself is covered by loop_detection_test.go and
// TestWanderKill_ToolLoop covers the attribution end to end.
func TestDispatchRunStoppedInLoop(t *testing.T) {
	t.Parallel()
	assert.False(t, dispatchRunStoppedInLoop(nil))
}
