package agent

// Session scoping of the dispatch decision tools (#559): cancel_dispatch,
// apply_dispatch and dismiss_dispatch resolve only the calling session's
// dispatches, the way steering and @handle lookups do (#399). The
// fixtures dispatch from "dispatch-parent-session" (runDispatchToolCall)
// and drive the tools from that session, from a stranger's, and from no
// session at all.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// runToolWithoutSession runs a tool on a context carrying no session —
// the shape a call from outside the agent loop would have.
func runToolWithoutSession(t *testing.T, tool fantasy.AgentTool, name string, params any) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{
		ID:    "dispatch-test-call-no-session",
		Name:  name,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

// finishedDispatchWithPrompting is finishedDispatchFixture with the
// parent's permission service in prompting mode, installed before the
// dispatch so the tools' permission asks are observable on the returned
// events channel. The dispatch carries the handle "tester".
func finishedDispatchWithPrompting(t *testing.T, mutate func(workspacePath string)) (*coordinator, dispatch.Entry, <-chan pubsub.Event[permission.PermissionRequest]) {
	t.Helper()
	sealGitConfig(t)
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	c.permissions = permission.NewPermissionService(c.cfg.WorkingDir(), false, nil)
	tool := c.dispatchTool()
	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main", Handle: "tester"}))
	agent.waitRunning(t)
	if mutate != nil {
		mutate(handle.WorkspacePath)
	}
	agent.release()
	entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusCompleted)
	return c, entry, c.permissions.Subscribe(t.Context())
}

// CancelDispatch is scoped to the calling session (#559): another session
// cannot cancel the dispatch by any of its three addresses, the refusal
// reads like an unknown ref and reveals nothing else about the target,
// the agent is never canceled — and the dispatching session still can.
func TestCancelDispatchScopedToCallerSession(t *testing.T) {
	tests := []struct {
		name string
		ref  func(dispatch.DispatchResult) string
	}{
		{"by dispatch ID", func(h dispatch.DispatchResult) string { return h.DispatchID }},
		{"by handle", func(h dispatch.DispatchResult) string { return "@" + h.Handle }},
		{"by session ID", func(h dispatch.DispatchResult) string { return h.SessionID }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, agent, handle := dispatchForCancelTest(t)
			ref := tt.ref(handle)

			err := c.CancelDispatch(t.Context(), "dispatch-other-session", ref)
			require.ErrorContains(t, err, "no dispatch")
			require.ErrorContains(t, err, "in this session")
			require.NotContains(t, err.Error(), "running", "the refusal must not reveal the dispatch's status")
			if ref != handle.DispatchID {
				require.NotContains(t, err.Error(), handle.DispatchID, "the refusal must not reveal the dispatch ID")
			}
			require.Empty(t, agent.cancels(), "a foreign session's cancel never reaches the agent")
			entry, ok := c.dispatchRegistry().Get(handle.DispatchID)
			require.True(t, ok)
			require.Equal(t, dispatch.StatusRunning, entry.Status, "the dispatch keeps running")

			require.NoError(t, c.CancelDispatch(t.Context(), "dispatch-parent-session", ref))
			require.Equal(t, []string{handle.SessionID}, agent.cancels())
			entry = waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusKilled)
			require.Equal(t, dispatch.ReasonCanceled, entry.Result.KilledReason)
		})
	}
}

