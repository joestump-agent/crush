package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool             { return &b }
func intPtr(i int) *int                { return &i }
func intOrOffPtr(v IntOrOff) *IntOrOff { return &v }
func durationPtr(d Duration) *Duration { return &d }

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
// knob, and that an explicit 0 (or "off") threshold disables nudging,
// the same as enabled: false.
func TestResolveTodoEnforcementGlobal(t *testing.T) {
	t.Parallel()

	global := &TodoEnforcementConfig{
		Enabled:        boolPtr(false),
		NudgeThreshold: intOrOffPtr(9),
		HardGate:       boolPtr(true),
	}
	settings := ResolveTodoEnforcement(global, nil)
	assert.False(t, settings.Enabled)
	assert.Equal(t, 9, settings.NudgeThreshold)
	assert.True(t, settings.HardGate)

	settings = ResolveTodoEnforcement(&TodoEnforcementConfig{
		Enabled:        boolPtr(true),
		NudgeThreshold: intOrOffPtr(0),
	}, nil)
	assert.False(t, settings.Enabled, "an explicit 0 threshold disables nudging even with enabled: true")
	assert.Equal(t, 0, settings.NudgeThreshold)

	settings = ResolveTodoEnforcement(&TodoEnforcementConfig{
		NudgeThreshold: intOrOffPtr(0),
	}, nil)
	assert.False(t, settings.Enabled, "'off' (0) disables nudging")
}

// TestResolveTodoEnforcementAgentOverride pins the per agent type
// layering: only the fields the override sets replace the global ones.
func TestResolveTodoEnforcementAgentOverride(t *testing.T) {
	t.Parallel()

	global := &TodoEnforcementConfig{
		Enabled:        boolPtr(true),
		NudgeThreshold: intOrOffPtr(6),
		HardGate:       boolPtr(false),
	}

	// Only the threshold is overridden; enabled and hard gate carry over.
	settings := ResolveTodoEnforcement(global, &TodoEnforcementConfig{NudgeThreshold: intOrOffPtr(1)})
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

	global := &TodoEnforcementConfig{NudgeThreshold: intOrOffPtr(3)}
	agent := Agent{TodoEnforcement: &TodoEnforcementConfig{NudgeThreshold: intOrOffPtr(8)}}
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
}

// TestResolveTodoEnforcementKillKnobs pins the kill knobs' layering and
// their disable-by-zero semantics. A kill threshold above the old cap
// resolves verbatim: the ladder honors it, unclamped.
func TestResolveTodoEnforcementKillKnobs(t *testing.T) {
	t.Parallel()

	global := &TodoEnforcementConfig{
		KillAfterNudges: intOrOffPtr(3),
		StallWindow:     durationPtr(Duration(300 * time.Second)),
		HardTimeout:     durationPtr(Duration(1800 * time.Second)),
	}
	settings := ResolveTodoEnforcement(global, nil)
	assert.Equal(t, 3, settings.KillAfterNudges)
	assert.Equal(t, 300*time.Second, settings.StallWindow)
	assert.Equal(t, 1800*time.Second, settings.HardTimeout)

	// The per-agent override replaces each knob field by field.
	settings = ResolveTodoEnforcement(global, &TodoEnforcementConfig{
		KillAfterNudges: intOrOffPtr(1),
		StallWindow:     durationPtr(0),
	})
	assert.Equal(t, 1, settings.KillAfterNudges)
	assert.Zero(t, settings.StallWindow, "an explicit 0 must disable the stall window")
	assert.Equal(t, 1800*time.Second, settings.HardTimeout, "fields the override does not set carry over")

	// A threshold above the old nudge cap is honored as written.
	settings = ResolveTodoEnforcement(&TodoEnforcementConfig{KillAfterNudges: intOrOffPtr(5)}, nil)
	assert.Equal(t, 5, settings.KillAfterNudges)
}

// TestTodoEnforcementValidate pins the load-time rule: 0 or "off"
// disables a knob, positives are fine, and a negative value is a load
// error that names the path of the offending key.
func TestTodoEnforcementValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, (&TodoEnforcementConfig{
		NudgeThreshold:  intOrOffPtr(0),
		KillAfterNudges: intOrOffPtr(4),
		StallWindow:     durationPtr(0),
		HardTimeout:     durationPtr(Duration(5 * time.Minute)),
	}).Validate("options.todo_enforcement"))
	require.NoError(t, (&TodoEnforcementConfig{}).Validate("options.todo_enforcement"))
	var nilCfg *TodoEnforcementConfig
	require.NoError(t, nilCfg.Validate("options.todo_enforcement"))

	negatives := map[string]*TodoEnforcementConfig{
		"nudge_threshold":   {NudgeThreshold: intOrOffPtr(-1)},
		"kill_after_nudges": {KillAfterNudges: intOrOffPtr(-1)},
		"stall_window":      {StallWindow: durationPtr(Duration(-5 * time.Second))},
		"hard_timeout":      {HardTimeout: durationPtr(Duration(-1800 * time.Second))},
	}
	for field, cfg := range negatives {
		err := cfg.Validate("options.todo_enforcement")
		require.Error(t, err, field)
		assert.Contains(t, err.Error(), "options.todo_enforcement."+field+":", field)
		assert.Contains(t, err.Error(), "must not be negative", field)
	}

	// Whole-second negatives render as bare numbers, the way they are
	// written.
	err := (&TodoEnforcementConfig{HardTimeout: durationPtr(Duration(-1800 * time.Second))}).Validate("options.todo_enforcement")
	assert.Contains(t, err.Error(), "got -1800")
}

