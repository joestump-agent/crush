package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/config"
)

// contextFilesTemplate renders only the project context files' contents.
const contextFilesTemplate = `{{range .ContextFiles}}{{.Content}};{{end}}`

func TestWithContextPaths(t *testing.T) {
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "crush"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "crush"))

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("global"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "NOTES.md"), []byte("agent"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "crush.json"),
		[]byte(`{
  "options": {"disable_default_providers": true, "disable_provider_auto_update": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}}
}`), 0o644))
	store, err := config.Init(dir, "", false)
	require.NoError(t, err)

	render := func(opts ...Option) string {
		t.Helper()
		p, err := NewPrompt("context", contextFilesTemplate, append([]Option{WithWorkingDir(dir)}, opts...)...)
		require.NoError(t, err)
		out, err := p.Build(t.Context(), "provider", "model", store)
		require.NoError(t, err)
		return out
	}

	// The default reads options.context_paths, which includes AGENTS.md.
	require.Equal(t, "global;", render())
	// An agent's context paths (#432) replace the global ones.
	require.Equal(t, "agent;", render(WithContextPaths([]string{"NOTES.md"})))
	// An empty list keeps the global paths.
	require.Equal(t, "global;", render(WithContextPaths(nil)))
}
