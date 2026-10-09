package cmd

import (
	"bytes"
	"log/slog"
	"path/filepath"
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
// A diagnostic that names no agent logs no agent attribute (#578).
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
		{Severity: config.DiagnosticWarning, Message: "ignoring allow-commands entries not in the default banned list: shh"},
	})
	out := buf.String()
	require.Equal(t, 2, strings.Count(out, "\n"), "one log line per diagnostic")
	require.Contains(t, out, "level=WARN")
	require.Contains(t, out, "Config load produced a diagnostic")
	require.Contains(t, out, "agent=rev")
	require.Contains(t, out, "agents.rev.card: a runtime a2a agent needs the URL of its Agent Card")
	require.Contains(t, out, "ignoring allow-commands entries not in the default banned list: shh")
	require.NotContains(t, out, `agent=""`)
}

// isolateEnv points the config load at an empty per-test home, so a
// test store never reads the machine's real config.
func isolateEnv(t *testing.T) (workDir, dataDir string) {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "crush"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "crush"))
	return t.TempDir(), t.TempDir()
}

// An allow-commands entry that is not in the default banned list is
// recorded as a load diagnostic (#578): this ran before crushlog.Setup
// when it still went to the discard handler, so the user believed the
// command was unblocked while the blocklist still applied. The replays
// that run once the logger exists reach it instead.
func TestWarnUnknownAllowedCommands(t *testing.T) {
	// Not parallel: isolateEnv uses t.Setenv.
	workDir, dataDir := isolateEnv(t)
	store, err := config.Init(workDir, dataDir, false)
	require.NoError(t, err)

	warnUnknownAllowedCommands(store, []string{"ssh", "shh"}, []string{"curl"})
	diags := store.LoadDiagnostics()
	require.Len(t, diags, 1)
	require.Empty(t, diags[0].Agent, "an allow-commands entry names no agent")
	require.Equal(t, config.DiagnosticWarning, diags[0].Severity)
	require.Equal(t, "ignoring allow-commands entries not in the default banned list: shh", diags[0].Message)

	// The `crush run` stderr replay prints it as one warning line.
	var out bytes.Buffer
	writeLoadDiagnostics(&out, store.LoadDiagnostics())
	require.Equal(t, "warning: ignoring allow-commands entries not in the default banned list: shh\n", out.String())
}

// Entries that are in the default banned list subtract from the block
// list, so they record nothing (#578).
func TestWarnKnownAllowedCommandsSilent(t *testing.T) {
	// Not parallel: isolateEnv uses t.Setenv.
	workDir, dataDir := isolateEnv(t)
	store, err := config.Init(workDir, dataDir, false)
	require.NoError(t, err)

	warnUnknownAllowedCommands(store, []string{"ssh"}, []string{"curl"})
	require.Empty(t, store.LoadDiagnostics())
}