// TestTodoEnforcementConfigFromJSON pins the knob types' JSON forms from
// bytes: duration strings, "off", legacy integers, and the invalid
// strings that must fail.
func TestTodoEnforcementConfigFromJSON(t *testing.T) {
	t.Parallel()

	// Duration strings and "off" load; the values resolve to the same
	// windows as the legacy integer seconds.
	var fromString TodoEnforcementConfig
	require.NoError(t, json.Unmarshal([]byte(`{
		"stall_window": "5m",
		"hard_timeout": "1h30m"
	}`), &fromString))
	require.NotNil(t, fromString.StallWindow)
	assert.Equal(t, 5*time.Minute, time.Duration(*fromString.StallWindow))
	require.NotNil(t, fromString.HardTimeout)
	assert.Equal(t, 90*time.Minute, time.Duration(*fromString.HardTimeout))

	var fromOff TodoEnforcementConfig
	require.NoError(t, json.Unmarshal([]byte(`{
		"stall_window": "off",
		"hard_timeout": "off",
		"nudge_threshold": "off",
		"kill_after_nudges": "off"
	}`), &fromOff))
	require.NotNil(t, fromOff.StallWindow)
	assert.Zero(t, time.Duration(*fromOff.StallWindow))
	require.NotNil(t, fromOff.HardTimeout)
	assert.Zero(t, time.Duration(*fromOff.HardTimeout))
	require.NotNil(t, fromOff.NudgeThreshold)
	assert.Zero(t, *fromOff.NudgeThreshold)
	require.NotNil(t, fromOff.KillAfterNudges)
	assert.Zero(t, *fromOff.KillAfterNudges)

	// Legacy integer seconds still load, unchanged.
	var fromLegacy TodoEnforcementConfig
	require.NoError(t, json.Unmarshal([]byte(`{
		"stall_window": 300,
		"hard_timeout": 1800,
		"nudge_threshold": 8,
		"kill_after_nudges": 3
	}`), &fromLegacy))
	require.NotNil(t, fromLegacy.StallWindow)
	assert.Equal(t, 300*time.Second, time.Duration(*fromLegacy.StallWindow))
	require.NotNil(t, fromLegacy.HardTimeout)
	assert.Equal(t, 1800*time.Second, time.Duration(*fromLegacy.HardTimeout))
	require.NotNil(t, fromLegacy.NudgeThreshold)
	assert.Equal(t, IntOrOff(8), *fromLegacy.NudgeThreshold)
	require.NotNil(t, fromLegacy.KillAfterNudges)
	assert.Equal(t, IntOrOff(3), *fromLegacy.KillAfterNudges)

	// Settings resolve exactly as before for the legacy forms.
	settings := ResolveTodoEnforcement(&fromLegacy, nil)
	assert.Equal(t, 8, settings.NudgeThreshold)
	assert.Equal(t, 3, settings.KillAfterNudges)
	assert.Equal(t, 300*time.Second, settings.StallWindow)
	assert.Equal(t, 1800*time.Second, settings.HardTimeout)

	// Negative numbers still parse from bytes; the load-time Validate
	// is what rejects them (see TestTodoEnforcementValidate).
	var fromNegative TodoEnforcementConfig
	require.NoError(t, json.Unmarshal([]byte(`{"hard_timeout": -1800}`), &fromNegative))
	require.Error(t, fromNegative.Validate("options.todo_enforcement"))

	invalidDurations := []string{
		`{"stall_window": "5 minutes"}`,
		`{"hard_timeout": "soon"}`,
	}
	for _, raw := range invalidDurations {
		var cfg TodoEnforcementConfig
		err := json.Unmarshal([]byte(raw), &cfg)
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), "invalid duration", raw)
	}

	invalidCounts := []string{
		`{"nudge_threshold": "soon"}`,
		`{"kill_after_nudges": 2.5}`,
		`{"nudge_threshold": true}`,
	}
	for _, raw := range invalidCounts {
		var cfg TodoEnforcementConfig
		err := json.Unmarshal([]byte(raw), &cfg)
		require.Error(t, err, raw)
		assert.True(t,
			strings.Contains(err.Error(), "want an integer") || strings.Contains(err.Error(), "invalid value"),
			"got: %v", err,
		)
	}
}

// TestTodoEnforcementKnobRoundTrip pins the marshal forms the config
// writes back: 0 becomes "off", whole seconds stay bare numbers, and
// sub-second durations go out as duration strings.
func TestTodoEnforcementKnobRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   any
		want string
	}{
		{"duration off", Duration(0), `"off"`},
		{"duration whole seconds", Duration(300 * time.Second), `300`},
		{"duration minutes string", Duration(5 * time.Minute), `300`},
		{"duration sub-second", Duration(500 * time.Millisecond), `"500ms"`},
		{"int off", IntOrOff(0), `"off"`},
		{"int count", IntOrOff(8), `8`},
	}
	for _, tc := range cases {
		got, err := json.Marshal(tc.in)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, string(got), tc.name)
	}
}
