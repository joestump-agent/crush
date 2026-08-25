// Non-Interactive Stream Cursor
//
// The non-interactive printers keep a per-message cursor recording how
// many bytes of an assistant message have already been written to
// stdout, and emit only the tail on each streamed update. That
// bookkeeping is shared verbatim by `crush run` (internal/cmd) and
// App.RunNonInteractive (internal/app); it lives here so a fix cannot
// land in one copy and miss the other.
//
// @joestump 08/25/2026 - Extracted from the two call sites while
// reviewing the shrink-reset fix (#284), which had to be applied twice
// because the logic was duplicated.

package format

import (
	"log/slog"
	"strings"
)

// NextStreamChunk returns the not-yet-written tail of an assistant
// message's content given the cursor position readBytes, together with
// the cursor position the caller should store afterwards.
//
// Content for a given message ID is normally append-only, but a
// provider stream reset, a retried request, or a rewritten content part
// can make the same message come back shorter than the cursor. Slicing
// on a stale cursor would panic, so that case resets the cursor to zero
// and re-emits from the start: duplicating a little output is always
// preferable to aborting a run that may already have minutes of tool
// work behind it.
//
// Leading horizontal whitespace is trimmed whenever the chunk starts at
// the beginning of the message, since models frequently open with
// indentation that is meaningless on stdout.
func NextStreamChunk(content string, readBytes int) (chunk string, next int) {
	if len(content) < readBytes {
		slog.Warn("Non-interactive: message content shrank; resetting stream cursor",
			"message_length", len(content), "read_bytes", readBytes)
		readBytes = 0
	}
	chunk = content[readBytes:]
	if readBytes == 0 {
		chunk = strings.TrimLeft(chunk, " \t")
	}
	return chunk, len(content)
}
