package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// runUniqueDispatchToolCall invokes the DispatchAgent tool like
// runDispatchToolCall but with unique message and tool-call IDs, so
// several dispatches can run in one environment (the task session ID is
// messageID$$toolCallID and must not repeat).
func runUniqueDispatchToolCall(t *testing.T, tool fantasy.AgentTool, params DispatchAgentParams, seq int) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "dispatch-parent-session")
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, fmt.Sprintf("dispatch-parent-message-%d", seq))
	ctx = context.WithValue(ctx, tools.ContentWidthContextKey, 80)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    fmt.Sprintf("dispatch-tool-call-%d", seq),
		Name:  DispatchAgentToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

// The dispatch tool assigns handles (#313): an explicit one, one derived
// from the role, the default fallback — and collisions across concurrent
// dispatches get numeric suffixes. The running handle carries the handle
// so the model can address the agent from the moment the tool returns.
func TestDispatchToolAssignsHandles(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	first := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "@Tester",
		Role:   "writes tests",
	}, 1))
	require.Equal(t, "tester", first.Handle)

	second := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the other bug",
		Branch: "main",
		Handle: "tester",
	}, 2))
	require.Equal(t, "tester-2", second.Handle)

	third := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "document it",
		Branch: "main",
		Role:   "Docs Writer",
	}, 3))
	require.Equal(t, "docs-writer", third.Handle)

	fourth := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "do a thing",
		Branch: "main",
	}, 4))
	require.Equal(t, "agent", fourth.Handle)

	// The registry entries carry handle and role, and resolve by handle
	// for the dispatching session.
	snap, ok := c.DispatchByHandle("dispatch-parent-session", "tester")
	require.True(t, ok)
	require.Equal(t, first.DispatchID, snap.Entry.ID)
	require.Equal(t, "writes tests", snap.Entry.Role)

	close(agent.gate)
}

// DispatchLive lists only non-terminal dispatches (#313): finished
// handles never appear in the completions, keeping the not-continuable
// rule honest.
func TestDispatchLiveExcludesFinished(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	first := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "a", Branch: "main", Handle: "tester"}, 5))
	agent.waitRunning(t)
	decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{Prompt: "b", Branch: "main", Handle: "docs"}, 6))

	live := c.DispatchLive("dispatch-parent-session")
	handles := make(map[string]bool, len(live))
	for _, snap := range live {
		handles[snap.Entry.Handle] = true
	}
	require.True(t, handles["tester"], "a running dispatch must be live")
	require.True(t, handles["docs"])

	// Finish the first dispatch; its handle leaves the live set but still
	// resolves by handle (finished dispatches keep their handle).
	close(agent.gate)
	require.Eventually(t, func() bool {
		entry, ok := c.dispatchRegistry().Get(first.DispatchID)
		return ok && entry.Status.IsTerminal()
	}, 10*time.Second, 50*time.Millisecond)

	live = c.DispatchLive("dispatch-parent-session")
	for _, snap := range live {
		require.NotEqual(t, "tester", snap.Entry.Handle, "a finished dispatch must not be live")
	}
	snap, ok := c.DispatchByHandle("dispatch-parent-session", "tester")
	require.True(t, ok, "a finished dispatch still resolves by handle")
	require.True(t, snap.Entry.Status.IsTerminal())
	require.Equal(t, first.SessionID, snap.Entry.SessionID)

	// DeliverAgentMessageByHandle refuses the finished handle cleanly.
	err := c.DeliverAgentMessageByHandle(t.Context(), "dispatch-parent-session", "tester", "one more thing", nil)
	require.ErrorContains(t, err, "finished")
	require.ErrorContains(t, err, "dispatch a new agent")
}

// DeliverAgentMessageByHandle routes to the running agent behind the
// handle (#313's editor front door over the #312 seam), and the message
// tool accepts the handle as the address.
func TestDeliverByHandleRoutesAndToolAcceptsHandle(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "tester",
	}, 7))
	agent.waitRunning(t)

	atts := []message.Attachment{{FileName: "ref.png", MimeType: "image/png", Content: []byte("png")}}
	require.NoError(t, c.DeliverAgentMessageByHandle(t.Context(), "dispatch-parent-session", "tester", "stop writing Rust", atts))
	require.NoError(t, c.DeliverAgentMessageByHandle(t.Context(), "dispatch-parent-session", "@Tester", "normalized too", nil))
	injected := agent.injected()
	require.Len(t, injected, 2)
	require.Equal(t, "stop writing Rust", injected[0].Prompt)
	require.Equal(t, "normalized too", injected[1].Prompt)
	require.Equal(t, handle.SessionID, injected[0].SessionID)
	require.Equal(t, atts, injected[0].Attachments, "the editor's attachments ride the enqueued call")
	require.Nil(t, injected[1].Attachments)

	// Unknown handle refuses.
	err := c.DeliverAgentMessageByHandle(t.Context(), "dispatch-parent-session", "ghost", "hello", nil)
	require.ErrorContains(t, err, "no agent with handle @ghost")

	// The model-facing tool addresses by handle as well, with the "@"
	// tolerated.
	resp := runToolAsSession(t, c.messageAgentTool(), MessageAgentToolName, MessageAgentParams{
		Handle:  "@tester",
		Message: "add tests please",
	}, "dispatch-parent-session")
	require.False(t, resp.IsError, "unexpected tool error: %s", resp.Content)
	require.Contains(t, resp.Content, "@tester")
	require.Len(t, agent.injected(), 3)

	close(agent.gate)
}

