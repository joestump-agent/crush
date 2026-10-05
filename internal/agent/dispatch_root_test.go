package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

// blockingDispatchAgent is a dispatched-agent fake whose Run blocks until
// released, so a test can end the parent turn while the dispatch is still
// running. It records the context the run was started with: that context
// is the dispatch's root (#371), and teardown must cancel it.
type blockingDispatchAgent struct {
	dispatchTestAgent
	release chan struct{}

	mu     sync.Mutex
	runCtx context.Context
}

func (b *blockingDispatchAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	b.mu.Lock()
	b.runCtx = ctx
	b.mu.Unlock()
	<-b.release
	return b.result, b.err
}

func (b *blockingDispatchAgent) runContext() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runCtx
}

// recordingPermissionService counts Request calls on the parent service,
// so a test can tell a live bridge (which forwards every scoped request)
// from a dead one (which forwards nothing).
type recordingPermissionService struct {
	permission.Service

	mu    sync.Mutex
	calls int
}

func (r *recordingPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.Service.Request(ctx, req)
}

func (r *recordingPermissionService) requestCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// bridgeFixture is a coordinator wired for bridge tests: the parent
// permission service does not skip requests (unlike testEnv's), the
// dispatched-agent builder is a fake that captures the toolchain, and the
// dispatched agent blocks until released. The parent turn is already over
// when the fixture returns: the tool call's context is canceled.
type bridgeFixture struct {
	c         *coordinator
	parent    *recordingPermissionService
	toolchain *DispatchToolchain
	handle    dispatch.DispatchResult
	agent     *blockingDispatchAgent

	releaseOnce sync.Once
}

func (f *bridgeFixture) releaseAgent() {
	f.releaseOnce.Do(func() { close(f.agent.release) })
}

