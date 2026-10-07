package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// Agent Build Teardown
//
// Every coordinator run rebuilds the tool palette, and the task sub-agent
// built there renders its system prompt on a detached goroutine that runs
// git branch, git status and git log in the working directory. git status
// writes .git/index.lock and renames it over .git/index, so a render that
// outlives teardown races whoever removes the repository: the dispatch
// tests' t.TempDir() removal failed with "unlinkat .git: directory not
// empty" (#515). These tests hold the render on the fake clock and pin
// that both teardowns — the production CancelAll and the dispatch test
// fixture's cleanup — wait for it.
//
// @joestump-agent 10/07/2026 - Added for #515.

// newBuildTeardownEnv builds the dispatch fixture's coordinator over a git
// repository, with the offline test provider and a fake main agent, the
// way a dispatch's parent delivery turn finds it. It returns the
// coordinator, the repository's .git directory and a parent session ID.
func newBuildTeardownEnv(t *testing.T) (*coordinator, string, string) {
	t.Helper()
	env := testEnv(t)
	initGitRepo(t, env.workingDir)
	c := newDispatchTestCoordinator(t, env)

	const providerID = "test-provider"
	c.cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID:      providerID,
		Name:    "Test",
		Type:    openaicompat.Name,
		BaseURL: "http://127.0.0.1:0/v1",
		APIKey:  "test",
		Models:  []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	selected := config.SelectedModel{Provider: providerID, Model: "test-model"}
	c.cfg.OverridePreferredModel(config.SelectedModelTypeLarge, selected)
	c.cfg.OverridePreferredModel(config.SelectedModelTypeSmall, selected)

	main := &fakeMainAgent{model: dispatchTestModel()}
	c.mainAgent = main
	c.mainAgentName = config.AgentCoder
	c.agents = map[string]SessionAgent{config.AgentCoder: main}

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	return c, filepath.Join(env.workingDir, ".git"), parent.ID
}

// heldPromptRender installs a promptBuildHook that holds every system
// prompt render for one second of the bubble's fake clock — which only
// advances once every other goroutine in the bubble is durably blocked —
// and then reports whether the repository was still there when the render
// went on to run git.
func heldPromptRender(c *coordinator, gitDir string) (rendered, repoGone *atomic.Bool) {
	rendered, repoGone = new(atomic.Bool), new(atomic.Bool)
	c.promptBuildHook = func() {
		time.Sleep(time.Second)
		if _, err := os.Stat(gitDir); err != nil {
			repoGone.Store(true)
		}
		rendered.Store(true)
	}
	return rendered, repoGone
}

// TestCancelAllWaitsForAgentBuilds pins the production teardown (#515):
// CancelAll — the first step of App.Shutdown and Workspace.Shutdown —
// returns only once the agent builds a run started have finished, so no
// git process the coordinator started outlives it. Without the wait,
// CancelAll returns while the render is still held.
func TestCancelAllWaitsForAgentBuilds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		c, gitDir, parentID := newBuildTeardownEnv(t)
		rendered, _ := heldPromptRender(c, gitDir)

		// The delivery turn's shape: a run rebuilds the palette, which
		// builds the task sub-agent and starts its prompt render.
		_, err := c.run(context.Background(), nil, parentID, "deliver")
		require.NoError(t, err)
		require.False(t, rendered.Load(), "the render is held until the test blocks")

		c.CancelAll()
		require.True(t, rendered.Load(), "CancelAll returned while an agent build was still rendering its prompt")
	})
}

// TestDispatchFixtureJoinsAgentBuilds pins the test-side teardown (#515):
// the dispatch fixture's cleanup joins the agent builds a test's runs
// started before t.TempDir() removes the repository. The test body
// returns with the render still held, exactly as a dispatch test returns
// right after its parent delivery turn. Without the join, the render runs
// git only after the repository is gone; on a real clock it is the
// render racing the removal that left .git non-empty.
func TestDispatchFixtureJoinsAgentBuilds(t *testing.T) {
	t.Parallel()
	var rendered, repoGone *atomic.Bool
	synctest.Test(t, func(t *testing.T) {
		// Registered first, so it runs last, after t.TempDir()'s removal:
		// blocking here advances the fake clock, so a build the fixture
		// failed to join runs now and the assertions below see it, rather
		// than the bubble panicking on a goroutine still blocked.
		t.Cleanup(func() { time.Sleep(time.Minute) })

		c, gitDir, parentID := newBuildTeardownEnv(t)
		rendered, repoGone = heldPromptRender(c, gitDir)

		_, err := c.run(context.Background(), nil, parentID, "deliver")
		require.NoError(t, err)
	})
	require.True(t, rendered.Load(), "the held render must have run")
	require.False(t, repoGone.Load(), "an agent build ran git after the test's repository was removed")
}
