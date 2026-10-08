package shellconfig

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOption_Bool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug true
option progress false`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
}

func TestOption_BoolCaseInsensitive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug TRUE
option progress False
option metrics YES`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
	require.Equal(t, false, opts["disable_metrics"])
}

func TestOption_String(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option data-directory .crush
option notifications osc`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, ".crush", opts["data_directory"])
	require.Equal(t, "osc", opts["notifications"])
}

func TestOption_List(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option context-path .cursorrules
option context-path CRUSH.md`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["context_paths"].([]any)
	require.Len(t, paths, 2)
	require.Equal(t, ".cursorrules", paths[0])
	require.Equal(t, "CRUSH.md", paths[1])
}

func TestOption_Reset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./a
option skill-path ./b
option reset skill-path`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Empty(t, opts["skills_paths"].([]any))
}

func TestOption_ResetThenReadd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./inherited-a
option skill-path ./inherited-b
option reset skill-path
option skill-path ./mine`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["skills_paths"].([]any)
	require.Len(t, paths, 1)
	require.Equal(t, "./mine", paths[0])
}

func TestOption_ResetUnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset bogus-key`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_ResetNonListKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset debug`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not one")
}

func TestOption_UIUnknownKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui bogus true`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_UIWorkingDirFormat(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui working-dir-format {host}:{cwd}`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, "{host}:{cwd}", ui["working_dir_format"])
	_, err = LoadShellConfig(t.Context(), path, []byte(`option ui working-dir-format ""`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires a value")
}

func TestOption_UIWorkingDirFormatRequiresPlaceholder(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui working-dir-format "{pwd}"`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects at least one of")
}

func TestOption_UIExitBanner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui exit-banner compact`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, "compact", ui["exit_banner"])

	_, err = LoadShellConfig(t.Context(), path, []byte(`option ui exit-banner bogus`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects default, compact, or none")
}

func TestOption_BoolShorthand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug
option metrics`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["disable_metrics"])
}

func TestOption_InvertedBool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option metrics false`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["disable_metrics"])
}

func TestOption_UnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option bogus-key value`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_RequestTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option request-timeout 300
option request-timeout 0`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, float64(0), opts["request_timeout"])
}

func TestOption_RequestTimeoutInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option request-timeout soon`
	path := filepath.Join(dir, "crushrc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects a number of seconds")
}

// loadTodoEnforcement runs a crushrc script and returns the
// options.todo_enforcement block it produced.
func loadTodoEnforcement(t *testing.T, script string) map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crushrc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))
	return result["options"].(map[string]any)["todo_enforcement"].(map[string]any)
}

func TestOption_TodoNudge(t *testing.T) {
	t.Parallel()

	todo := loadTodoEnforcement(t, `option todo-nudge false
option todo-nudge on`)
	require.Equal(t, true, todo["enabled"], "last assignment wins, on is true")

	todo = loadTodoEnforcement(t, "option todo-nudge")
	require.Equal(t, true, todo["enabled"], "omitted value defaults to true")

	path := filepath.Join(t.TempDir(), "crushrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option todo-nudge maybe`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "todo-nudge")
}

func TestOption_TodoNudgeThreshold(t *testing.T) {
	t.Parallel()

	require.Equal(t, float64(3), loadTodoEnforcement(t, `option todo-nudge-threshold 3`)["nudge_threshold"])

	path := filepath.Join(t.TempDir(), "crushrc")
	for _, script := range []string{
		`option todo-nudge-threshold 0`,
		`option todo-nudge-threshold off`,
		`option todo-nudge-threshold -1`,
		`option todo-nudge-threshold soon`,
	} {
		_, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.Error(t, err, "script %q should fail", script)
		require.Contains(t, err.Error(), "todo-nudge-threshold", "script %q", script)
	}
}

