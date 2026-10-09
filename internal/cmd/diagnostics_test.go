package cmd

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
)

// A misconfigured agent definition reaches stderr as one line per
// diagnostic in `crush run` (#560); nothing is printed when the load was
// clean.
func TestWriteLoadDiagnostics(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	writeLoadDiagnostics(&out, nil)
	require.Empty(t, out.String(), "a clean load prints nothing")

	writeLoadDiagnostics(&out, []config.LoadDiagnostic{
		{Severity: config.DiagnosticWarning, Agent: "rev", Message: "cannot be dispatched: agents.rev.card: must use https; plain http is allowed only for loopback hosts"},
		{Severity: config.DiagnosticWarning, Agent: "worker", Message: "agents.worker.workspace: parsed but not honored by a builtin-runtime agent"},
	})
	require.Equal(t, strings.Join([]string{
		`warning: agent "rev": cannot be dispatched: agents.rev.card: must use https; plain http is allowed only for loopback hosts`,
		`warning: agent "worker": agents.worker.workspace: parsed but not honored by a builtin-runtime agent`,
		"",
	}, "\n"), out.String())
}

// The same diagnostics are replayed into the logger once it exists, so
// crush.log carries them for the TUI run that never sees stderr (#560).
func TestLogLoadDiagnostics(t *testing.T) {
	// Not parallel: the default logger is process-wide.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logLoadDiagnostics(nil)
	require.Empty(t, buf.String())

	logLoadDiagnostics([]config.LoadDiagnostic{
		{Severity: config.DiagnosticWarning, Agent: "rev", Message: "cannot be dispatched: agents.rev.card: a runtime a2a agent needs the URL of its Agent Card"},
	})
	out := buf.String()
	require.Equal(t, 1, strings.Count(out, "\n"), "one log line per diagnostic")
	require.Contains(t, out, "level=WARN")
	require.Contains(t, out, "Agent definition loaded with a diagnostic")
	require.Contains(t, out, "agent=rev")
	require.Contains(t, out, "agents.rev.card: a runtime a2a agent needs the URL of its Agent Card")
}
