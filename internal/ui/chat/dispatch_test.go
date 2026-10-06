package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/ui/anim"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
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
// a live snapshot and without a terminal record (#410) — and never
// spins, because nothing will advance the card again.
func TestDispatchCardStaleHandleWithoutTerminalRecordIsStatic(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), false))

	require.Contains(t, out, "working")
	require.False(t, item.Spinning())
	require.Equal(t, "msg$$call-dispatch-1", item.DispatchSessionID())
}

// A restarted or client/server card has only the persisted tool result:
// Content still holds the running handle, but the run stamped the
// terminal DispatchResult into Metadata (#410). The terminal record
// wins, so the card shows the durable findings instead of "working".
func TestDispatchCardPrefersTerminalMetadata(t *testing.T) {
	t.Parallel()

	terminal := dispatch.DispatchResult{
		DispatchID:  "dispatch-1",
		Branch:      "crush-dispatch-dispatch-1",
		SessionID:   "msg$$call-dispatch-1",
		Status:      dispatch.StatusCompleted,
		KeyFindings: "Added validation and two tests.",
	}
	b, err := json.Marshal(terminal)
	require.NoError(t, err)

	result := runningHandle(t, dispatch.StatusRunning)
	result.Metadata = string(b)

	item := newDispatchItem(t, result)
	out := dispatchTestRender(t, item, dispatchToolOpts(result, false))

	require.Contains(t, out, "complete")
	require.Contains(t, out, "Added validation and two tests.")
	require.NotContains(t, out, "working")
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
// renders as a steer with the agent's streaming answer beneath it.
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
}

// RebuildSteers rebuilds the steer log from the persisted child
// transcript (#410): Steer-marked user messages become steers — plain
// user messages (the dispatch's initial prompt, todo nudges) never do —
// and each assistant message answers the latest steer so far.
func TestDispatchCardRebuildSteers(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	// Live-recorded state is replaced wholesale by the rebuild.
	item.AddSteer("stale live steer")

	msg := func(role message.MessageRole, id, text string, steer bool) message.Message {
		part := message.TextContent{Text: text, Steer: steer}
		return message.Message{ID: id, Role: role, Parts: []message.ContentPart{part}}
	}

	msgs := []message.Message{
		msg(message.User, "child-prompt", "implement the login form with validation", false),
		msg(message.Assistant, "child-a1", "on it", false),
		msg(message.User, "nudge-1", "todo reminder", false),
		msg(message.User, "steer-1", "stop writing Rust and use Go", true),
		msg(message.Assistant, "child-a2", "switched to Go", false),
		msg(message.User, "steer-2", "also run the linter", true),
		msg(message.Assistant, "child-a3", "lint clean", false),
		msg(message.Assistant, "child-a4", "linter passes everywhere", false),
	}

	item.RebuildSteers(msgs)

	require.Len(t, item.Steers(), 2)
	require.Equal(t, "stop writing Rust and use Go", item.Steers()[0].Text)
	require.Equal(t, "switched to Go", item.Steers()[0].Response)
	require.Equal(t, "child-a2", item.Steers()[0].ResponseMessageID)
	// child-a3 and child-a4 both advance the latest steer; the last one
	// wins, matching the live retarget-latest semantics.
	require.Equal(t, "also run the linter", item.Steers()[1].Text)
	require.Equal(t, "linter passes everywhere", item.Steers()[1].Response)
	require.Equal(t, "child-a4", item.Steers()[1].ResponseMessageID)

	out := dispatchTestRender(t, item, dispatchToolOpts(runningHandle(t, dispatch.StatusRunning), true))
	require.Contains(t, out, "stop writing Rust and use Go")
	require.Contains(t, out, "linter passes everywhere")
	// The prompt still renders once, as the card's Task line — but the
	// unmarked nudge never appears as a steer.
	require.NotContains(t, out, "todo reminder")
}

// RebuildSteers with no Steer-marked messages yields an empty steer
// log: an untouched dispatch rebuilds to a bare card.
func TestDispatchCardRebuildSteersEmpty(t *testing.T) {
	t.Parallel()

	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	item.RebuildSteers([]message.Message{})
	require.Empty(t, item.Steers())
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

// The status line shows the @handle (#313): from the live snapshot when
// one has arrived, else from the persisted running handle.
func TestDispatchCardShowsHandle(t *testing.T) {
	t.Parallel()

	result := runningHandle(t, dispatch.StatusRunning)
	handle := dispatch.DispatchResult{DispatchID: "dispatch-1", Handle: "tester", SessionID: "msg$$call-dispatch-1", Status: dispatch.StatusRunning}
	b, err := json.Marshal(handle)
	require.NoError(t, err)
	result.Content = string(b)

	item := newDispatchItem(t, result)
	out := dispatchTestRender(t, item, dispatchToolOpts(result, false))
	require.Contains(t, out, "tester")

	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{ID: "dispatch-1", SessionID: "msg$$call-dispatch-1", Handle: "tester-2", Status: dispatch.StatusRunning},
	})
	out = dispatchTestRender(t, item, dispatchToolOpts(result, false))
	require.Contains(t, out, "tester-2", "the live snapshot's handle wins over the persisted one")
}

