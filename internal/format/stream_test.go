package format

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNextStreamChunk(t *testing.T) {
	t.Parallel()

	t.Run("first chunk trims leading horizontal whitespace", func(t *testing.T) {
		t.Parallel()
		chunk, next := NextStreamChunk("  \thello", 0)
		require.Equal(t, "hello", chunk)
		require.Equal(t, len("  \thello"), next, "cursor tracks raw content length, not the trimmed chunk")
	})

	t.Run("append-only growth emits only the tail", func(t *testing.T) {
		t.Parallel()
		chunk, next := NextStreamChunk("hello world", 5)
		require.Equal(t, " world", chunk)
		require.Equal(t, 11, next)
	})

	t.Run("mid-message chunk keeps leading whitespace", func(t *testing.T) {
		t.Parallel()
		chunk, _ := NextStreamChunk("a  b", 1)
		require.Equal(t, "  b", chunk, "only the start of a message is trimmed")
	})

	t.Run("shrunken content resets the cursor and re-emits", func(t *testing.T) {
		t.Parallel()
		// The 270 -> 2 shrink observed in the wild (#283).
		long := strings.Repeat("a", 270)
		_, next := NextStreamChunk(long, 0)
		require.Equal(t, 270, next)

		chunk, next := NextStreamChunk("ok", next)
		require.Equal(t, "ok", chunk, "re-emit from the start rather than slicing on a stale cursor")
		require.Equal(t, 2, next)
	})

	t.Run("empty content after a reset is not fatal", func(t *testing.T) {
		t.Parallel()
		chunk, next := NextStreamChunk("", 42)
		require.Empty(t, chunk)
		require.Equal(t, 0, next)
	})

	t.Run("unchanged content emits nothing", func(t *testing.T) {
		t.Parallel()
		chunk, next := NextStreamChunk("hello", 5)
		require.Empty(t, chunk)
		require.Equal(t, 5, next)
	})
}
