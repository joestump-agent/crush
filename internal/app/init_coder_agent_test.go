package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestInitCoderAgent_BuildsCoordinatorOnGlobalContext verifies that the
// coordinator's lifetime context derives from the app's global context,
// not from the caller's request context (#419). Canceling the caller's
// context after InitCoderAgent returns must not cancel the coordinator;
// canceling globalCtx must.
func TestInitCoderAgent_BuildsCoordinatorOnGlobalContext(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}}
	cfg.SetupAgents()

	globalCtx, cancelGlobal := context.WithCancel(context.Background())
	defer cancelGlobal()

	app := NewForTest(globalCtx)
	defer app.ShutdownForTest()
	app.config = config.NewTestStore(cfg)

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()

	var captured context.Context
	app.newCoordinator = func(ctx context.Context, _ agent.CoordinatorOptions) (agent.Coordinator, error) {
		captured = ctx
		return nil, nil
	}

	require.NoError(t, app.InitCoderAgent(callerCtx))
	require.NotNil(t, captured)

	cancelCaller()
	require.NoError(t, captured.Err())

	cancelGlobal()
	require.Eventually(t, func() bool {
		return captured.Err() != nil
	}, 5*time.Second, 10*time.Millisecond)
}

// recordingCoordinator records SetInteractive calls. Every other
// Coordinator method panics through the embedded nil interface: the
// reuse path under test must never reach them.
type recordingCoordinator struct {
	agent.Coordinator

	mu    sync.Mutex
	modes []bool
}

func (r *recordingCoordinator) SetInteractive(_ context.Context, interactive bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modes = append(r.modes, interactive)
	return nil
}

func (r *recordingCoordinator) interactiveCalls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.modes...)
}

// TestInitCoderAgent_ReusesCoordinatorAcrossAttaches pins the #420 fix:
// a second attach must reuse the existing coordinator instead of
// rebuilding it — a rebuild orphans the dispatch registry, the
// injection targets and the cron scheduler — and only ask it for the
// palette mode the attach requests.
func TestInitCoderAgent_ReusesCoordinatorAcrossAttaches(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}}
	cfg.SetupAgents()

	globalCtx, cancelGlobal := context.WithCancel(context.Background())
	defer cancelGlobal()

	app := NewForTest(globalCtx)
	defer app.ShutdownForTest()
	app.config = config.NewTestStore(cfg)

	coord := &recordingCoordinator{}
	var builds int
	app.newCoordinator = func(context.Context, agent.CoordinatorOptions) (agent.Coordinator, error) {
		builds++
		return coord, nil
	}

	require.NoError(t, app.InitCoderAgent(context.Background()))
	require.NoError(t, app.InitCoderAgent(context.Background()))
	require.NoError(t, app.InitCoderAgentNonInteractive(context.Background()))

	require.Equal(t, 1, builds, "a second attach must not build a new coordinator")
	require.Same(t, coord, app.AgentCoordinator)
	// The first build carries its mode via CoordinatorOptions; every
	// later attach goes through SetInteractive instead.
	require.Equal(t, []bool{true, false}, coord.interactiveCalls(),
		"each re-attach must request the palette mode it was given")
}
