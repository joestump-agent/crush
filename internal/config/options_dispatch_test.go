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
