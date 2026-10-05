package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/stretchr/testify/require"
)

// dispatchForCancelTest dispatches one gated agent through the real
// dispatch tool and parks it inside Run, returning the coordinator, the
// fake, and the running handle.
func dispatchForCancelTest(t *testing.T) (*coordinator, *gatedDispatchAgent, dispatch.DispatchResult) {
	t.Helper()
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()
	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)
	return c, agent, handle
}

// waitTerminalDispatch joins the background run on the given status and
// returns the terminal registry entry.
func waitTerminalDispatch(t *testing.T, c *coordinator, dispatchID string, want dispatch.Status) dispatch.Entry {
	t.Helper()
	var entry dispatch.Entry
	require.Eventually(t, func() bool {
		e, ok := c.dispatchRegistry().Get(dispatchID)
		if !ok || e.Status != want {
			return false
		}
		entry = e
		return true
	}, 10*time.Second, 50*time.Millisecond)
	return entry
}

// CancelDispatch stops one running dispatched agent by any of its three
// addresses — dispatch ID, @handle, or child session ID — and the run
// ends killed with the user-cancel reason, the workspace and its salvage
// diff preserved (#373). The kill path is the watchdog's, so the kill
// reason lands before the agent's cancel and the dispatch's root context
// survives long enough to capture the diff.
func TestCancelDispatchStopsRunningDispatch(t *testing.T) {
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

			// Work in the workspace before the cancel: the salvage diff
			// captured on the kill must still see it, which is only
			// possible if the dispatch's root context survived.
			artifact := filepath.Join(handle.WorkspacePath, "notes.md")
			require.NoError(t, os.WriteFile(artifact, []byte("salvage me"), 0o644))

			require.NoError(t, c.CancelDispatch(t.Context(), tt.ref(handle)))

			require.Equal(t, []string{handle.SessionID}, agent.cancels(),
				"the dispatched agent is canceled, never the parent")
			entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusKilled)
			require.Equal(t, dispatch.ReasonCanceled, entry.Result.KilledReason)
			require.DirExists(t, handle.WorkspacePath,
				"the workspace is preserved for the re-dispatch-or-dismiss decision")
			require.Contains(t, entry.Result.DiffSummary, "notes.md")
			require.NotContains(t, entry.Result.DiffSummary, "(diff unavailable")
		})
	}
}

// A cancel for an unknown ref is refused before anything runs, and a
// finished dispatch refuses with "already finished" — task sessions are
// never continuable, so a cancel must never restart or re-address one.
// Neither path cancels the agent.
func TestCancelDispatchRefusesUnknownAndFinished(t *testing.T) {
	c, agent, handle := dispatchForCancelTest(t)

	require.ErrorContains(t, c.CancelDispatch(t.Context(), "no-such-dispatch"), "no dispatch")
	require.ErrorContains(t, c.CancelDispatch(t.Context(), "@no-such-handle"), "no dispatch")
	require.Empty(t, agent.cancels())

	close(agent.gate)
	waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusCompleted)

	require.ErrorContains(t, c.CancelDispatch(t.Context(), handle.DispatchID), "already finished")
	require.ErrorContains(t, c.CancelDispatch(t.Context(), handle.SessionID), "already finished")
	require.Empty(t, agent.cancels(), "a refusal never reaches the agent")

	require.ErrorContains(t, c.CancelDispatch(t.Context(), "no-such-dispatch"), "no dispatch",
		"an unknown ref is refused after a dispatch exists too")
}

// The cancel goes to the dispatched agent only: the parent session's
// agent is never canceled, so the parent's run is unaffected.
func TestCancelDispatchNeverCancelsParent(t *testing.T) {
	parent := &mockSessionAgent{
		model: dispatchTestModel(),
		runFunc: func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, nil
		},
	}
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	c.mainAgent = parent

	tool := c.dispatchTool()
	handle := decodeDispatchHandle(t, runDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "fix the bug", Branch: "main"}))
	agent.waitRunning(t)

	require.NoError(t, c.CancelDispatch(t.Context(), handle.SessionID))
	require.Equal(t, []string{handle.SessionID}, agent.cancels())
	require.Empty(t, parent.cancelled, "the parent agent is never canceled")
	entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusKilled)
	require.Equal(t, dispatch.ReasonCanceled, entry.Result.KilledReason)
}

// The cancel_dispatch tool drives the same kill through the model's
// interface: success by dispatch_id and by handle, refusals as tool
// error responses, and a missing address refused up front.
func TestCancelDispatchTool(t *testing.T) {
	tests := []struct {
		name    string
		params  func(dispatch.DispatchResult) CancelDispatchParams
		wantErr string
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
		{
			name:    "missing both params",
			params:  func(dispatch.DispatchResult) CancelDispatchParams { return CancelDispatchParams{} },
			wantErr: "dispatch_id or handle is required",
		},
		{
			name: "unknown ref",
			params: func(dispatch.DispatchResult) CancelDispatchParams {
				return CancelDispatchParams{DispatchID: "no-such-dispatch"}
			},
			wantErr: "no dispatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, agent, handle := dispatchForCancelTest(t)
			tool := c.cancelDispatchTool()

			resp := runTool(t, tool, CancelDispatchToolName, tt.params(handle))
			if tt.wantErr != "" {
				require.True(t, resp.IsError, "expected a tool error, got: %s", resp.Content)
				require.Contains(t, resp.Content, tt.wantErr)
				require.Empty(t, agent.cancels())
				return
			}
			require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
			require.Contains(t, resp.Content, "Canceling dispatched agent")
			require.Equal(t, []string{handle.SessionID}, agent.cancels())
			entry := waitTerminalDispatch(t, c, handle.DispatchID, dispatch.StatusKilled)
			require.Equal(t, dispatch.ReasonCanceled, entry.Result.KilledReason)
		})
	}
}
