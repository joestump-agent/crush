package app

import (
	"context"
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