// The cancel_dispatch tool scopes through the session it runs in: a
// foreign session's call refuses by dispatch_id and by handle, a call
// with no session behind it refuses outright instead of falling through
// to the unscoped path, and the dispatching session's call kills the run.
func TestCancelDispatchToolScopedToCallerSession(t *testing.T) {
	tests := []struct {
		name   string
		params func(dispatch.DispatchResult) CancelDispatchParams
	}{
		{
			name: "by dispatch id",
			params: func(h dispatch.DispatchResult) CancelDispatchParams {
				return CancelDispatchParams{DispatchID: h.DispatchID}
			},
		},
		{
			name: "by handle",
			params: func(h dispatch.DispatchResult) CancelDispatchParams {
				return CancelDispatchParams{Handle: "@" + h.Handle}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, agent, handle := dispatchForCancelTest(t)
			tool := c.cancelDispatchTool()

			resp := runToolAsSession(t, tool, CancelDispatchToolName, tt.params(handle), "dispatch-other-session")
			require.True(t, resp.IsError, "a foreign session's cancel must refuse: %s", resp.Content)
			require.Contains(t, resp.Content, "no dispatch")
			require.Contains(t, resp.Content, "in this session")
			require.Empty(t, agent.cancels())

			resp = runToolWithoutSession(t, tool, CancelDispatchToolName, tt.params(handle))
			require.True(t, resp.IsError, "a call with no session must refuse: %s", resp.Content)
			require.Contains(t, resp.Content, "no calling session")
			require.Empty(t, agent.cancels())

			resp = runToolAsSession(t, tool, CancelDispatchToolName, tt.params(handle), "dispatch-parent-session")
			require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
			require.Contains(t, resp.Content, "Canceling dispatched agent")
			require.Equal(t, []string{handle.SessionID}, agent.cancels())
			entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusKilled)
			require.Equal(t, dispatch.ReasonCanceled, entry.Result.KilledReason)
		})
	}
}

// An empty caller is CancelDispatch's explicit unscoped path (#559), for a
// caller with no session behind it: it still resolves the dispatch. Only
// the model-facing tool is barred from taking it, which
// TestCancelDispatchToolScopedToCallerSession pins.
func TestCancelDispatchWithoutCallerIsUnscoped(t *testing.T) {
	c, agent, handle := dispatchForCancelTest(t)

	require.NoError(t, c.CancelDispatch(t.Context(), "", handle.SessionID))
	require.Equal(t, []string{handle.SessionID}, agent.cancels())
	entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusKilled)
	require.Equal(t, dispatch.ReasonCanceled, entry.Result.KilledReason)
}

// apply_dispatch and dismiss_dispatch resolve only the calling session's
// finished dispatches (#559): a foreign session is refused by dispatch_id
// and by handle before any permission is asked, the refusal reads like an
// unknown dispatch and reveals neither branch nor status, a call with no
// session behind it refuses the same way, and the workspace survives
// untouched. The dispatching session's own call then goes through.
func TestApplyAndDismissScopedToCallerSession(t *testing.T) {
	c, entry, events := finishedDispatchWithPrompting(t, func(ws string) {
		require.NoError(t, os.WriteFile(filepath.Join(ws, "kept.txt"), []byte("keep me"), 0o644))
	})
	require.Equal(t, "dispatch-parent-session", entry.ParentSessionID)

	calls := []struct {
		name   string
		tool   fantasy.AgentTool
		params any
	}{
		{"apply by id", c.applyDispatchTool(), ApplyDispatchParams{DispatchID: entry.ID}},
		{"apply by handle", c.applyDispatchTool(), ApplyDispatchParams{Handle: "@tester"}},
		{"dismiss by id", c.dismissDispatchTool(), DismissDispatchParams{DispatchID: entry.ID}},
		{"dismiss by handle", c.dismissDispatchTool(), DismissDispatchParams{Handle: "tester"}},
	}
	for _, tc := range calls {
		name := tc.tool.Info().Name
		resp := runToolAsSession(t, tc.tool, name, tc.params, "dispatch-other-session")
		require.True(t, resp.IsError, "%s from a foreign session must refuse: %s", tc.name, resp.Content)
		require.Contains(t, resp.Content, "no dispatch", tc.name)
		require.Contains(t, resp.Content, "in this session", tc.name)
		require.NotContains(t, resp.Content, entry.Branch, "%s must not reveal the branch", tc.name)
		require.NotContains(t, resp.Content, "finished", "%s must not reveal the status", tc.name)

		resp = runToolWithoutSession(t, tc.tool, name, tc.params)
		require.True(t, resp.IsError, "%s with no session must refuse: %s", tc.name, resp.Content)
		require.Contains(t, resp.Content, "no calling session", tc.name)
	}

	select {
	case ev := <-events:
		t.Fatalf("a scoped refusal must never ask for permission, got %q", ev.Payload.Description)
	default:
	}
	require.DirExists(t, entry.Path)
	require.FileExists(t, filepath.Join(entry.Path, "kept.txt"))
	_, ok := c.dispatchRegistry().Get(entry.ID)
	require.True(t, ok, "a refusal leaves the dispatch in the registry")

	// The dispatching session's own dismiss asks, is granted, and tears
	// the workspace down — the scope check is not in its way.
	resCh := make(chan fantasy.ToolResponse, 1)
	go func() {
		resCh <- runToolAsSession(t, c.dismissDispatchTool(), DismissDispatchToolName, DismissDispatchParams{Handle: "tester"}, "dispatch-parent-session")
	}()
	select {
	case ev := <-events:
		require.True(t, c.permissions.Grant(ev.Payload))
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatching session's dismiss should ask the parent for permission")
	}
	resp := <-resCh
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	requireWorkspaceGone(t, c, entry, "tester")
}

