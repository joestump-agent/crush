package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestTodoEnforcementFromCrushrc verifies that the todo-*/dispatch-* option
// keys (#403) load through the config loader and resolve to the settings a
// crushrc-only user asked for.
func TestTodoEnforcementFromCrushrc(t *testing.T) {
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))

	workDir := t.TempDir()
	dataDir := t.TempDir()
	script := `option todo-nudge false
option todo-nudge-threshold 3
option todo-hard-gate on
option todo-kill-after-nudges 5
option dispatch-stall 5m
option dispatch-timeout 1h`
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "crushrc"), []byte(script), 0o644))

	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)

	settings := config.ResolveTodoEnforcement(store.Config().Options.TodoEnforcement, nil)
	require.False(t, settings.Enabled)
	require.Equal(t, 3, settings.NudgeThreshold)
	require.True(t, settings.HardGate)
	require.Equal(t, 5, settings.KillAfterNudges)
	require.Equal(t, 5*time.Minute, settings.StallWindow)
	require.Equal(t, time.Hour, settings.HardTimeout)
}

// TestTodoEnforcementCrushrcMatchesJson verifies that a crushrc using the
// todo-*/dispatch-* keys loads into the same resolved settings as the
// equivalent crush.json.
func TestTodoEnforcementCrushrcMatchesJson(t *testing.T) {
	loadSettings := func(name, content string) config.TodoEnforcementSettings {
		t.Helper()
		isolated := t.TempDir()
		t.Setenv("HOME", isolated)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
		t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))

		workDir := t.TempDir()
		dataDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o644))

		store, err := config.Load(workDir, dataDir, false)
		require.NoError(t, err)
		return config.ResolveTodoEnforcement(store.Config().Options.TodoEnforcement, nil)
	}

	rc := loadSettings("crushrc", `option todo-nudge false
option todo-nudge-threshold 3
option todo-hard-gate on
option todo-kill-after-nudges 5
option dispatch-stall 500ms
option dispatch-timeout 1h`)
	json := loadSettings("crush.json", `{
	"options": {
		"todo_enforcement": {
			"enabled": false,
			"nudge_threshold": 3,
			"hard_gate": true,
			"kill_after_nudges": 5,
			"stall_window": "500ms",
			"hard_timeout": 3600
		}
	}
}`)
	require.Equal(t, json, rc)
}
