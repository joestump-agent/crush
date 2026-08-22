package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	cleanup := isolateGlobalConfig()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// isolateGlobalConfig points every global Crush lookup at a throwaway
// directory tree for the lifetime of the test binary, and returns the
// func that tears it down.
//
// config.Init always merges the user-level config locations on top of
// the working dir, so without this the package loads the developer's
// real ~/.config/crush/crush.json: their providers, LSPs, global
// CRUSH.md context, skills, and MCP servers. Several tests here reach
// config.Init with no isolation of their own — channels_test's
// insertChannelWorkspace, and agenttest.NewCoordinator by way of
// accepted_run_integration_test — so on a developer machine they load
// whatever that config declares, connect to those MCP servers for
// real, and issue authenticated model-discovery requests to the
// provider endpoints it names. That is slow, non-deterministic, and
// dependent on host state a test has no business reading.
//
// It is the same trap that made TestSidekickDashboardSubscribeDeliversToolPushes
// report 129 tools where it expects 1; see isolateGlobalConfig in
// internal/agent/agent_test.go.
//
// This has to happen once, before m.Run: most tests in this package
// run under t.Parallel(), which forbids t.Setenv.
func isolateGlobalConfig() func() {
	root, err := os.MkdirTemp("", "crush-backend-test-*")
	if err != nil {
		panic(fmt.Sprintf("isolate global config: %v", err))
	}
	// CRUSH_SKILLS_DIR replaces the whole default skills search path.
	// It is not optional: that path reaches into ~/.agents/skills and
	// ~/.claude/skills via home.Dir(), which caches os.UserHomeDir() in
	// a package var at init, so redirecting HOME cannot reach it.
	// XDG_CONFIG_HOME covers the home.Config() lookups that have no
	// dedicated override (crush/ignore, git/ignore, commands).
	//
	// XDG_DATA_HOME is deliberately left alone: config.cachePathFor
	// resolves the Catwalk provider catalog cache under it, and
	// redirecting it would send every config.Init back to the network
	// for a provider list.
	for envVar, dir := range map[string]string{
		"CRUSH_GLOBAL_CONFIG": filepath.Join(root, "config", "crush"),
		"CRUSH_GLOBAL_DATA":   filepath.Join(root, "data", "crush"),
		"CRUSH_CACHE_DIR":     filepath.Join(root, "cache"),
		"CRUSH_SKILLS_DIR":    filepath.Join(root, "skills"),
		"XDG_CONFIG_HOME":     filepath.Join(root, "config"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic(fmt.Sprintf("isolate global config: %v", err))
		}
		if err := os.Setenv(envVar, dir); err != nil {
			panic(fmt.Sprintf("isolate global config: %v", err))
		}
	}
	return func() { os.RemoveAll(root) }
}

// TestGlobalConfigIsolated guards the isolation TestMain installs. It
// is the assertion the rest of this package leans on implicitly: a
// bare config.Init here must see nothing from the host.
//
// Not parallel — it reads process env that the xdgIsolated tests
// mutate with t.Setenv.
func TestGlobalConfigIsolated(t *testing.T) {
	root := os.Getenv("CRUSH_GLOBAL_CONFIG")
	require.NotEmpty(t, root, "TestMain must set CRUSH_GLOBAL_CONFIG")

	// The lookups config.Init merges on top of the working dir all
	// have to land inside the throwaway tree, not the developer's
	// home. GlobalSkillsDirs is the one that cannot be fixed by
	// redirecting HOME, since home.Dir caches os.UserHomeDir at init.
	require.True(t, strings.HasPrefix(config.GlobalConfig(), root),
		"GlobalConfig %q escaped the test tree", config.GlobalConfig())
	for _, dir := range config.GlobalSkillsDirs() {
		require.Equal(t, os.Getenv("CRUSH_SKILLS_DIR"), dir,
			"GlobalSkillsDirs reached outside the test tree")
	}

	// An empty working dir plus the isolated globals means an empty
	// config. A non-zero count here is the host's crush.json leaking
	// in, which is what makes MCP servers get spawned for real.
	cfg, err := config.Init(t.TempDir(), "", false)
	require.NoError(t, err)
	require.Empty(t, cfg.Config().MCP, "host MCP servers leaked into the test config")
	require.Empty(t, cfg.Config().LSP, "host LSP servers leaked into the test config")
}
