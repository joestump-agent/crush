package model

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/bubbles/v2/textarea"

	"github.com/charmbracelet/crush/internal/agent/tools"
	mcptools "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/ui/chat"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/joestump-agent/a2tea/event"
	"github.com/stretchr/testify/require"
	a2ui "github.com/tmc/a2ui"
)

// a2uiActionWorkspace stubs the workspace calls the A2UI action round-trip
// touches, recording a2ui_action / a2ui_error calls and serving canned
// responses.
type a2uiActionWorkspace struct {
	workspace.Workspace

	// toolCalls records (name, toolName) pairs in call order.
	toolCalls [][2]string
	// lastArgs records the args of the most recent call.
	lastArgs map[string]any
	// response is the canned CallMCPTool result.
	response workspace.MCPToolCallResult
	// err is returned from CallMCPTool when set.
	err error
}

func (w *a2uiActionWorkspace) AgentIsReady() bool     { return true }
func (w *a2uiActionWorkspace) Config() *config.Config { return nil }

func (w *a2uiActionWorkspace) AgentRun(_ context.Context, _, _ string, _ ...message.Attachment) error {
	return nil
}

func (w *a2uiActionWorkspace) CallMCPTool(_ context.Context, name, toolName string, args map[string]any) (workspace.MCPToolCallResult, error) {
	w.toolCalls = append(w.toolCalls, [2]string{name, toolName})
	w.lastArgs = args
	return w.response, w.err
}

// a2uiActionForm is an MCP-served surface with a submit button carrying a
// server-side action, plus a TextField whose value feeds the action context.
const a2uiActionForm = `{"version":"v0.9","updateComponents":{"surfaceId":"booking","components":[` +
	`{"component":"Card","id":"root","child":"col"},` +
	`{"component":"Column","id":"col","children":["start","btn-confirm"]},` +
	`{"component":"TextField","id":"start","label":"Start","value":"2026-03-20"},` +
	`{"component":"Button","id":"btn-confirm","child":"btn-confirm-t","action":{"event":{"name":"confirm_booking"}}},` +
	`{"component":"Text","id":"btn-confirm-t","text":"Confirm"}` +
	`]}}`

// newA2UIActionUI builds a UI whose chat holds a tool message item carrying
// an MCP-served A2UI surface with provenance registered to mcpName.
func newA2UIActionUI(t *testing.T, ws *a2uiActionWorkspace, mcpName string) *UI {
	t.Helper()
	com := common.DefaultCommon(ws)
	m := &UI{
		com:      com,
		status:   NewStatus(com, nil),
		chat:     NewChat(com, config.ScrollbarDefault),
		textarea: textarea.New(),
		state:    uiChat,
		focus:    uiFocusMain,
		width:    140,
		height:   45,
		session:  &session.Session{ID: "sess-1"},
	}

	meta, err := json.Marshal(tools.ReadMCPResourceResponseMetadata{
		A2UISurfaces:         []string{"<a2ui-json>" + a2uiActionForm + "</a2ui-json>"},
		MCPSurfaceProvenance: []string{mcpName},
	})
	require.NoError(t, err)

	item := chat.NewMCPToolMessageItem(com.Styles, message.ToolCall{
		ID:       "call-1",
		Name:     "mcp_" + mcpName + "_get_form",
		Input:    `{}`,
		Finished: true,
	}, &message.ToolResult{
		ToolCallID: "call-1",
		Content:    "form",
		Metadata:   string(meta),
	}, false)
	m.chat.AppendMessages(item)
	// Render once so the item builds its live surface models and registers
	// provenance.
	_ = item.RawRender(100)
	return m
}

