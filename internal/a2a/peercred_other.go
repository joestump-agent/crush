//go:build !linux && !darwin

package a2a

import "net"

// connPeerUID reports no credentials on platforms without a supported
// socket peer-credential API (Windows): the bearer token alone gates
// the host there, on top of the 0700 socket directory (#357).
func connPeerUID(c net.Conn) (string, bool) {
	return "", false
}
