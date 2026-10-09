package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOptionsGetDispatchMaxConcurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		options  *Options
		expected int
	}{
		{
			name:     "nil options",
			options:  nil,
			expected: DefaultDispatchMaxConcurrent,
		},
		{
			name:     "unset section",
			options:  &Options{},
			expected: DefaultDispatchMaxConcurrent,
		},
		{
			name:     "unset field",
			options:  &Options{Dispatch: &DispatchOptions{}},
			expected: DefaultDispatchMaxConcurrent,
		},
		{
			name:     "configured",
			options:  &Options{Dispatch: &DispatchOptions{MaxConcurrent: ptr(2)}},
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, tt.options.GetDispatchMaxConcurrent())
		})
	}
}

func TestOptionsDispatchMaxConcurrentFromJSON(t *testing.T) {
	t.Parallel()

	var cfg Config
	require.NoError(t, json.Unmarshal([]byte(`{"options":{"dispatch":{"max_concurrent":2}}}`), &cfg))
	require.NotNil(t, cfg.Options.Dispatch)
	require.Equal(t, 2, *cfg.Options.Dispatch.MaxConcurrent)
	require.Equal(t, 2, cfg.Options.GetDispatchMaxConcurrent())
}

func TestDispatchOptionsValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, (*DispatchOptions)(nil).Validate("options.dispatch"))
	require.NoError(t, (&DispatchOptions{}).Validate("options.dispatch"))
	require.NoError(t, (&DispatchOptions{MaxConcurrent: ptr(1)}).Validate("options.dispatch"))

	err := (&DispatchOptions{MaxConcurrent: ptr(0)}).Validate("options.dispatch")
	require.Error(t, err)
	require.Contains(t, err.Error(), "options.dispatch.max_concurrent")

	err = (&DispatchOptions{MaxConcurrent: ptr(-3)}).Validate("options.dispatch")
	require.Error(t, err)
	require.Contains(t, err.Error(), "options.dispatch.max_concurrent")
}

func TestOptionsGetDispatchDefaultAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		options  *Options
		expected string
	}{
		{
			name:     "nil options",
			options:  nil,
			expected: DefaultDispatchAgent,
		},
		{
			name:     "unset section",
			options:  &Options{},
			expected: DefaultDispatchAgent,
		},
		{
			name:     "unset field",
			options:  &Options{Dispatch: &DispatchOptions{}},
			expected: DefaultDispatchAgent,
		},
		{
			name:     "configured",
			options:  &Options{Dispatch: &DispatchOptions{DefaultAgent: "reviewer"}},
			expected: "reviewer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, tt.options.GetDispatchDefaultAgent())
		})
	}
}

func TestOptionsDispatchDefaultAgentFromJSON(t *testing.T) {
	t.Parallel()

	var cfg Config
	require.NoError(t, json.Unmarshal([]byte(`{"options":{"dispatch":{"default_agent":"reviewer"}}}`), &cfg))
	require.NotNil(t, cfg.Options.Dispatch)
	require.Equal(t, "reviewer", cfg.Options.Dispatch.DefaultAgent)
	require.Equal(t, "reviewer", cfg.Options.GetDispatchDefaultAgent())
}

