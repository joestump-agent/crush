package backend

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A workspace whose agent host has not started refuses a snapshot at
// once and holds a stream until its caller gives up; an unknown
// workspace is not found (#421).
func TestA2AHostBeforeTheHostStarts(t *testing.T) {
	b, ws, _ := newPublishingWorkspace(t)

	_, err := b.A2AHost(t.Context(), ws.ID, false)
	require.ErrorIs(t, err, ErrA2AHostNotRunning)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = b.A2AHost(ctx, ws.ID, true)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a stream waits for the host")

	_, err = b.A2AHost(t.Context(), "no-such-workspace", true)
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}