// The apply and dismiss permission prompts name the dispatch the way the
// user knows it (#559): its @handle, its branch, and the session that
// dispatched it, so the person approving is told whose work this is.
func TestApplyAndDismissPermissionDescribesHandleAndSession(t *testing.T) {
	c, entry, events := finishedDispatchWithPrompting(t, nil)

	// askThenDeny runs a tool call, captures the permission ask it
	// publishes, denies it so nothing changes, and returns the ask.
	askThenDeny := func(run func() fantasy.ToolResponse) permission.PermissionRequest {
		t.Helper()
		resCh := make(chan fantasy.ToolResponse, 1)
		go func() { resCh <- run() }()
		var ev pubsub.Event[permission.PermissionRequest]
		select {
		case ev = <-events:
		case <-time.After(5 * time.Second):
			t.Fatal("the tool should ask the parent for permission")
		}
		require.True(t, c.permissions.Deny(ev.Payload))
		resp := <-resCh
		require.True(t, resp.IsError, "a denied call must refuse: %s", resp.Content)
		return ev.Payload
	}

	req := askThenDeny(func() fantasy.ToolResponse {
		return runToolAsSession(t, c.applyDispatchTool(), ApplyDispatchToolName, ApplyDispatchParams{Handle: "tester", Mode: "squash"}, "dispatch-parent-session")
	})
	require.Equal(t, ApplyDispatchToolName, req.ToolName)
	require.Equal(t, "dispatch-parent-session", req.SessionID)
	require.Contains(t, req.Description, entry.ID)
	require.Contains(t, req.Description, "@tester")
	require.Contains(t, req.Description, "branch "+entry.Branch)
	require.Contains(t, req.Description, "dispatched from session dispatch-parent-session")
	require.Contains(t, req.Description, "via squash")

	req = askThenDeny(func() fantasy.ToolResponse {
		return runToolAsSession(t, c.dismissDispatchTool(), DismissDispatchToolName, DismissDispatchParams{DispatchID: entry.ID}, "dispatch-parent-session")
	})
	require.Equal(t, DismissDispatchToolName, req.ToolName)
	require.Contains(t, req.Description, entry.ID)
	require.Contains(t, req.Description, "@tester")
	require.Contains(t, req.Description, "branch "+entry.Branch)
	require.Contains(t, req.Description, "dispatched from session dispatch-parent-session")

	// Both were denied: the workspace and the entry survive.
	require.DirExists(t, entry.Path)
	_, ok := c.dispatchRegistry().Get(entry.ID)
	require.True(t, ok)
}

// dispatchPermissionSubject carries every identity the prompt needs and
// leaves out what the entry lacks rather than printing blanks.
func TestDispatchPermissionSubject(t *testing.T) {
	t.Parallel()

	full := dispatch.Entry{ID: "d-1", Handle: "tester", Branch: "crush-dispatch-d-1", ParentSessionID: "sess-a"}
	require.Equal(t, "d-1 (@tester, branch crush-dispatch-d-1, dispatched from session sess-a)", dispatchPermissionSubject(full))

	bare := dispatch.Entry{ID: "d-2", Branch: "crush-dispatch-d-2"}
	require.Equal(t, "d-2 (branch crush-dispatch-d-2)", dispatchPermissionSubject(bare))
}
