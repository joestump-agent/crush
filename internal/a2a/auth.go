package a2a

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// bearerSchemeName is the card's security scheme name — the name the
// dispatch client stores its credential under and the served calls are
// authenticated against (#357).
const bearerSchemeName = a2aspec.SecuritySchemeName("crush-bearer")

// bearerAuthorizationHeader is the HTTP header the bearer credential
// rides, both directions (#357).
const bearerAuthorizationHeader = "Authorization"

// hostTokenBytes is the entropy in the per-process bearer token: 32
// bytes of crypto/rand, base64url-encoded (#357).
const hostTokenBytes = 32

// newHostToken generates the process host's bearer token: 32 bytes of
// crypto/rand, base64url. It lives only in the ServerFactory's memory —
// never persisted, never logged (#357).
func newHostToken() (string, error) {
	buf := make([]byte, hostTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("a2a: generate host token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hostAuthenticator is the server-side [a2asrv.CallInterceptor] (#357):
// every served call must carry the host's bearer token and — where the
// platform reports socket peer credentials — arrive from the host's own
// user. On success the call is marked authenticated, which is what the
// task store's authorizer reads for tasks/list.
type hostAuthenticator struct {
	a2asrv.PassthroughCallInterceptor
	factory *ServerFactory
}

var _ a2asrv.CallInterceptor = (*hostAuthenticator)(nil)

// Before implements [a2asrv.CallInterceptor]: an unauthenticated call
// is rejected with a2a.ErrUnauthenticated — the handler is never
// invoked — and an authenticated one is stamped with the peer's
// identity on the call context.
func (h *hostAuthenticator) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	decision := authDecision{
		token:         h.factory.authToken(),
		wantUID:       strconv.Itoa(os.Getuid()),
		authorization: authorizationHeader(callCtx),
	}
	if peerUID, ok := peerUIDFromContext(ctx); ok {
		decision.peerKnown = true
		decision.peerUID = peerUID
	}
	if err := decision.authorize(); err != nil {
		return ctx, nil, err
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(decision.userName(), nil)
	return ctx, nil, nil
}

// authorizationHeader extracts the request's Authorization header from
// the call context's service parameters — the JSON-RPC transport copies
// the HTTP headers there when it opens the call (#357).
func authorizationHeader(callCtx *a2asrv.CallContext) []string {
	if callCtx == nil {
		return nil
	}
	vals, _ := callCtx.ServiceParams().Get(bearerAuthorizationHeader)
	return vals
}

// authDecision is the pure authentication decision one served call
// faces (#357): the request must carry the host's bearer token, and —
// when the platform reported socket peer credentials — the peer must be
// the host's own user. Everything is injected, so the rejected-uid case
// a single process cannot produce on its own live socket is
// unit-testable.
type authDecision struct {
	// token is the host's bearer token.
	token string
	// wantUID is the uid the peer must present when credentials are
	// known.
	wantUID string
	// peerKnown reports whether the platform reported peer credentials;
	// false disables the uid half of the decision (Windows).
	peerKnown bool
	// peerUID is the peer's uid, meaningful only when peerKnown.
	peerUID string
	// authorization is the request's Authorization header values.
	authorization []string
}

// authorize returns a2a.ErrUnauthenticated unless every required half
// of the check passes.
func (d authDecision) authorize() error {
	if !d.tokenMatches() {
		return a2aspec.ErrUnauthenticated
	}
	if d.peerKnown && d.peerUID != d.wantUID {
		return a2aspec.ErrUnauthenticated
	}
	return nil
}

// userName is the authenticated identity the check grants: the uid the
// peer presented, when it is known.
func (d authDecision) userName() string {
	if d.peerKnown {
		return "crush:" + d.peerUID
	}
	return "crush"
}

// tokenMatches compares the request's bearer credential with the
// host's token in constant time.
func (d authDecision) tokenMatches() bool {
	const prefix = "Bearer "
	if len(d.authorization) != 1 {
		return false
	}
	got, ok := strings.CutPrefix(d.authorization[0], prefix)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(d.token)) == 1
}
