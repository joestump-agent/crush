//go:build !windows

package a2a

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A socket directory owned by another user is refused whatever its
// mode (#558): the owner can widen it again at will, then swap the
// socket under the in-process client. Ownership cannot be faked on a
// real file system, so the check takes the expected uid as a seam.
func TestSocketDirRefusesForeignOwner(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fi, err := os.Lstat(dir)
	require.NoError(t, err)
	require.NoError(t, checkSocketDirOwner(dir, fi, os.Getuid()))

	err = checkSocketDirOwner(dir, fi, os.Getuid()+1)
	require.Error(t, err)
	require.Contains(t, err.Error(), dir)
	require.Contains(t, err.Error(), "owned by uid")
}

// Making the socket 0600 must not follow a symlink at its path (#558):
// os.Chmod does, so a link swapped in between the bind and the chmod
// would steer the mode change at a file of the planter's choosing.
// Whether the platform changes the link's own mode (macOS) or refuses
// (Linux), the target keeps its mode.
func TestChmodSocketDoesNotFollowSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, nil, 0o644))
	require.NoError(t, os.Chmod(target, 0o644)) // Past the umask.
	link := filepath.Join(dir, "link.sock")
	require.NoError(t, os.Symlink(target, link))

	_ = chmodSocket(link)

	fi, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), fi.Mode().Perm(), "the symlink target must keep its mode")
}
