package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReloadDoesNotMutatePublishedConfig is a regression test for a
// data race between config auto-reload and concurrent Config() readers
// (seen as TestDisableDockerMCP_PublishesConfigChanged failing the
// -race build in internal/backend). reloadFromDiskLocked used to
// publish the freshly loaded config via setConfig before resolving its
// models and running SetupAgents, so the live snapshot's Agents field
// was reassigned in place while readers held the pointer, breaking the
// swap-never-mutate contract documented on Config(). The reader loop
// below mirrors coordinator buildTools reading Config().Agents;
// SetConfigField drives the auto-reload that used to mutate the
// published snapshot.
func TestReloadDoesNotMutatePublishedConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	t.Setenv("CRUSH_GLOBAL_CONFIG", dir)
	t.Setenv("CRUSH_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("openai", "gpt-4")), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			// The read itself is the probe: it must never race a
			// reload mutating the published snapshot.
			_, _ = store.Config().Agents[AgentTask]
		}
	})

	for i := range 20 {
		require.NoError(t, store.SetConfigField(ScopeGlobal, "disable_notifications", i%2 == 0))
	}
	close(done)
	wg.Wait()
}
