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
func TestAgentDefinitionWarnings(t *testing.T) {
	logs := captureLogs(t)
	card := "https://example.com/agent.json"
	token := "$T"
	idle := Duration(0)
	builtinRuntime := AgentRuntimeBuiltin
	a2aRuntime := AgentRuntimeA2A

	warnUnhonoredFields("warn-builtin", AgentDefinition{
		Runtime:   &builtinRuntime,
		Card:      &card,
		Auth:      &AgentAuth{Token: &token},
		Transport: &AgentTransport{IdleTimeout: &idle},
	})
	out := logs.String()
	for _, field := range []string{"card", "auth", "transport"} {
		require.Contains(t, out, "agent=warn-builtin field="+field, "a builtin definition's %s is ignored and must warn", field)
	}

	warnUnhonoredFields("warn-a2a", AgentDefinition{
		Runtime:   &a2aRuntime,
		Card:      &card,
		Auth:      &AgentAuth{Token: &token},
		Transport: &AgentTransport{IdleTimeout: &idle},
	})
	require.NotContains(t, logs.String(), "agent=warn-a2a", "an a2a definition honors card, auth and transport")

	warnUnusableExternal("warn-unusable", "agents.warn-unusable.card: must use https")
	warnUnusableExternal("warn-unusable", "agents.warn-unusable.card: must use https")
	require.Equal(t, 1, bytes.Count([]byte(logs.String()), []byte("agent=warn-unusable")), "warned once per reason")
	require.Contains(t, logs.String(), "External agent definition cannot be dispatched")
}
