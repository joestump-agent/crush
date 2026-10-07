package backend

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/a2a"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/stretchr/testify/require"
)

// A workspace whose agent host has not started refuses a snapshot at
// once and holds a stream until its caller gives up; an unknown
// workspace is not found (#421).
func TestA2AHostBeforeTheHostStarts(t *testing.T) {
	t.Parallel()
	b, ws := newHostWorkspace(t, newHostSource())

	_, err := b.A2AHost(t.Context(), ws.ID, false)
	require.ErrorIs(t, err, ErrA2AHostNotRunning)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = b.A2AHost(ctx, ws.ID, true)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a stream waits for the host")

	_, err = b.A2AHost(t.Context(), "no-such-workspace", true)
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}

// hostSource stands in for an app's A2A host slot (#421): empty until
// set, with a channel closed when it is. read is closed on the first
// look at it.
type hostSource struct {
	mu       sync.Mutex
	host     *a2a.ServerFactory
	ready    chan struct{}
	read     chan struct{}
	readOnce sync.Once
}

func newHostSource() *hostSource {
	return &hostSource{ready: make(chan struct{}), read: make(chan struct{})}
}

func (s *hostSource) get() (*a2a.ServerFactory, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readOnce.Do(func() { close(s.read) })
	return s.host, s.ready
}

func (s *hostSource) set(host *a2a.ServerFactory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = host
	close(s.ready)
}

// idleRunner is a served agent that runs until it is canceled.
type idleRunner struct{}

func (idleRunner) Run(ctx context.Context, _ agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (idleRunner) Cancel(string) {}

func (idleRunner) EnqueueWhenBusy(agent.SessionAgentCall) bool { return false }

// newHostWorkspace installs a synthetic workspace whose A2A host comes
// from source.
func newHostWorkspace(t *testing.T, source *hostSource) (*Backend, *Workspace) {
	t.Helper()
	b, _ := newTestBackend(t)
	ws, _ := insertTestWorkspace(t, b, t.TempDir())
	ws.ctx, ws.cancel = context.WithCancel(context.Background())
	t.Cleanup(ws.cancel)
	ws.a2aHost = source.get
	return b, ws
}

// A stream opened before anything is dispatched is held through both
// halves of the host's start — the app recording its host, then the
// host coming up — and answered with that host (#421).
func TestA2AHostWaitsForTheHostToStart(t *testing.T) {
	t.Parallel()
	source := newHostSource()
	b, ws := newHostWorkspace(t, source)

	type result struct {
		host *a2a.ServerFactory
		err  error
	}
	got := make(chan result, 1)
	go func() {
		host, err := b.A2AHost(t.Context(), ws.ID, true)
		got <- result{host, err}
	}()

	// Set the host only once the stream has found the slot empty, so it
	// is the wait that sees it.
	<-source.read
	factory := a2a.NewServerFactory(t.TempDir())
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	source.set(factory)
	_, err := factory.StartServer(t.Context(), a2a.ServerParams{
		DispatchID: "d1", Runner: idleRunner{}, SessionID: "s1", ContextID: "s1", Listed: true,
	})
	require.NoError(t, err)

	select {
	case r := <-got:
		require.NoError(t, r.err)
		require.Same(t, factory, r.host)
	case <-time.After(10 * time.Second):
		t.Fatal("the held stream never got the host")
	}
}

// A held stream ends with its workspace rather than outliving it (#421).
func TestA2AHostEndsWithTheWorkspace(t *testing.T) {
	t.Parallel()
	b, ws := newHostWorkspace(t, newHostSource())

	got := make(chan error, 1)
	go func() {
		_, err := b.A2AHost(t.Context(), ws.ID, true)
		got <- err
	}()
	ws.cancel()

	select {
	case err := <-got:
		require.ErrorIs(t, err, ErrWorkspaceClosing)
	case <-time.After(10 * time.Second):
		t.Fatal("the held stream outlived its workspace")
	}
}