// ValidateDispatchDefaultAgent runs against the resolved agents, so every
// case below populates Config.Agents the way SetupAgents would.
func TestValidateDispatchDefaultAgent(t *testing.T) {
	t.Parallel()

	agents := func(m map[string]Agent) map[string]Agent { return m }
	worker := Agent{ID: AgentWorker, Role: AgentRoleDispatch}

	tests := []struct {
		name    string
		options *Options
		agents  map[string]Agent
		wantErr string
	}{
		{
			name:    "unset default resolves to the worker",
			options: &Options{},
			agents:  agents(map[string]Agent{AgentWorker: worker}),
		},
		{
			name:    "valid user dispatch agent",
			options: &Options{Dispatch: &DispatchOptions{DefaultAgent: "reviewer"}},
			agents: agents(map[string]Agent{
				AgentWorker: worker,
				"reviewer":  {ID: "reviewer", Role: AgentRoleDispatch},
			}),
		},
		{
			name:    "unknown agent",
			options: &Options{Dispatch: &DispatchOptions{DefaultAgent: "nope"}},
			agents:  agents(map[string]Agent{AgentWorker: worker}),
			wantErr: `options.dispatch.default_agent: unknown agent "nope"`,
		},
		{
			name:    "non-dispatch agent",
			options: &Options{Dispatch: &DispatchOptions{DefaultAgent: "coder"}},
			agents: agents(map[string]Agent{
				AgentWorker: worker,
				"coder":     {ID: "coder", Role: AgentRoleMain},
			}),
			wantErr: `options.dispatch.default_agent: agent "coder" is a "main" agent, not a dispatch agent`,
		},
		{
			name:    "disabled agent",
			options: &Options{Dispatch: &DispatchOptions{DefaultAgent: "reviewer"}},
			agents: agents(map[string]Agent{
				AgentWorker: worker,
				"reviewer":  {ID: "reviewer", Role: AgentRoleDispatch, Disabled: true},
			}),
			wantErr: `options.dispatch.default_agent: agent "reviewer" is disabled`,
		},
		{
			name:    "an implicitly disabled worker is the opt-out, not an error",
			options: &Options{},
			agents:  agents(map[string]Agent{AgentWorker: {ID: AgentWorker, Role: AgentRoleDispatch, Disabled: true}}),
		},
		{
			// An external agent every dispatch would refuse is a
			// misconfiguration, not a warning (#560).
			name:    "unusable external agent",
			options: &Options{Dispatch: &DispatchOptions{DefaultAgent: "reviewer"}},
			agents: agents(map[string]Agent{
				AgentWorker: worker,
				"reviewer": {
					ID: "reviewer", Role: AgentRoleDispatch, Runtime: AgentRuntimeA2A,
					Unusable: "agents.reviewer.card: a runtime a2a agent needs the URL of its Agent Card",
				},
			}),
			wantErr: `options.dispatch.default_agent: agent "reviewer" cannot be dispatched: agents.reviewer.card: a runtime a2a agent needs the URL of its Agent Card`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &Config{Options: tt.options, Agents: tt.agents}
			err := c.ValidateDispatchDefaultAgent()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

// ValidateAgentToolsets runs the #433 load-time sanity checks against
// the resolved agents, so every case resolves definitions through
// SetupAgents first.
func TestValidateAgentToolsets(t *testing.T) {
	t.Parallel()

	validate := func(t *testing.T, agentsJSON string) error {
		t.Helper()
		var cfg Config
		cfg.Options = &Options{}
		if agentsJSON != "" {
			require.NoError(t, json.Unmarshal([]byte(agentsJSON), &cfg.AgentDefinitions))
		}
		cfg.SetupAgents()
		return cfg.ValidateAgentToolsets()
	}

	t.Run("the defaults all resolve to tools", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, validate(t, ""))
	})

	t.Run("an @read worker is the reviewer use case and loads", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, validate(t, `{"worker": {"tools": {"allow": ["@read"]}}}`))
	})

	t.Run("a disabled worker is the opt-out and loads", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, validate(t, `{"worker": {"disabled": true}}`))
	})

	t.Run("a tool-less coder fails at load", func(t *testing.T) {
		t.Parallel()
		err := validate(t, `{"coder": {"tools": {"allow": []}}}`)
		require.EqualError(t, err, "agents.coder: enabled agent resolves to no tools")
	})

	t.Run("a tool-less plan fails at load", func(t *testing.T) {
		t.Parallel()
		err := validate(t, `{"plan": {"tools": {"allow": []}}}`)
		require.EqualError(t, err, "agents.plan: enabled agent resolves to no tools")
	})

	t.Run("a tool-less dispatch agent fails at load", func(t *testing.T) {
		t.Parallel()
		err := validate(t, `{"reviewer": {"role": "dispatch", "tools": {"allow": []}}}`)
		require.EqualError(t, err, "agents.reviewer: enabled agent resolves to no tools")
	})

	t.Run("a disabled tool-less dispatch agent loads", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, validate(t, `{"reviewer": {"role": "dispatch", "disabled": true, "tools": {"allow": []}}}`))
	})

	t.Run("a tool-less task keeps its runtime behavior", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, validate(t, `{"task": {"tools": {"allow": []}}}`))
	})
}
