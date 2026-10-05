package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPartsRoundTrip pins the on-disk part encoding: every part type must
// survive a trip through marshalParts unchanged.
func TestPartsRoundTrip(t *testing.T) {
	t.Parallel()

	parts := []ContentPart{
		ReasoningContent{Thinking: "hmm", Signature: "sig", ToolID: "t1", StartedAt: 1, FinishedAt: 2},
		TextContent{Text: "hello"},
		ImageURLContent{URL: "https://example.test/a.png", Detail: "high"},
		BinaryContent{Path: "/tmp/a.bin", MIMEType: "application/octet-stream", Data: []byte{0, 1, 2}},
		ToolCall{ID: "c1", Name: "bash", Input: `{"cmd":"ls"}`, ProviderExecuted: true, Finished: true},
		ToolResult{ToolCallID: "c1", Name: "bash", Content: "out", MIMEType: "text/plain", IsError: true},
		Finish{Reason: FinishReasonEndTurn, Time: 42, Message: "done"},
		ShellCommand{Command: "ls -la", Output: "total 0", ExitCode: 0},
	}

	encoded, err := marshalParts(parts)
	require.NoError(t, err)

	decoded, err := unmarshalParts(encoded)
	require.NoError(t, err)
	require.Equal(t, parts, decoded)
}

func TestUnmarshalPartsEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("empty array yields no parts", func(t *testing.T) {
		t.Parallel()

		got, err := unmarshalParts([]byte(`[]`))
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("unknown type is an error", func(t *testing.T) {
		t.Parallel()

		_, err := unmarshalParts([]byte(`[{"type":"nope","data":{}}]`))
		require.ErrorContains(t, err, "unknown part type: nope")
	})

	t.Run("payload may precede its type tag", func(t *testing.T) {
		t.Parallel()

		// JSON does not guarantee key order.
		got, err := unmarshalParts([]byte(`[{"data":{"text":"hi"},"type":"text"}]`))
		require.NoError(t, err)
		require.Equal(t, []ContentPart{TextContent{Text: "hi"}}, got)
	})

	t.Run("malformed json is an error", func(t *testing.T) {
		t.Parallel()

		_, err := unmarshalParts([]byte(`{`))
		require.Error(t, err)
	})
}

// TestSteerMarkSurvivesRoundTrip pins the steer flag (#410): it must
// survive the on-disk encoding (persist/reload) and streaming appends,
// or rebuilt dispatch cards lose their steer log.
func TestSteerMarkSurvivesRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("persist and reload keep the flag", func(t *testing.T) {
		t.Parallel()

		encoded, err := marshalParts([]ContentPart{TextContent{Text: "stop writing Rust", Steer: true}})
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"steer":true`)

		decoded, err := unmarshalParts(encoded)
		require.NoError(t, err)
		require.Equal(t, []ContentPart{TextContent{Text: "stop writing Rust", Steer: true}}, decoded)
	})

	t.Run("unmarked messages stay valid", func(t *testing.T) {
		t.Parallel()

		// Pre-#410 payloads have no steer key; omitempty keeps them
		// byte-identical on rewrite.
		decoded, err := unmarshalParts([]byte(`[{"type":"text","data":{"text":"hi"}}]`))
		require.NoError(t, err)
		require.False(t, decoded[0].(TextContent).Steer)

		encoded, err := marshalParts(decoded)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "steer")
	})

	t.Run("AppendContent preserves the flag", func(t *testing.T) {
		t.Parallel()

		msg := &Message{Parts: []ContentPart{TextContent{Text: "st", Steer: true}}}
		msg.AppendContent("reaming")
		require.Equal(t, TextContent{Text: "streaming", Steer: true}, msg.Parts[0])
	})
}
