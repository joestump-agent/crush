package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	mcptools "github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/commands"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/textarea"
	"github.com/charmbracelet/crush/internal/workspace"
	"github.com/stretchr/testify/require"
)

// mcpPromptWorkspace serves a fixed set of MCP prompts, standing in for
// connected servers that advertise them.
type mcpPromptWorkspace struct {
	workspace.Workspace
	prompts []commands.MCPPrompt
	dataDir string
	calls   int
}

func (w *mcpPromptWorkspace) AgentIsReady() bool   { return true }
func (w *mcpPromptWorkspace) AgentReadyErr() error { return nil }

// Init fans out to the other loaders too, and loadCustomCommands reads
// Options.DataDirectory, so this has to be a real (if empty) config.
func (w *mcpPromptWorkspace) Config() *config.Config {
	return &config.Config{Options: &config.Options{DataDirectory: w.dataDir}}
}

func (w *mcpPromptWorkspace) ListMCPPrompts(context.Context) ([]commands.MCPPrompt, error) {
	w.calls++
	return w.prompts, nil
}

// The rest of what Init touches, stubbed to their empty results.
func (w *mcpPromptWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo {
	return nil
}

func (w *mcpPromptWorkspace) MCPGetStates() map[string]mcptools.ClientInfo {
	return nil
}

// TestInitLoadsMCPPrompts pins MCP prompt loading into startup.
//
// m.mcpPrompts used to be populated from exactly one place — the
// mcp.EventStateChanged branch of the pubsub handler. Servers publish that
// event as they connect, which normally happens before this model is
// subscribed to the bus, so the event was missed and the field stayed nil for
// the entire session. The commands dialog then hid its MCP Prompts category
// outright, because commandsRadioView renders an empty string when there are
// no user commands and no prompts, and every server's prompts were
// unreachable from the UI even though the whole pipeline behind them worked.
func TestInitLoadsMCPPrompts(t *testing.T) {
	t.Parallel()

	ws := &mcpPromptWorkspace{
		dataDir: t.TempDir(),
		prompts: []commands.MCPPrompt{
			{ID: "cairn:run_capture", PromptID: "run_capture", ClientID: "cairn"},
			{ID: "gitea:review", PromptID: "review", ClientID: "gitea"},
		},
	}
	com := common.DefaultCommon(ws)
	m := &UI{
		com:      com,
		status:   NewStatus(com, nil),
		chat:     NewChat(com, config.ScrollbarDefault),
		textarea: textarea.New(),
		state:    uiChat,
		focus:    uiFocusEditor,
		width:    140,
		height:   45,
	}

	require.Empty(t, m.mcpPrompts, "precondition: nothing loaded before Init")

	// Init must schedule the load without depending on any MCP event.
	runInitCmds(t, m, m.Init())

	require.Positive(t, ws.calls, "Init must ask the workspace for MCP prompts")
	require.Len(t, m.mcpPrompts, 2,
		"prompts must be available without waiting for an mcp.EventStateChanged")
}

// runInitCmds executes the commands Init returned and feeds any
// mcpPromptsLoadedMsg back through Update, mirroring the runtime loop.
func runInitCmds(t *testing.T, m *UI, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		// Init fans out to every startup loader, and the others want a far
		// fuller workspace than this fixture builds. Isolate each command so
		// a sibling's missing stub cannot mask the one assertion here —
		// which is that Init asks for MCP prompts at all.
		var msg tea.Msg
		func() {
			defer func() { _ = recover() }()
			msg = c()
		}()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				run(sub)
			}
			return
		}
		if loaded, ok := msg.(mcpPromptsLoadedMsg); ok {
			m.mcpPrompts = loaded.Prompts
		}
	}
	run(cmd)
}