func TestOption_TodoHardGate(t *testing.T) {
	t.Parallel()

	require.Equal(t, false, loadTodoEnforcement(t, `option todo-hard-gate off`)["hard_gate"])
	require.Equal(t, true, loadTodoEnforcement(t, `option todo-hard-gate on`)["hard_gate"])

	path := filepath.Join(t.TempDir(), "crushrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option todo-hard-gate maybe`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "todo-hard-gate")
}

func TestOption_TodoKillAfterNudges(t *testing.T) {
	t.Parallel()

	require.Equal(t, float64(3), loadTodoEnforcement(t, `option todo-kill-after-nudges 3`)["kill_after_nudges"])
	require.Equal(t, float64(0), loadTodoEnforcement(t, `option todo-kill-after-nudges off`)["kill_after_nudges"])
	require.Equal(t, float64(0), loadTodoEnforcement(t, `option todo-kill-after-nudges 0`)["kill_after_nudges"])

	path := filepath.Join(t.TempDir(), "crushrc")
	for _, script := range []string{
		`option todo-kill-after-nudges -1`,
		`option todo-kill-after-nudges soon`,
	} {
		_, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.Error(t, err, "script %q should fail", script)
		require.Contains(t, err.Error(), "todo-kill-after-nudges", "script %q", script)
	}
}

func TestOption_DispatchStall(t *testing.T) {
	t.Parallel()

	require.Equal(t, float64(300), loadTodoEnforcement(t, `option dispatch-stall 5m`)["stall_window"])
	require.Equal(t, float64(300), loadTodoEnforcement(t, `option dispatch-stall 300`)["stall_window"])
	require.Equal(t, float64(0), loadTodoEnforcement(t, `option dispatch-stall off`)["stall_window"])
	require.Equal(t, "500ms", loadTodoEnforcement(t, `option dispatch-stall 500ms`)["stall_window"],
		"sub-second durations keep their precision, the way a crush.json string does")

	path := filepath.Join(t.TempDir(), "crushrc")
	for _, script := range []string{
		`option dispatch-stall soon`,
		`option dispatch-stall -5m`,
		`option dispatch-stall -300`,
	} {
		_, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.Error(t, err, "script %q should fail", script)
		require.Contains(t, err.Error(), "dispatch-stall", "script %q", script)
	}
}

func TestOption_DispatchTimeout(t *testing.T) {
	t.Parallel()

	require.Equal(t, float64(3600), loadTodoEnforcement(t, `option dispatch-timeout 1h`)["hard_timeout"])
	require.Equal(t, float64(1800), loadTodoEnforcement(t, `option dispatch-timeout 1800`)["hard_timeout"])
	require.Equal(t, float64(0), loadTodoEnforcement(t, `option dispatch-timeout off`)["hard_timeout"])

	path := filepath.Join(t.TempDir(), "crushrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option dispatch-timeout -5m`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "dispatch-timeout")
}

func TestOption_TodoDispatchUnknownKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "crushrc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option dispatch-bogus 5m`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_DispatchMaxConcurrent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option dispatch-max-concurrent 3`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	dispatch := opts["dispatch"].(map[string]any)
	require.Equal(t, float64(3), dispatch["max_concurrent"])
}

func TestOption_DispatchMaxConcurrentInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crushrc")

	for _, script := range []string{
		"option dispatch-max-concurrent 0",
		"option dispatch-max-concurrent -1",
		"option dispatch-max-concurrent soon",
	} {
		_, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.Error(t, err, "script: %s", script)
		require.Contains(t, err.Error(), "dispatch-max-concurrent", "script: %s", script)
	}
}

func TestOption_DispatchDefaultAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option dispatch-default-agent reviewer`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	dispatch := opts["dispatch"].(map[string]any)
	require.Equal(t, "reviewer", dispatch["default_agent"])
}

func TestOption_DispatchDefaultAgentInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crushrc")

	for _, script := range []string{
		"option dispatch-default-agent",
	} {
		_, err := LoadShellConfig(t.Context(), path, []byte(script))
		require.Error(t, err, "script: %s", script)
		require.Contains(t, err.Error(), "dispatch-default-agent", "script: %s", script)
	}
}

// The a2a-* keys (#358) land under options.a2a with the JSON key names,
// so a crushrc and a crush.json describe the same listener.
func TestOption_A2A(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option a2a-listen 127.0.0.1:7443
option a2a-tls-cert certs/a2a.pem
option a2a-tls-key certs/a2a-key.pem
option a2a-client-ca certs/ca.pem`
	path := filepath.Join(dir, "crushrc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, map[string]any{
		"listen":    "127.0.0.1:7443",
		"tls_cert":  "certs/a2a.pem",
		"tls_key":   "certs/a2a-key.pem",
		"client_ca": "certs/ca.pem",
	}, opts["a2a"])
}

// Every a2a-* key needs a value; the plain-TCP refusal itself is a load
// error, checked against the merged config, not here.
func TestOption_A2ARequiresValue(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "crushrc")

	for _, key := range []string{"a2a-listen", "a2a-tls-cert", "a2a-tls-key", "a2a-client-ca"} {
		_, err := LoadShellConfig(t.Context(), path, []byte("option "+key))
		require.Error(t, err, "key: %s", key)
		require.Contains(t, err.Error(), key+" requires a value")
	}

	_, err := LoadShellConfig(t.Context(), path, []byte("option a2a-bogus x"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}
