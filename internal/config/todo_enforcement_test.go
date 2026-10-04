package config

import (
	"testing"

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
