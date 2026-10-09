package config

import (
	"bytes"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// lockedBuffer is a bytes.Buffer safe for the logger's writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs routes the default logger into a buffer for the test. Not
// for parallel tests: the default logger is process-wide.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	var buf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A builtin-runtime definition that sets card, auth, or transport has
// them ignored, and says so (#434); a runtime a2a one honors them and
// stays quiet. An unusable external definition warns with its reason.
// Each finding is a LoadDiagnostic naming the agent (#560), and is
// logged once per process so a reload does not repeat it.
func TestAgentDefinitionWarnings(t *testing.T) {
	logs := captureLogs(t)
	card := "https://example.com/agent.json"
	token := "$T"
	idle := Duration(0)
	builtinRuntime := AgentRuntimeBuiltin
	a2aRuntime := AgentRuntimeA2A

	diags := unhonoredFieldDiagnostics("warn-builtin", AgentDefinition{
		Runtime:   &builtinRuntime,
		Card:      &card,
		Auth:      &AgentAuth{Token: &token},
		Transport: &AgentTransport{IdleTimeout: &idle},
	})
	require.Len(t, diags, 3)
	out := logs.String()
	for i, field := range []string{"card", "auth", "transport"} {
		require.Contains(t, out, "agent=warn-builtin field="+field, "a builtin definition's %s is ignored and must warn", field)
		require.Equal(t, DiagnosticWarning, diags[i].Severity)
		require.Equal(t, "warn-builtin", diags[i].Agent)
		require.Equal(t, "agents.warn-builtin."+field+": parsed but not honored by a builtin-runtime agent", diags[i].Message)
	}

	require.Empty(t, unhonoredFieldDiagnostics("warn-a2a", AgentDefinition{
		Runtime:   &a2aRuntime,
		Card:      &card,
		Auth:      &AgentAuth{Token: &token},
		Transport: &AgentTransport{IdleTimeout: &idle},
	}), "an a2a definition honors card, auth and transport")
	require.NotContains(t, logs.String(), "agent=warn-a2a")

	// The same finding twice is one log line but a diagnostic each time:
	// the log is deduped for the life of the process, the diagnostics
	// describe the config that was just resolved.
	for range 2 {
		diag, ok := unusableExternalDiagnostic("warn-unusable", "agents.warn-unusable.card: must use https")
		require.True(t, ok)
		require.Equal(t, LoadDiagnostic{
			Severity: DiagnosticWarning,
			Agent:    "warn-unusable",
			Message:  "cannot be dispatched: agents.warn-unusable.card: must use https",
		}, diag)
	}
	require.Equal(t, 1, bytes.Count([]byte(logs.String()), []byte("agent=warn-unusable")), "warned once per reason")
	require.Contains(t, logs.String(), "External agent definition cannot be dispatched")

	_, ok := unusableExternalDiagnostic("warn-usable", "")
	require.False(t, ok, "a usable external agent raises nothing")
}

// A kill knob set through the todo_enforcement alias on a non-dispatch
// agent is dropped (#402) and reported as a diagnostic that names the
// agent and the field (#560).
func TestAgentDefinitionWarnings_KillKnobsOnNonDispatchAgent(t *testing.T) {
	nudges := IntOrOff(2)
	diags := unhonoredFieldDiagnostics(AgentCoder, AgentDefinition{
		TodoEnforcement: &TodoEnforcementConfig{KillAfterNudges: &nudges},
	})
	require.Equal(t, []LoadDiagnostic{{
		Severity: DiagnosticWarning,
		Agent:    AgentCoder,
		Message:  "agents.coder.todo_enforcement: kill thresholds apply to dispatch agents only and are ignored",
	}}, diags)

	dispatch := AgentRoleDispatch
	require.Empty(t, unhonoredFieldDiagnostics("kill-dispatch", AgentDefinition{
		Role:            &dispatch,
		TodoEnforcement: &TodoEnforcementConfig{KillAfterNudges: &nudges},
	}), "a dispatch agent honors its kill knobs")
}

// LoadDiagnostic.String is the line `crush run` prints.
func TestLoadDiagnosticString(t *testing.T) {
	t.Parallel()
	d := LoadDiagnostic{Severity: DiagnosticWarning, Agent: "rev", Message: "cannot be dispatched: agents.rev.card: must use https"}
	require.Equal(t, `warning: agent "rev": cannot be dispatched: agents.rev.card: must use https`, d.String())
	require.Equal(t, slog.LevelWarn, d.Severity.Level())
}