func newBridgeFixture(t *testing.T) *bridgeFixture {
	t.Helper()

	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)

	f := &bridgeFixture{
		c: c,
		parent: &recordingPermissionService{
			Service: permission.NewPermissionService(t.TempDir(), false, nil),
		},
		agent: &blockingDispatchAgent{
			dispatchTestAgent: dispatchTestAgent{
				model:  dispatchTestModel(),
				result: &fantasy.AgentResult{},
			},
			release: make(chan struct{}),
		},
	}
	c.permissions = f.parent
	t.Cleanup(f.releaseAgent)

	c.dispatchAgentBuilder = func(_ context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		f.toolchain = opts.Toolchain
		return &dispatchedAgent{
			agent:       f.agent,
			model:       f.agent.model,
			providerCfg: config.ProviderConfig{ID: "test-provider"},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, "dispatch-parent-session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "dispatch-parent-message")
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	input, err := json.Marshal(DispatchAgentParams{Prompt: "do work"})
	require.NoError(t, err)
	resp, err := c.dispatchTool().Run(ctx, fantasy.ToolCall{
		ID:    "dispatch-tool-call",
		Name:  DispatchAgentToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	f.handle = decodeDispatchHandle(t, resp)
	require.NotNil(t, f.toolchain, "builder never captured the toolchain")

	// The parent turn ends as soon as the tool call returns: the
	// dispatched run and its bridge must not.
	cancel()
	return f
}

// A scoped permission request raised after the dispatch tool call's
// context is canceled still reaches the parent's subscribers, and the
// parent's grant resolves it (#371).
func TestDispatchPermissionBridgeGrantAfterTurnEnds(t *testing.T) {
	f := newBridgeFixture(t)

	sub := f.parent.Subscribe(t.Context())
	allowedCh := make(chan error, 1)
	go func() {
		allowed, err := f.toolchain.Permissions().Request(context.Background(), permission.CreatePermissionRequest{
			SessionID:   f.handle.SessionID,
			ToolCallID:  "scoped-grant",
			ToolName:    "bash",
			Description: "Execute command: make test",
			Action:      "execute",
		})
		if err == nil && allowed {
			allowedCh <- nil
		} else {
			allowedCh <- errors.New("scoped request did not resolve as granted")
		}
	}()

	select {
	case ev := <-sub:
		require.Equal(t, "scoped-grant", ev.Payload.ToolCallID)
		require.True(t, f.parent.Grant(ev.Payload), "parent grant did not resolve the pending request")
	case <-time.After(10 * time.Second):
		t.Fatal("scoped request never reached the parent after the turn ended")
	}

	select {
	case err := <-allowedCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("scoped request was not resolved by the parent's grant")
	}
}

// The deny path mirrors the grant: the parent's verdict resolves the
// scoped request as denied (#371).
func TestDispatchPermissionBridgeDenyAfterTurnEnds(t *testing.T) {
	f := newBridgeFixture(t)

	sub := f.parent.Subscribe(t.Context())
	allowedCh := make(chan bool, 1)
	go func() {
		allowed, err := f.toolchain.Permissions().Request(context.Background(), permission.CreatePermissionRequest{
			SessionID:   f.handle.SessionID,
			ToolCallID:  "scoped-deny",
			ToolName:    "bash",
			Description: "Execute command: rm -rf /",
			Action:      "execute",
		})
		require.NoError(t, err)
		allowedCh <- allowed
	}()

	select {
	case ev := <-sub:
		require.Equal(t, "scoped-deny", ev.Payload.ToolCallID)
		require.True(t, f.parent.Deny(ev.Payload), "parent deny did not resolve the pending request")
	case <-time.After(10 * time.Second):
		t.Fatal("scoped request never reached the parent after the turn ended")
	}

	select {
	case allowed := <-allowedCh:
		require.False(t, allowed)
	case <-time.After(10 * time.Second):
		t.Fatal("scoped request was not resolved by the parent's deny")
	}
}

// When the dispatched run returns, its teardown cancels the dispatch's
// root (the bridge exits with it), drops the live record, and a later
// scoped request fails with a context error instead of stranding a
// waiter (#371).
func TestDispatchTeardownCancelsRootAndDropsRecord(t *testing.T) {
	f := newBridgeFixture(t)

	require.Eventually(t, func() bool {
		f.c.dispatchMu.Lock()
		defer f.c.dispatchMu.Unlock()
		live, ok := f.c.liveDispatches[f.handle.DispatchID]
		return ok &&
			live.sessionID == f.handle.SessionID &&
			live.agent == SessionAgent(f.agent) &&
			live.kill != nil &&
			live.cancel != nil &&
			live.done != nil
	}, 10*time.Second, 10*time.Millisecond, "running dispatch not registered in liveDispatches")

	f.releaseAgent()

	require.Eventually(t, func() bool {
		entry, ok := f.c.dispatchWS.Get(f.handle.DispatchID)
		return ok && entry.Status != dispatch.StatusRunning
	}, 10*time.Second, 10*time.Millisecond, "dispatch never reached a terminal status")

	require.Eventually(t, func() bool {
		f.c.dispatchMu.Lock()
		defer f.c.dispatchMu.Unlock()
		_, ok := f.c.liveDispatches[f.handle.DispatchID]
		return !ok
	}, 10*time.Second, 10*time.Millisecond, "liveDispatches entry not dropped at teardown")

	require.Error(t, f.agent.runContext().Err(), "dispatch root context still alive after teardown")

	ctxReq, cancelReq := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelReq()
	_, err := f.toolchain.Permissions().Request(ctxReq, permission.CreatePermissionRequest{
		SessionID:  f.handle.SessionID,
		ToolCallID: "scoped-after-teardown",
		ToolName:   "bash",
		Action:     "execute",
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// A setup failure after the root exists cancels it: the bridge dies with
// the dispatch, so the parent never sees a forwarded request and the
// scoped waiter resolves with a context error (#371).
func TestDispatchSetupFailureCancelsRoot(t *testing.T) {
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)

	parent := &recordingPermissionService{
		Service: permission.NewPermissionService(t.TempDir(), false, nil),
	}
	c.permissions = parent

	var toolchain *DispatchToolchain
	c.dispatchAgentBuilder = func(_ context.Context, opts dispatchAgentOptions) (*dispatchedAgent, error) {
		toolchain = opts.Toolchain
		return nil, errors.New("boom")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, tools.SessionIDContextKey, "dispatch-parent-session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "dispatch-parent-message")
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	input, err := json.Marshal(DispatchAgentParams{Prompt: "do work"})
	require.NoError(t, err)
	resp, err := c.dispatchTool().Run(ctx, fantasy.ToolCall{
		ID:    "dispatch-tool-call",
		Name:  DispatchAgentToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "build dispatched agent")
	require.NotNil(t, toolchain)
	cancel()

	ctxReq, cancelReq := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelReq()
	_, err = toolchain.Permissions().Request(ctxReq, permission.CreatePermissionRequest{
		SessionID:  "dispatch-parent-session",
		ToolCallID: "scoped-failed-setup",
		ToolName:   "bash",
		Action:     "execute",
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, parent.requestCalls(), "dead bridge must not forward requests to the parent")
}
