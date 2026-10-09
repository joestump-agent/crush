package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
)

// DiagnosticSeverity grades a LoadDiagnostic. Only warnings exist today:
// a problem severe enough to be an error fails the load instead.
type DiagnosticSeverity string

// DiagnosticWarning marks a definition that loaded but will not behave
// the way it was written.
const DiagnosticWarning DiagnosticSeverity = "warning"

// Level maps the severity onto the slog level a replay logs it at.
func (s DiagnosticSeverity) Level() slog.Level {
	return slog.LevelWarn
}

// LoadDiagnostic is one finding about an agent definition that did not
// fail the load but that the user should hear about (#560): an
// external agent that cannot be dispatched, a field the runtime parses
// but does not honor, or kill knobs dropped from a non-dispatch agent.
//
// Load runs before the file logger exists — its path comes from the
// loaded config — so a warning logged during load reaches nobody. The
// diagnostics are kept on the ConfigStore instead and replayed by the
// command once the logger is up: to stderr in `crush run`, to crush.log
// and a startup notice in the TUI.
type LoadDiagnostic struct {
	Severity DiagnosticSeverity
	Agent    string
	Message  string
}

// String renders the diagnostic as the one-line form the CLI prints:
// `warning: agent "rev": cannot be dispatched: agents.rev.card: …`.
func (d LoadDiagnostic) String() string {
	return fmt.Sprintf("%s: agent %q: %s", d.Severity, d.Agent, d.Message)
}

// sortLoadDiagnostics puts the diagnostics in a stable order, by agent
// then message, so a replay reads the same whatever order the agent map
// was walked in.
func sortLoadDiagnostics(diags []LoadDiagnostic) {
	slices.SortFunc(diags, func(a, b LoadDiagnostic) int {
		return cmp.Or(cmp.Compare(a.Agent, b.Agent), cmp.Compare(a.Message, b.Message))
	})
}