func TestHandleA2UIButtonClickedMCPSurfaceRoundTrips(t *testing.T) {
	t.Parallel()

	ws := &a2uiActionWorkspace{
		response: workspace.MCPToolCallResult{Content: "Booking confirmed"},
	}
	m := newA2UIActionUI(t, ws, "recipe")
	cleanup := mcptools.SetToolsForTest("recipe", "a2ui_action")
	t.Cleanup(cleanup)

	cmd := m.handleA2UIButtonClicked(event.ButtonClicked{
		Source: event.Source{ComponentID: "btn-confirm", SurfaceID: "booking"},
		ID:     "btn-confirm",
		Action: &a2ui.EventAction{Name: "confirm_booking"},
	})
	require.NotNil(t, cmd, "an MCP surface with a2ui_action must round-trip")
	msg := cmd()
	result, ok := msg.(a2uiActionResultMsg)
	require.True(t, ok, "the cmd must produce an a2uiActionResultMsg")
	require.NoError(t, result.err)
	require.Equal(t, "booking", result.surfaceID)

	// The server got the action name and the surface's field values as the
	// context.
	require.Len(t, ws.toolCalls, 1)
	require.Equal(t, [2]string{"recipe", "a2ui_action"}, ws.toolCalls[0])
	require.Equal(t, "confirm_booking", ws.lastArgs["name"])
	ctx, ok := ws.lastArgs["context"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "2026-03-20", ctx["start"])

	// The surface is NOT retired — MCP surfaces are long-lived.
	_, found := m.chat.A2UISurfaceFieldValues("booking")
	require.True(t, found, "the surface must stay live after the round-trip")
}

func TestHandleA2UIButtonClickedMCPSurfaceWithoutActionToolFallsBack(t *testing.T) {
	t.Parallel()

	ws := &a2uiActionWorkspace{}
	m := newA2UIActionUI(t, ws, "recipe")
	// Server serves surfaces but NOT a2ui_action.
	cleanup := mcptools.SetToolsForTest("recipe", "some_other_tool")
	t.Cleanup(cleanup)

	cmd := m.handleA2UIButtonClicked(event.ButtonClicked{
		Source: event.Source{ComponentID: "btn-confirm", SurfaceID: "booking"},
		ID:     "btn-confirm",
		Action: &a2ui.EventAction{Name: "confirm_booking"},
	})
	require.NotNil(t, cmd, "fallback must start an agent turn")
	runCmdTree(cmd)

	require.Empty(t, ws.toolCalls, "no a2ui_action call without the tool")
}

func TestHandleA2UIActionResultAppliesSurfacePayload(t *testing.T) {
	t.Parallel()

	ws := &a2uiActionWorkspace{}
	m := newA2UIActionUI(t, ws, "recipe")
	cleanup := mcptools.SetToolsForTest("recipe", "a2ui_action")
	t.Cleanup(cleanup)

	// A response carrying an A2UI payload updates the surface in place.
	update := `{"version":"v0.9","updateComponents":{"surfaceId":"booking","components":[` +
		`{"component":"Text","id":"btn-confirm-t","text":"Booked!"}]}}`
	cmd := m.handleA2UIActionResult(a2uiActionResultMsg{
		surfaceID: "booking",
		mcpName:   "recipe",
		surfaces:  []string{update},
	})
	require.Nil(t, cmd)

	// The surface still exists after the in-place update.
	_, found := m.chat.A2UISurfaceFieldValues("booking")
	require.True(t, found)
}

func TestHandleA2UIActionResultErrorReports(t *testing.T) {
	t.Parallel()

	ws := &a2uiActionWorkspace{}
	m := newA2UIActionUI(t, ws, "recipe")

	cmd := m.handleA2UIActionResult(a2uiActionResultMsg{
		surfaceID: "booking",
		mcpName:   "recipe",
		err:       context.DeadlineExceeded,
	})
	require.NotNil(t, cmd, "an error must surface as a note")
}

func TestReportA2UIErrorCallsErrorTool(t *testing.T) {
	t.Parallel()

	ws := &a2uiActionWorkspace{}
	m := newA2UIActionUI(t, ws, "recipe")
	cleanup := mcptools.SetToolsForTest("recipe", "a2ui_error")
	t.Cleanup(cleanup)

	cmd := m.reportA2UIError("booking", "INVALID_JSON", "bad payload")
	require.NotNil(t, cmd)
	msg := cmd()
	require.Nil(t, msg, "a successful a2ui_error call produces no message")

	require.Len(t, ws.toolCalls, 1)
	require.Equal(t, [2]string{"recipe", "a2ui_error"}, ws.toolCalls[0])
	require.Equal(t, "INVALID_JSON", ws.lastArgs["code"])
	require.Equal(t, "booking", ws.lastArgs["surfaceId"])
}
