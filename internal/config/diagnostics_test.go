package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
)

// LoadDiagnostic renders an agent diagnostic with the agent named and
// a diagnostic about the config as a whole, such as an allow-commands
// entry, as a plain warning line (#560, #578).
func TestLoadDiagnosticString(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		`warning: agent "rev": cannot be dispatched: agents.rev.card: must use https`,
		config.LoadDiagnostic{
			Severity: config.DiagnosticWarning,
			Agent:    "rev",
			Message:  "cannot be dispatched: agents.rev.card: must use https",
		}.String())

	require.Equal(t,
		"warning: ignoring allow-commands entries not in the default banned list: shh",
		config.LoadDiagnostic{
			Severity: config.DiagnosticWarning,
			Message:  "ignoring allow-commands entries not in the default banned list: shh",
		}.String())
}

// A diagnostic the command recorded rides with the load-raised ones in
// LoadDiagnostics, in the stable order (#578).
func TestAddLoadDiagnostic(t *testing.T) {
	workDir, dataDir := isolateReloadEnv(t)
	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	require.Empty(t, store.LoadDiagnostics())

	store.AddLoadDiagnostic(config.LoadDiagnostic{
		Severity: config.DiagnosticWarning,
		Message:  "ignoring allow-commands entries not in the default banned list: shh",
	})
	diags := store.LoadDiagnostics()
	require.Len(t, diags, 1)
	require.Equal(t, config.DiagnosticWarning, diags[0].Severity)
	require.Empty(t, diags[0].Agent)
	require.Equal(t, "ignoring allow-commands entries not in the default banned list: shh", diags[0].Message)

	store.AddLoadDiagnostic(config.LoadDiagnostic{
		Severity: config.DiagnosticWarning,
		Agent:    "rev",
		Message:  "cannot be dispatched",
	})
	again := store.LoadDiagnostics()
	require.Len(t, again, 2)
	// A diagnostic that names no agent sorts before the agent ones.
	require.Empty(t, again[0].Agent)
	require.Equal(t, "rev", again[1].Agent)
}
