package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/charmbracelet/crush/internal/config"
)

// logLoadDiagnostics writes the diagnostics the config load and the
// command collected into the log (#560, #578). They were collected
// before the file logger existed — its path comes from the loaded
// config — so whatever was logged then went to the discard handler
// installed in Execute; this replays the set once crushlog.Setup has
// run.
func logLoadDiagnostics(diags []config.LoadDiagnostic) {
	for _, d := range diags {
		var args []any
		if d.Agent != "" {
			args = append(args, "agent", d.Agent)
		}
		args = append(args, "message", d.Message)
		slog.Log(context.Background(), d.Severity.Level(),
			"Config load produced a diagnostic", args...)
	}
}

// writeLoadDiagnostics prints the load diagnostics to w, one line each
// in the form `warning: agent "rev": …` (or `warning: …` for a
// diagnostic that names no agent), so a non-interactive `crush run`
// shows a misconfigured agent or allow-commands entry before the first
// dispatch (#560, #578). Nothing is written when there are none.
func writeLoadDiagnostics(w io.Writer, diags []config.LoadDiagnostic) {
	for _, d := range diags {
		_, _ = fmt.Fprintln(w, d.String())
	}
}