// The card label distinguishes a user cancel (#373) from the other kill
// reasons at a glance: "canceled" only for the user-cancel reason,
// "killed" for every other kill, and the existing labels otherwise.
func TestDispatchStateLabelCanceledVsKilled(t *testing.T) {
	tests := []struct {
		status       dispatch.Status
		killedReason string
		want         string
	}{
		{dispatch.StatusProvisioned, "", "queued"},
		{dispatch.StatusRunning, "", "working"},
		{dispatch.StatusCompleted, "", "complete"},
		{dispatch.StatusFailed, "", "failed"},
		{dispatch.StatusKilled, dispatch.ReasonCanceled, "canceled"},
		{dispatch.StatusKilled, dispatch.ReasonHardTimeout, "killed"},
		{dispatch.StatusKilled, dispatch.ReasonStalledTodos, "killed"},
		{dispatch.StatusKilled, "", "killed"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, dispatchStateLabel(tt.status, tt.killedReason),
			"status %q reason %q", tt.status, tt.killedReason)
	}
}

// A killed card whose terminal payload carries the user-cancel reason
// renders "canceled" in the status line (#373); the other kills keep
// "killed".
func TestDispatchCardCanceledLabel(t *testing.T) {
	tests := []struct {
		name         string
		killedReason string
		want         string
	}{
		{"user cancel", dispatch.ReasonCanceled, "canceled ·"},
		{"other kill", dispatch.ReasonHardTimeout, "killed ·"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
			item.SetDispatchSnapshot(dispatch.TodoSnapshot{
				Entry: dispatch.Entry{
					ID:        "dispatch-1",
					SessionID: "msg$$call-dispatch-1",
					Status:    dispatch.StatusKilled,
					StartedAt: time.Now().Add(-30 * time.Second),
					Result: &dispatch.DispatchResult{
						Status:       dispatch.StatusKilled,
						KilledReason: tt.killedReason,
					},
				},
			})
			out := dispatchTestRender(t, item, dispatchToolOpts(nil, false))
			require.Contains(t, out, tt.want)
		})
	}
}

// newSteerCard builds a live running dispatch card with one steer,
// optionally in its expanded state (#411).
func newSteerCard(t *testing.T, steerText string, expanded bool) *DispatchToolMessageItem {
	t.Helper()
	item := newDispatchItem(t, runningHandle(t, dispatch.StatusRunning))
	item.SetDispatchSnapshot(dispatch.TodoSnapshot{
		Entry: dispatch.Entry{
			ID:        "dispatch-1",
			SessionID: "msg$$call-dispatch-1",
			Status:    dispatch.StatusRunning,
		},
	})
	item.AddSteer(steerText)
	if expanded {
		item.ToggleExpanded()
	}
	return item
}

// noSpace keeps only the non-space runes so wrap positions cannot hide
// text: if the whole steer survives wrapping, this comparison holds.
func noSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// Steer lines are width-bound (#411): across render widths and steer
// shapes, every line of the card stays inside the width, the whole steer
// text is visible across the wrapped lines, and a short steer stays on
// one line. Collapsed cards fold steer newlines into spaces; expanded
// cards keep them.
func TestDispatchCardSteerWidthBound(t *testing.T) {
	t.Parallel()

	shapes := []struct {
		name string
		text string
	}{
		{"short", "stop writing Rust"},
		{"medium", "stop writing Rust and use Go"},
		{"long", "refactor the streaming answer path so that token deltas never re-render the markdown body and the steer conversation stays readable"},
		{"multiline", "first line of the steer\nsecond line of the steer\nthird line of the steer"},
		{"widerunes", "日本語の長いステアテキストは幅の広いランダムで正しく折り返されなければならない"},
		{"longword", "unbreakablylongwordwithnospaceatallthatgoesonandonandonandonandon"},
	}
	for _, expanded := range []bool{false, true} {
		for _, width := range []int{40, 60, 100} {
			for _, shape := range shapes {
				t.Run(fmt.Sprintf("w%03d/exp=%v/%s", width, expanded, shape.name), func(t *testing.T) {
					t.Parallel()

					item := newSteerCard(t, shape.text, expanded)
					out := ansi.Strip(item.Render(width))

					var widest int
					var shortOnOneLine bool
					for line := range strings.SplitSeq(out, "\n") {
						widest = max(widest, ansi.StringWidth(line))
						if strings.Contains(line, shape.text) {
							shortOnOneLine = true
						}
					}
					require.LessOrEqual(t, widest, width, "card at width %d:\n%s", width, out)

					// Nothing is truncated: the whole steer text is
					// present, wrap positions aside.
					want := noSpace(shape.text)
					if !expanded {
						want = noSpace(strings.ReplaceAll(shape.text, "\n", " "))
					}
					require.Contains(t, noSpace(out), want, "card at width %d:\n%s", width, out)

					if shape.name == "short" {
						require.True(t, shortOnOneLine, "short steer must stay on one line:\n%s", out)
					}
				})
			}
		}
	}
}

// A long steer at width 60 pins the exact wrap shape (#411): the steer
// line breaks at the card edge, continuation lines indent under the
// text, and the answer follows. Regenerate the golden file with
// `go test ./internal/ui/chat -update`.
func TestDispatchCardSteerWidthGolden(t *testing.T) {
	t.Parallel()

	item := newSteerCard(t,
		"refactor the streaming answer path so that token deltas never re-render the markdown body", false)
	item.UpdateSteerAnswer("assistant-1", "on it, splitting the render pass")

	golden.RequireEqual(t, []byte(ansi.Strip(item.Render(60))))
}
