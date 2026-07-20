package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joestump-agent/a2tea"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/session"
)

// captureSink records the latest snapshot set the collector emitted.
type captureSink struct {
	mu   sync.Mutex
	last []dispatchSnapshot
	got  int
}

func (s *captureSink) emit(snaps []dispatchSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = snaps
	s.got++
}

func (s *captureSink) latest() []dispatchSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func TestFormatDispatchLine(t *testing.T) {
	t.Parallel()
	line := formatDispatchLine(dispatchSnapshot{
		Workspace: "/ws/a",
		Status:    "running",
		Todos: []session.Todo{
			{Content: "step one", Status: session.TodoStatusCompleted},
			{Content: "step two", Status: session.TodoStatusInProgress},
			{Content: "step three", Status: session.TodoStatusPending},
		},
	})
	require.Contains(t, line, "/ws/a")
	require.Contains(t, line, "running")
	require.Contains(t, line, "1/3")
	require.Contains(t, line, "step two", "the in-progress todo should be surfaced")
}

func TestComposeDispatchSurface_Empty(t *testing.T) {
	t.Parallel()
	surface := composeDispatchSurface(nil)
	require.Contains(t, surface.Content, "deleteSurface")
	require.Contains(t, surface.Content, dispatchSurfaceID)
	// The clear payload must still be valid A2UI the renderer can scan.
	_, err := a2tea.Scan(surface.Content)
	require.NoError(t, err)
}

func TestComposeDispatchSurface_WithDispatches(t *testing.T) {
	t.Parallel()
	surface := composeDispatchSurface([]dispatchSnapshot{
		{DispatchID: "d1", Workspace: "/ws/one", Status: "running", Todos: []session.Todo{
			{Content: "a", Status: session.TodoStatusCompleted},
			{Content: "b", Status: session.TodoStatusInProgress},
		}},
		{DispatchID: "d2", Workspace: "/ws/two", Status: "running"},
	})

	require.Contains(t, surface.Content, "updateComponents")
	require.Contains(t, surface.Content, dispatchSurfaceID)
	require.Contains(t, surface.Content, "/ws/one")
	require.Contains(t, surface.Content, "/ws/two")
	require.Contains(t, surface.Content, "1/2")

	// It must render as a valid A2UI surface.
	parts, err := a2tea.Scan(surface.Content)
	require.NoError(t, err)
	require.NotEmpty(t, parts)
}

func TestDispatchProgress_TracksReducesAndUntracks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := session.NewInMemoryService()
	sink := &captureSink{}
	p := newDispatchProgress(sink)

	sess, err := sessions.Create(ctx, "dispatch")
	require.NoError(t, err)

	p.track(ctx, "d1", "/ws/one", sess.ID, sessions)

	// Initial emit: one active dispatch, no todos yet.
	require.Eventually(t, func() bool {
		snaps := sink.latest()
		return len(snaps) == 1 && snaps[0].DispatchID == "d1" && len(snaps[0].Todos) == 0
	}, time.Second, 5*time.Millisecond)

	// The dispatched agent records todos: a save publishes an update the
	// collector reduces into the snapshot.
	sess.Todos = []session.Todo{
		{Content: "a", Status: session.TodoStatusCompleted},
		{Content: "b", Status: session.TodoStatusInProgress},
	}
	_, err = sessions.Save(ctx, sess)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		snaps := sink.latest()
		return len(snaps) == 1 && len(snaps[0].Todos) == 2
	}, time.Second, 5*time.Millisecond)

	// Untrack drops it and re-emits an empty set.
	p.untrack("d1")
	require.Eventually(t, func() bool {
		return len(sink.latest()) == 0
	}, time.Second, 5*time.Millisecond)
}

func TestDispatchProgress_IgnoresOtherSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := session.NewInMemoryService()
	sink := &captureSink{}
	p := newDispatchProgress(sink)

	tracked, err := sessions.Create(ctx, "tracked")
	require.NoError(t, err)
	other, err := sessions.Create(ctx, "other")
	require.NoError(t, err)

	p.track(ctx, "d1", "/ws", tracked.ID, sessions)
	defer p.untrack("d1")

	// An update to a different session in the same store must not alter
	// the tracked dispatch's todos.
	other.Todos = []session.Todo{{Content: "x", Status: session.TodoStatusInProgress}}
	_, err = sessions.Save(ctx, other)
	require.NoError(t, err)

	require.Never(t, func() bool {
		snaps := sink.latest()
		return len(snaps) == 1 && len(snaps[0].Todos) > 0
	}, 100*time.Millisecond, 10*time.Millisecond)
}

// nonBlockingSink asserts the collector composes text the surface builder
// accepts even for many concurrent dispatches (defensive smoke over the
// reduce/compose path).
func TestComposeSurface_ScansForManyDispatches(t *testing.T) {
	t.Parallel()
	var snaps []dispatchSnapshot
	for i := 0; i < 5; i++ {
		snaps = append(snaps, dispatchSnapshot{
			DispatchID: "d" + strings.Repeat("x", i+1),
			Workspace:  "/ws/" + strings.Repeat("y", i+1),
			Status:     "running",
		})
	}
	_, err := a2tea.Scan(composeDispatchSurface(snaps).Content)
	require.NoError(t, err)
}