// runToolAsSession is runTool over a caller-supplied session: the
// message_agent tool scopes its delivery to the session it runs in
// (#399), so tests must be able to pick it.
func runToolAsSession(t *testing.T, tool fantasy.AgentTool, name string, params any, sessionID string) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, sessionID)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "dispatch-test-call-session",
		Name:  name,
		Input: string(input),
	})
	require.NoError(t, err)
	return resp
}

// Dispatches are scoped to the session that created them (#399): another
// session's model cannot address the agent by session ID or by handle,
// the refusal learns nothing about the target, and a foreign session's
// surfaces resolve nothing.
func TestDeliverScopedToCallerSession(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, _ := newInjectionEnv(t, agent)
	tool := c.dispatchTool()

	handle := decodeDispatchHandle(t, runUniqueDispatchToolCall(t, tool, DispatchAgentParams{
		Prompt: "fix the bug",
		Branch: "main",
		Handle: "tester",
	}, 8))
	agent.waitRunning(t)

	err := c.DeliverAgentMessage(t.Context(), AgentMessage{
		SessionID:     handle.SessionID,
		FromSessionID: "dispatch-other-session",
		Text:          "hello from next door",
	})
	require.ErrorContains(t, err, "no running agent")
	require.ErrorContains(t, err, "dispatch one first")

	err = c.DeliverAgentMessageByHandle(t.Context(), "dispatch-other-session", "tester", "hello from next door", nil)
	require.ErrorContains(t, err, "no agent with handle @tester")

	_, ok := c.DispatchByHandle("dispatch-other-session", "tester")
	require.False(t, ok)
	require.Empty(t, c.DispatchLive("dispatch-other-session"))

	resp := runToolAsSession(t, c.messageAgentTool(), MessageAgentToolName, MessageAgentParams{
		SessionID: handle.SessionID,
		Message:   "from the wrong session",
	}, "dispatch-other-session")
	require.True(t, resp.IsError, "a foreign session's tool call must refuse")
	require.Contains(t, resp.Content, "no running agent")

	resp = runToolAsSession(t, c.messageAgentTool(), MessageAgentToolName, MessageAgentParams{
		Handle:  "tester",
		Message: "from the wrong session",
	}, "dispatch-other-session")
	require.True(t, resp.IsError, "a foreign session's handle tool call must refuse")

	require.NoError(t, c.DeliverAgentMessage(t.Context(), AgentMessage{
		SessionID:     handle.SessionID,
		FromSessionID: "dispatch-parent-session",
		Text:          "from the parent",
	}))
	require.NoError(t, c.DeliverAgentMessageByHandle(t.Context(), "dispatch-parent-session", "tester", "from the parent too", nil))
	require.Len(t, agent.injected(), 2)

	close(agent.gate)
}

// The @-completion and handle-routing surfaces read the dispatch
// registry without provisioning it (#370): a single @ keystroke must
// not run git or create <workingDir>/.crush in a repo where the user
// has never dispatched an agent.
func TestHandleSurfacesDoNotProvisionWorkspace(t *testing.T) {
	agent := newGatedDispatchAgent()
	c, env := newInjectionEnv(t, agent)

	require.Nil(t, c.DispatchLive(""))
	_, ok := c.DispatchByHandle("", "ghost")
	require.False(t, ok)
	err := c.DeliverAgentMessageByHandle(t.Context(), "", "ghost", "hello", nil)
	require.ErrorContains(t, err, "no agent with handle @ghost")

	_, err = os.Stat(filepath.Join(env.workingDir, ".crush"))
	require.True(t, os.IsNotExist(err), "a handle read must not create .crush")
	c.dispatchMu.Lock()
	provider, collector := c.dispatchProvider, c.dispatchCollector
	c.dispatchMu.Unlock()
	require.Nil(t, provider)
	require.Nil(t, collector)
}

// The same holds in a directory with no git repository at all: the
// live/handle surfaces report empty, and delivery refuses with the
// unknown-handle error, without provisioning anything (#370).
func TestHandleSurfacesInNonGitDirectory(t *testing.T) {
	env := testEnv(t)
	c := newDispatchTestCoordinator(t, env)

	require.Nil(t, c.DispatchLive(""))
	_, ok := c.DispatchByHandle("", "ghost")
	require.False(t, ok)
	err := c.DeliverAgentMessageByHandle(t.Context(), "", "ghost", "hello", nil)
	require.ErrorContains(t, err, "no agent with handle @ghost")

	_, err = os.Stat(filepath.Join(env.workingDir, ".crush"))
	require.True(t, os.IsNotExist(err), "a handle read must not create .crush")
}
