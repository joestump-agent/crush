//go:build !windows

package a2a

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// verifySocketDir is the POSIX half of ensureSocketDir's checks (#558):
// the directory must belong to this user, and is brought to exactly
// 0700 when it is anything else. [os.MkdirAll] leaves an existing
// directory's owner and mode alone, so without this a directory planted
// by another user, or one left loose, is used as found.
func verifySocketDir(dir string, fi os.FileInfo) error {
	if err := checkSocketDirOwner(dir, fi, os.Getuid()); err != nil {
		return err
	}
	if fi.Mode().Perm() == 0o700 {
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("a2a: tighten socket dir %s to 0700: %w", dir, err)
	}
	return nil
}

// checkSocketDirOwner refuses a socket directory owned by anyone but
// uid. The mode is no substitute: whatever it is now, the owner can
// widen it again at will, and then swap the socket under the client.
func checkSocketDirOwner(dir string, fi os.FileInfo, uid int) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("a2a: socket dir %s: owner unavailable", dir)
	}
	if int(st.Uid) != uid {
		return fmt.Errorf("a2a: socket dir %s is owned by uid %d, not uid %d", dir, st.Uid, uid)
	}
	return nil
}

// chmodSocket makes the freshly bound socket 0600 without following a
// symlink at its path (#558). [os.Chmod] follows symlinks, so a link
// swapped in between the bind and the chmod would steer the mode change
// at any file the planter chose. The directory is opened O_NOFOLLOW and
// the socket is addressed relative to it, so the path is never resolved
// through a link.
func chmodSocket(path string) error {
	dir, err := os.OpenFile(filepath.Dir(path), os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("a2a: open socket dir: %w", err)
	}
	defer dir.Close()
	dirfd, base := int(dir.Fd()), filepath.Base(path)

	err = unix.Fchmodat(dirfd, base, 0o600, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		// Linux before 6.6 has no fchmodat2, so its fchmodat cannot
		// take AT_SYMLINK_NOFOLLOW (x/sys answers EOPNOTSUPP), and a
		// seccomp profile that predates the syscall answers EPERM.
		// Confirm the entry is the socket and not a link, then chmod
		// it plainly: the directory was verified this user's and 0700
		// before the bind, so nobody else can swap the entry in between.
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("a2a: stat socket %s: %w", path, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
			return fmt.Errorf("a2a: %s is not a socket", path)
		}
		err = unix.Fchmodat(dirfd, base, 0o600, 0)
	}
	if err != nil {
		return fmt.Errorf("a2a: chmod socket %s: %w", path, err)
	}
	return nil
}
