package model

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/util"
)

// loadDiagnosticsTTL keeps the startup warning on screen long enough to
// be read: it is the one notice of a problem the config load found,
// and the default status TTL is tuned for acknowledgements.
const loadDiagnosticsTTL = 30 * time.Second

// loadDiagnosticsNotice surfaces the agent-definition diagnostics the
// config load collected (#560) as one startup warning in the status
// bar. Load ran before the TUI and before the file logger existed, so
// without this the user would learn of a misconfigured agent only when
// a dispatch to it is refused. It runs as a command, not inline in
// Init, so it reads the workspace off the update loop like the other
// startup loaders. Nil when there is nothing to report.
func (m *UI) loadDiagnosticsNotice() tea.Msg {
	if m.com == nil || m.com.Workspace == nil {
		return nil
	}
	diags := m.com.Workspace.LoadDiagnostics()
	if len(diags) == 0 {
		return nil
	}
	return util.InfoMsg{
		Type: util.InfoTypeWarn,
		Msg:  loadDiagnosticsText(diags),
		TTL:  loadDiagnosticsTTL,
	}
}

// loadDiagnosticsText renders the status-bar line for diags, which must
// not be empty. The bar holds one line, so it carries the first
// diagnostic in full and counts the rest; every one of them is in
// crush.log by the time the TUI is up.
func loadDiagnosticsText(diags []config.LoadDiagnostic) string {
	first := diags[0]
	text := fmt.Sprintf("Agent %q: %s", first.Agent, first.Message)
	if rest := len(diags) - 1; rest > 0 {
		return fmt.Sprintf("%s (+%d more in crush.log)", text, rest)
	}
	return text + " (see crush.log)"
}
