package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSummarizeThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cw   int64
		want int64
	}{
		{name: "unknown window is zero for the caller to skip", cw: 0, want: 0},
		{name: "small window keeps the ratio", cw: 128_000, want: 25_600},
		{name: "window at the large threshold keeps the ratio", cw: 200_000, want: 40_000},
		{name: "just above the threshold is a hair over the floor", cw: 200_001, want: 20_001},
		{name: "400k window keeps 10%", cw: 400_000, want: 40_000},
		{name: "524k window keeps 10%", cw: 524_288, want: 52_429},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, summarizeThreshold(tt.cw))
		})
	}
}

// TestSummarizeThresholdCoversOneTurn is the control for the session that died
// at 498,253 of a 524,288-token window: it had 26,035 tokens left, which the
// fixed 20,000 buffer called plenty, and the next request was refused. The
// headroom must be wider than one turn's addition, and one large tool result is
// the unit that matters.
func TestSummarizeThresholdCoversOneTurn(t *testing.T) {
	t.Parallel()

	const cw = 524_288
	tokens := int64(498_253)
	remaining := cw - tokens

	assert.LessOrEqual(t, remaining, summarizeThreshold(cw), "must summarise before the window is exhausted")
	assert.Less(t, int64(largeContextWindowBuffer), summarizeThreshold(cw), "headroom must exceed the old fixed buffer")
}

func TestSummarizeThresholdNeverShrinksBelowTheBuffer(t *testing.T) {
	t.Parallel()

	for cw := int64(largeContextWindowThreshold) + 1; cw <= 2_000_000; cw += 997 {
		got := summarizeThreshold(cw)
		assert.GreaterOrEqual(t, got, int64(largeContextWindowBuffer), "window %d", cw)
		assert.GreaterOrEqual(t, got*int64(largeContextWindowRatio), cw, "window %d keeps less than a tenth free", cw)
	}
}
