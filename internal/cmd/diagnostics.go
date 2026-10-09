package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/charmbracelet/crush/internal/config"
)

// logLoadDiagnostics writes the agent-definition diagnostics config.Load
// collected into the log (#560). Load runs before the file logger exists
// — its path comes from the loaded config — so whatever it logged went
// to the discard handler installed in Execute; this replays the
// collected set once crushlog.Setup has run.
func logLoadDiagnostics(diags []config.LoadDiagnostic) {
	for _, d := range diags {
		slog.Log(context.Background(), d.Severity.Level(),
			"Agent definition loaded with a diagnostic", "agent", d.Agent, "message", d.Message)
	}
}

// writeLoadDiagnostics prints the agent-definition diagnostics to w, one
// line each in the form `warning: agent "rev": …`, so a non-interactive
// `crush run` shows a misconfigured agent before the model's first
// dispatch is refused (#560). Nothing is written when there are none.
func writeLoadDiagnostics(w io.Writer, diags []config.LoadDiagnostic) {
	for _, d := range diags {
		_, _ = fmt.Fprintln(w, d.String())
	}
}
