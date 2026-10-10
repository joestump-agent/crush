package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// A relative --cwd must come back absolute: in client/server mode the
// resolved value becomes proto.Workspace.Path, and the server process would
// otherwise resolve a relative path against its own working directory.
func TestResolveCwd_RelativeFlagYieldsAbsolutePath(t *testing.T) {
	original, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.Chdir(original)
	})

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.Chdir(dir))

	cmd := &cobra.Command{}
	cmd.Flags().StringP("cwd", "c", "", "Current working directory")
	require.NoError(t, cmd.Flags().Set("cwd", "sub"))

	resolved, err := ResolveCwd(cmd)
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(resolved))
	require.Equal(t, filepath.Join(dir, "sub"), resolved)
}
