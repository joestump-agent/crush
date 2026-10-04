package config

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// TestResolveTodoEnforcementDefaults pins the ladder's defaults: nil
// everywhere means nudging on, threshold 4, hard gate off.
func TestResolveTodoEnforcementDefaults(t *testing.T) {
	t.Parallel()

	settings := ResolveTodoEnforcement(nil, nil)
	assert.True(t, settings.Enabled, "nudge injection is on by default")
	assert.Equal(t, 4, settings.NudgeThreshold)
	assert.False(t, settings.HardGate, "the hard gate is off by default")
}

// TestResolveTodoEnforcementGlobal pins that the global block sets every
// knob, and that an invalid (non-positive) threshold falls back to the
// default rather than nudging on the first tool call.
func TestResolveTodoEnforcementGlobal(t *testing.T) {
	t.Parallel()

	global := &TodoEnforcementConfig{
		Enabled:        boolPtr(false),
		NudgeThreshold: intPtr(9),
		HardGate:       boolPtr(true),
	}
	settings := ResolveTodoEnforcement(global, nil)
	assert.False(t, settings.Enabled)
	assert.Equal(t, 9, settings.NudgeThreshold)
	assert.True(t, settings.HardGate)

	settings = ResolveTodoEnforcement(&TodoEnforcementConfig{NudgeThreshold: intPtr(-1)}, nil)
	assert.Equal(t, 4, settings.NudgeThreshold, "a non-positive threshold must fall back to the default")
}

// TestResolveTodoEnforcementAgentOverride pins the per agent type
// layering: only the fields the override sets replace the global ones.
func TestResolveTodoEnforcementAgentOverride(t *testing.T) {
	t.Parallel()

	global := &TodoEnforcementConfig{
		Enabled:        boolPtr(true),
		NudgeThreshold: intPtr(6),
		HardGate:       boolPtr(false),
	}

	// Only the threshold is overridden; enabled and hard gate carry over.
	settings := ResolveTodoEnforcement(global, &TodoEnforcementConfig{NudgeThreshold: intPtr(1)})
	require.True(t, settings.Enabled)
	assert.Equal(t, 1, settings.NudgeThreshold)
	assert.False(t, settings.HardGate)

	// The agent turns the gate on and nudging off; the threshold carries
	// over.
	settings = ResolveTodoEnforcement(global, &TodoEnforcementConfig{
		Enabled:  boolPtr(false),
		HardGate: boolPtr(true),
	})
	assert.False(t, settings.Enabled)
	assert.Equal(t, 6, settings.NudgeThreshold)
	assert.True(t, settings.HardGate)
}

// TestAgentResolvedTodoEnforcement pins the Agent helper used by the
// coordinator's builders.
func TestAgentResolvedTodoEnforcement(t *testing.T) {
	t.Parallel()

	global := &TodoEnforcementConfig{NudgeThreshold: intPtr(3)}
	agent := Agent{TodoEnforcement: &TodoEnforcementConfig{NudgeThreshold: intPtr(8)}}
	assert.Equal(t, 8, agent.ResolvedTodoEnforcement(global).NudgeThreshold)

	bare := Agent{}
	assert.Equal(t, 3, bare.ResolvedTodoEnforcement(global).NudgeThreshold)
	assert.Equal(t, 4, bare.ResolvedTodoEnforcement(nil).NudgeThreshold)
}

// TestResolveTodoEnforcementKillDefaults pins the wander-kill knobs
// (#316): kill after two ignored nudges by default, stall window and
// hard timeout off.
func TestResolveTodoEnforcementKillDefaults(t *testing.T) {
	t.Parallel()

	settings := ResolveTodoEnforcement(nil, nil)
	assert.Equal(t, 2, settings.KillAfterNudges)
	assert.Zero(t, settings.StallWindow)
	assert.Zero(t, settings.HardTimeout)
	assert.Zero(t, settings.InactivityTimeout)
}

// TestResolveTodoEnforcementKillKnobs pins the kill knobs' layering,
// their disable-by-zero semantics, and the negative-value guards.
func TestResolveTodoEnforcementKillKnobs(t *testing.T) {
	t.Parallel()

	global := &TodoEnforcementConfig{
		KillAfterNudges:   intPtr(3),
		StallWindow:       intPtr(300),
		HardTimeout:       intPtr(1800),
		InactivityTimeout: intPtr(900),
	}
	settings := ResolveTodoEnforcement(global, nil)
	assert.Equal(t, 3, settings.KillAfterNudges)
	assert.Equal(t, 300*time.Second, settings.StallWindow)
	assert.Equal(t, 1800*time.Second, settings.HardTimeout)
	assert.Equal(t, 900*time.Second, settings.InactivityTimeout)

	// The per-agent override replaces each knob field by field.
	settings = ResolveTodoEnforcement(global, &TodoEnforcementConfig{
		KillAfterNudges:   intPtr(1),
		StallWindow:       intPtr(0),
		InactivityTimeout: intPtr(60),
	})
	assert.Equal(t, 1, settings.KillAfterNudges)
	assert.Zero(t, settings.StallWindow, "an explicit 0 must disable the stall window")
	assert.Equal(t, 1800*time.Second, settings.HardTimeout, "fields the override does not set carry over")
	assert.Equal(t, 60*time.Second, settings.InactivityTimeout)

	// Negative values are guards, not settings.
	settings = ResolveTodoEnforcement(&TodoEnforcementConfig{
		KillAfterNudges:   intPtr(-1),
		StallWindow:       intPtr(-5),
		HardTimeout:       intPtr(-5),
		InactivityTimeout: intPtr(-5),
	}, nil)
	assert.Equal(t, 0, settings.KillAfterNudges)
	assert.Zero(t, settings.StallWindow)
	assert.Zero(t, settings.HardTimeout)
	assert.Zero(t, settings.InactivityTimeout, "a non-positive inactivity timeout disables the backstop")
}

// TestTodoEnforcementInactivityTimeoutFromJSON pins the A2A backstop
// knob (#360) parsing from the config file: seconds in JSON, a resolved
// duration out, zero when the key is absent.
func TestTodoEnforcementInactivityTimeoutFromJSON(t *testing.T) {
	t.Parallel()

	var cfg Config
	require.NoError(t, json.Unmarshal([]byte(`{"options":{"todo_enforcement":{"inactivity_timeout":900}}}`), &cfg))
	require.NotNil(t, cfg.Options.TodoEnforcement)
	require.Equal(t, 900, *cfg.Options.TodoEnforcement.InactivityTimeout)
	assert.Equal(t, 15*time.Minute, ResolveTodoEnforcement(cfg.Options.TodoEnforcement, nil).InactivityTimeout)

	var bare Config
	require.NoError(t, json.Unmarshal([]byte(`{"options":{"todo_enforcement":{"stall_window":300}}}`), &bare))
	assert.Zero(t, ResolveTodoEnforcement(bare.Options.TodoEnforcement, nil).InactivityTimeout, "the backstop stays off unless the key is set")
}
