package chat

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// newDispatchItem builds a dispatch agent block around one tool call,
// with or without a persisted tool result.
func newDispatchItem(t *testing.T, result *message.ToolResult) *DispatchToolMessageItem {
	t.Helper()
	sty := styles.CharmtonePantera()
	return NewDispatchToolMessageItem(&sty, message.ToolCall{
		ID:       "call-dispatch-1",
		Name:     agent.DispatchAgentToolName,
		Input:    `{"prompt":"implement the login form with validation"}`,
		Finished: true,
	}, result, false)
}

// dispatchToolOpts renders the item's render context directly, like the
// other tool render tests.
func dispatchToolOpts(result *message.ToolResult, spinning bool) *ToolRenderOpts {
	return &ToolRenderOpts{
		ToolCall: message.ToolCall{
			ID:       "call-dispatch-1",
			Name:     agent.DispatchAgentToolName,
			Input:    `{"prompt":"implement the login form with validation"}`,
			Finished: true,
		},
		Result:     result,
		Anim:       anim.New(anim.Settings{ID: "call-dispatch-1"}),
		Status:     ToolStatusSuccess,
		IsSpinning: spinning,
	}
}

func dispatchTestRender(t *testing.T, item *DispatchToolMessageItem, opts *ToolRenderOpts) string {
	t.Helper()
	sty := styles.CharmtonePantera()
	return ansi.Strip((&DispatchToolRenderContext{dispatch: item}).RenderTool(&sty, 100, opts))
}

// runningHandle is the JSON the dispatch_agent tool returns immediately.
func runningHandle(t *testing.T, status dispatch.Status) *message.ToolResult {
	t.Helper()
	handle := dispatch.DispatchResult{
		DispatchID:    "dispatch-1",
		Branch:        "crush-dispatch-dispatch-1",
		WorkspacePath: "/tmp/ws",
		SessionID:     "msg$$call-dispatch-1",
		Status:        status,
	}
	b, err := json.Marshal(handle)
	require.NoError(t, err)
	return &message.ToolResult{ToolCallID: "call-dispatch-1", Content: string(b)}
}

// The live card composes from a collector snapshot: state, elapsed,
// tokens, todo ratio, and the current todo as the one-line activity,
// next to the dispatched task.
func TestDispatchCardRendersLiveSnapshot(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:        "dispatch-1",
			SessionID: "msg$$call-dispatch-1",
			Status:    dispatch.StatusRunning,
			StartedAt: time.Now().Add(-30 * time.Second),
		},
		CurrentTodo:      "wiring up form validation",
		TodoCompleted:    1,
		TodoTotal:        3,
		PromptTokens:     1500,
		CompletionTokens: 300,
	})

	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), true))

	require.Contains(t, out, "Dispatch")
	require.Contains(t, out, "working")
	require.Contains(t, out, "30s")
	require.Contains(t, out, "1.8K tokens")
	require.Contains(t, out, "1/3 todos")
	require.Contains(t, out, "wiring up form validation")
	require.Contains(t, out, "implement the login form with validation")
	require.True(t, item.Spinning())
}

// The queued state renders before the run starts.
func TestDispatchCardRendersQueuedState(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, nil)
	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:        "dispatch-1",
			SessionID: "msg$$call-dispatch-1",
			Status:    dispatch.StatusProvisioned,
		},
	})

	out := dispatchTestRender(t, item, dispatchToolOpts(nil, true))
	require.Contains(t, out, "queued")
	require.True(t, item.Spinning())
}

// On completion the block stays as the durable record: the findings
// summary and the diff stat from the terminal DispatchResult, and no
// spinner.
func TestDispatchCardCompletionIsDurable(t *testing.T) {
	t.Parallel()

	started := time.Now().Add(-2 * time.Minute)
	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:         "dispatch-1",
			SessionID:  "msg$$call-dispatch-1",
			Status:     dispatch.StatusCompleted,
			StartedAt:  started,
			FinishedAt: started.Add(74 * time.Second),
			Result: &dispatch.DispatchResult{
				DispatchID:  "dispatch-1",
				Status:      dispatch.StatusCompleted,
				KeyFindings: "Added validation and two tests.",
				DiffSummary: "internal/ui/login.go | +12 -3\n\n+func validate()",
			},
		},
		TodoCompleted: 3,
		TodoTotal:     3,
	})

	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), false))

	require.Contains(t, out, "complete")
	require.Contains(t, out, "1m14s")
	require.Contains(t, out, "Added validation and two tests.")
	require.Contains(t, out, "internal/ui/login.go | +12 -3")
	require.False(t, item.Spinning())
}

