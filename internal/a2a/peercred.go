package a2a

import (
	"context"
	"net"
)

// peerUIDContextKey stores the authenticated peer uid in a connection's
// context (#357).
type peerUIDContextKey struct{}

// withPeerCredentials is the [http.Server.ConnContext] hook: it reads
// the connected peer's uid off the unix socket — where the platform
// reports it — and stores it in the connection's context for the auth
// interceptor (#357).
func withPeerCredentials(ctx context.Context, c net.Conn) context.Context {
	uid, ok := connPeerUID(c)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, peerUIDContextKey{}, uid)
}

// peerUIDFromContext returns the uid the connection's peer authenticated
// as, when the platform reported socket credentials.
func peerUIDFromContext(ctx context.Context) (string, bool) {
	uid, ok := ctx.Value(peerUIDContextKey{}).(string)
	return uid, ok
}
