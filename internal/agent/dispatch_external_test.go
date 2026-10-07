package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// A Secret never prints its value (#434): not through fmt's verbs, not
// through either slog handler, not through encoding/json — on its own or
// inside the params struct that carries it.
func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()
	const value = "s3cr3t-bearer-value"
	secret := NewSecret(value)
	require.Equal(t, value, secret.Reveal())
	require.False(t, secret.IsZero())
	require.True(t, Secret{}.IsZero())

	params := ExternalAgentParams{CardURL: "https://reviewer.example.net/card.json", Token: secret}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("Resolving", "token", secret, "params", params)
	slog.New(slog.NewTextHandler(&logs, nil)).Info("Resolving", "token", secret, "params", params)
	encoded, err := json.Marshal(params)
	require.NoError(t, err)

	outputs := []string{
		fmt.Sprintf("%v %+v %#v %s %q %x", secret, secret, secret, secret, secret, secret),
		fmt.Sprintf("%v %+v %#v", params, params, params),
		logs.String(),
		string(encoded),
	}
	for _, out := range outputs {
		require.NotContains(t, out, value)
	}
	require.Contains(t, fmt.Sprintf("%+v", params), redactedSecret)
	require.Contains(t, string(encoded), redactedSecret)
}
