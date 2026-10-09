//go:build windows

package a2a

import "os"

// verifySocketDir has nothing to add on Windows (#558): a file's owner
// is a SID rather than a uid, and the mode bits [os.Lstat] reports do
// not describe NTFS access. ensureSocketDir's symlink and directory
// checks still run, and the fallback directory lives under the user's
// own temp directory.
func verifySocketDir(dir string, fi os.FileInfo) error {
	return nil
}

// chmodSocket is a no-op on Windows, where a unix socket's mode bits
// carry no access control; the bearer token gates the host there
// (#357).
func chmodSocket(path string) error {
	return nil
}
