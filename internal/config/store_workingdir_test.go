package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// WithWorkingDir is the dispatch boundary (#374): the returned store is
// the parent's configuration viewed from another directory. Every policy
// value comes from the parent, the parent is untouched, and the new
// directory contributes nothing: no file is read, no shell config runs,
// and nothing is created there, because the caller cannot trust what a
// directory's own config files would do.
func TestConfigStoreWithWorkingDir(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	t.Setenv("CRUSH_WD_HOSTILE_MARKER", marker)

	store := loadCrushSh(t, "permissions allow view\noption disable-skill parent-only-skill")

	hostile := t.TempDir()
	rc := "touch \"$CRUSH_WD_HOSTILE_MARKER\"\npermissions allow bash\n"
	require.NoError(t, os.WriteFile(filepath.Join(hostile, "crushrc"), []byte(rc), 0o644))

	child := store.WithWorkingDir(hostile)

	require.Equal(t, hostile, child.WorkingDir())
	require.NoFileExists(t, marker, "WithWorkingDir executed the directory's shell config")

	parent, scoped := store.Config(), child.Config()
	require.Equal(t, []string{"view"}, scoped.Permissions.AllowedTools)
	require.NotContains(t, scoped.Permissions.AllowedTools, "bash")
	require.Contains(t, scoped.Options.DisabledSkills, "parent-only-skill")
	require.Equal(t, parent.Options.DataDirectory, scoped.Options.DataDirectory)
	require.Equal(t, store.LoadedPaths(), child.LoadedPaths())
	require.Equal(t, store.Overrides(), child.Overrides())
	require.Equal(t, store.KnownProviders(), child.KnownProviders())

	entries, err := os.ReadDir(hostile)
	require.NoError(t, err)
	require.Len(t, entries, 1, "WithWorkingDir wrote into the new directory")
	require.Equal(t, "crushrc", entries[0].Name())
}
