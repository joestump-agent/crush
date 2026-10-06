//go:build linux

package a2a

import (
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

// connPeerUID reads the effective uid the kernel attached to the socket
// peer via SO_PEERCRED (#357). The uid is the kernel's word on who is
// on the other end — a client cannot forge it.
func connPeerUID(c net.Conn) (string, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return "", false
	}
	var (
		uid   uint32
		found bool
		err   error
	)
	raw, err := uc.SyscallConn()
	if err != nil {
		return "", false
	}
	cerr := raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		cred, err = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err == nil && cred != nil {
			uid = cred.Uid
			found = true
		}
	})
	if cerr != nil || err != nil {
		return "", false
	}
	return strconv.FormatUint(uint64(uid), 10), found
}