// A failed run surfaces its error in the durable record.
func TestDispatchCardFailureShowsError(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:        "dispatch-1",
			SessionID: "msg$$call-dispatch-1",
			Status:    dispatch.StatusFailed,
			Result: &dispatch.DispatchResult{
				Status: dispatch.StatusFailed,
				Error:  "provider timeout",
			},
		},
	})

	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), false))
	require.Contains(t, out, "failed")
	require.Contains(t, out, "provider timeout")
	require.False(t, item.Spinning())
}

// A reloaded session renders from the persisted running handle without
// a live snapshot — and never spins, because nothing will advance the
// card again.
func TestDispatchCardStaleHandleIsStatic(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), false))

	require.Contains(t, out, "working")
	require.False(t, item.Spinning())
	require.Equal(t, "msg$$call-dispatch-1", item.DispatchSessionID())
}

// A result arriving live re-parses the handle.
func TestDispatchCardSetResultReparsesHandle(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, nil)
	// No result yet: the tool call is still open, so the card spins
	// while the dispatch provisions.
	require.True(t, item.Spinning())
	item.SetResult(runningHandle(t, dispatch.StatusRunning))
	require.Equal(t, "msg$$call-dispatch-1", item.DispatchSessionID())
	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), false))
	require.Contains(t, out, "working")
}

// The dispatch block is a nested tool container: the child session's
// tool activity nests underneath it.
func TestDispatchCardNestedTools(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	nested := NewBashToolMessageItem(&sty, message.ToolCall{
		ID:       "nested-1",
		Name:     "bash",
		Input:    `{"command":"go test ./..."}`,
		Finished: true,
	}, nil, false, "")

	item.AddNestedTool(nested)
	require.Len(t, item.NestedTools(), 1)

	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), true))
	require.Contains(t, out, "go test ./...")

	item.SetNestedTools(nil)
	require.Empty(t, item.NestedTools())
}

// Mid-run injection (#312): an injected message recorded on the block
// renders as a steer with the agent's streaming answer beneath it, and
// the initial dispatch prompt is never mistaken for a steer.
func TestDispatchCardRendersSteerConversation(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:        "dispatch-1",
			SessionID: "msg$$call-dispatch-1",
			Status:    dispatch.StatusRunning,
		},
	})

	// The dispatch's own prompt is not a steer.
	require.True(t, item.IsInitialDispatchPrompt("implement the login form with validation"))
	// Any other text before the first steer is not the prompt either.
	require.False(t, item.IsInitialDispatchPrompt("stop writing Rust"))

	item.AddSteer("stop writing Rust and use Go")
	item.UpdateSteerAnswer("assistant-1", "understood, switching to Go")
	// A later assistant message retargets the answer: the block shows the
	// agent's most recent reply.
	item.UpdateSteerAnswer("assistant-2", "done, go.mod updated")

	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), true))
	require.Contains(t, out, "stop writing Rust and use Go")
	require.Contains(t, out, "done, go.mod updated")
	require.NotContains(t, out, "understood, switching to Go")

	// The second steer starts a fresh answer slot; the first keeps its
	// final text.
	item.AddSteer("also run the linter")
	item.UpdateSteerAnswer("assistant-3", "lint clean")
	out = dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), true))
	require.Contains(t, out, "also run the linter")
	require.Contains(t, out, "lint clean")
	require.Contains(t, out, "done, go.mod updated")

	require.Len(t, item.Steers(), 2)
	// After the first steer, nothing is ever treated as the initial
	// prompt again.
	require.False(t, item.IsInitialDispatchPrompt("implement the login form with validation"))
}

// Steers survive into the terminal record view: the block keeps the
// conversation next to the findings once the run completes.
func TestDispatchCardKeepsSteersOnCompletion(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:        "dispatch-1",
			SessionID: "msg$$call-dispatch-1",
			Status:    dispatch.StatusCompleted,
			Result: &dispatch.DispatchResult{
				Status:      dispatch.StatusCompleted,
				KeyFindings: "Switched to Go.",
			},
		},
	})
	item.AddSteer("stop writing Rust")
	item.UpdateSteerAnswer("assistant-1", "switched")

	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), false))
	require.Contains(t, out, "stop writing Rust")
	require.Contains(t, out, "Switched to Go.")
}

// UpdateSteerAnswer without any recorded steer is a no-op.
func TestDispatchCardSteerAnswerWithoutSteer(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	require.False(t, item.UpdateSteerAnswer("assistant-1", "orphan"))
	require.Empty(t, item.Steers())
}
