package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetupAndRestore covers the contract the four test packages
// lean on: Setup strips the hostile environment and pins the hermetic
// one, and restore brings back exactly what was there before.
func TestSetupAndRestore(t *testing.T) {
	// A hostile environment, the kind a developer's shell or a git
	// hook exports.
	t.Setenv("GIT_DIR", "/some/other/repo/.git")
	t.Setenv("GIT_INDEX_FILE", "/some/other/repo/.git/index")
	t.Setenv("GIT_CONFIG_GLOBAL", "/some/hostile/config")
	t.Setenv("GIT_EXEC_PATH", "/usr/lib/git-core")
	originalLCAll := os.Getenv("LC_ALL")

	restore, err := Setup()
	require.NoError(t, err)

	// Inherited GIT_* variables are gone, except the one we keep.
	_, hasDir := os.LookupEnv("GIT_DIR")
	require.False(t, hasDir, "GIT_DIR must be unset")
	_, hasIndex := os.LookupEnv("GIT_INDEX_FILE")
	require.False(t, hasIndex, "GIT_INDEX_FILE must be unset")
	require.Equal(t, "/usr/lib/git-core", os.Getenv("GIT_EXEC_PATH"),
		"GIT_EXEC_PATH must be kept")

	// The pinned values.
	require.Equal(t, os.DevNull, os.Getenv("GIT_CONFIG_GLOBAL"))
	require.Equal(t, "1", os.Getenv("GIT_CONFIG_NOSYSTEM"))
	require.Equal(t, "C", os.Getenv("LC_ALL"))
	require.Equal(t, testName, os.Getenv("GIT_AUTHOR_NAME"))
	require.Equal(t, testEmail, os.Getenv("GIT_AUTHOR_EMAIL"))
	require.Equal(t, testName, os.Getenv("GIT_COMMITTER_NAME"))
	require.Equal(t, testEmail, os.Getenv("GIT_COMMITTER_EMAIL"))

	ceiling, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	require.Equal(t, ceiling, os.Getenv("GIT_CEILING_DIRECTORIES"))
	require.Equal(t, "2", os.Getenv("GIT_CONFIG_COUNT"))
	require.Equal(t, "maintenance.auto", os.Getenv("GIT_CONFIG_KEY_0"))
	require.Equal(t, "false", os.Getenv("GIT_CONFIG_VALUE_0"))
	require.Equal(t, "gc.auto", os.Getenv("GIT_CONFIG_KEY_1"))
	require.Equal(t, "0", os.Getenv("GIT_CONFIG_VALUE_1"))

	// restore brings the originals back, set and unset alike.
	restore()
	require.Equal(t, "/some/other/repo/.git", os.Getenv("GIT_DIR"))
	require.Equal(t, "/some/other/repo/.git/index", os.Getenv("GIT_INDEX_FILE"))
	require.Equal(t, "/some/hostile/config", os.Getenv("GIT_CONFIG_GLOBAL"))
	require.Equal(t, "/usr/lib/git-core", os.Getenv("GIT_EXEC_PATH"))
	require.Equal(t, originalLCAll, os.Getenv("LC_ALL"))
}

// TestSetupSpawnsNoBackgroundMaintenance pins the reason for the
// maintenance pins: under Setup, a commit must not start the detached
// `git maintenance run --auto` that otherwise outlives it and races
// t.TempDir's cleanup. Git's trace2 event log records every child
// process the commit starts.
func TestSetupSpawnsNoBackgroundMaintenance(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	restore, err := Setup()
	require.NoError(t, err)
	t.Cleanup(restore)

	repo := t.TempDir()
	git := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	git(nil, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("one"), 0o644))
	git(nil, "add", "-A")

	trace := filepath.Join(t.TempDir(), "trace.json")
	git([]string{"GIT_TRACE2_EVENT=" + trace}, "commit", "-qm", "initial")

	events, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Contains(t, string(events), `"commit"`, "the trace must cover the commit")
	require.NotContains(t, string(events), `"maintenance"`,
		"a commit under Setup must not start background maintenance")
}
