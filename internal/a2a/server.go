package a2a

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/skills"
	"github.com/charmbracelet/crush/internal/version"
)

// shutdownTimeout bounds the A2A host's graceful shutdown: the HTTP
// server stops accepting, in-flight requests drain, and the Serve
// goroutine is reaped even when a client holds a stream open.
const shutdownTimeout = 5 * time.Second

// maxUnixSocketPathLen is the maximum length of a Unix domain socket
// path. The macOS sun_path field is 104 bytes; Linux allows 108. The
// limit is 104 so a path that fits here is portable across both
// platforms.
const maxUnixSocketPathLen = 104

// a2aURLHost is the Host header every A2A request must carry. The URL
// never leaves the process: the card endpoint is a routing label, and
// the client's dialer maps it onto the process's unix socket.
const a2aURLHost = "crush-a2a"

// a2aSocketDirName is the directory under the data directory that holds
// the A2A host socket.
const a2aSocketDirName = "a2a"

// agentsPathPrefix is the path prefix under which the host routes
// requests to a dispatch's JSON-RPC handler.
const agentsPathPrefix = "/agents/"

// ServerParams are the inputs for serving one dispatched agent over A2A
// (#70): the runner drives the agent's session, Diff and Todos feed the
// executor's artifact and progress events, and Name/Description/Skills
// shape the served AgentCard.
type ServerParams struct {
	// DispatchID is the registry entry's id the served dispatch answers
	// as (#346). Required: the host routes /agents/<DispatchID> here.
	DispatchID string
	// Runner is the dispatched agent's SessionAgent — the a2a.Runner
	// slice of it. Required.
	Runner Runner
	// SessionID is the ephemeral task session backing the dispatch.
	// Required.
	SessionID string
	// Diff collects the completion artifact. Optional; the a2a.GitDiff
	// or dispatch.Workspace.Diff contracts both fit.
	Diff DiffFunc
	// Todos streams per-session progress as TaskStatusUpdateEvents
	// (#174). Optional; the production source is the dispatch
	// registry's todo collector.
	Todos TodoSource
	// Name is the agent's display name on the card — the dispatch's
	// @handle. Defaults to a generic label.
	Name string
	// Description is the agent's one-line card description — the
	// dispatch's role.
	Description string
	// Skills are the Crush skills the dispatch was given; each becomes
	// one A2A skill entry on the card.
	Skills []*skills.Skill
	// Version advertised on the card. Defaults to the build version.
	Version string
	// Call is the template every served turn runs with (#71) — the
	// dispatch's full call shaping; the prompt is overridden per message.
	Call agent.SessionAgentCall
	// InactivityTimeout is the A2A-level backstop (#360): a served run
	// that yields no events for this long is ended by the executor with
	// a Failed status carrying the reason. 0 (the default) disables it;
	// when set, the SDK's own inactivity guard also runs, one minute
	// later, as an outer net for a wedged executor.
	InactivityTimeout time.Duration
	// CancelReason reports why the dispatched run was killed (#316), for
	// the Canceled status an out-of-band cancel emits (#342). Optional;
	// the production value is the dispatch run's kill reason. A nil func
	// or an empty string falls back to "canceled".
	CancelReason func() string
}

// Server is one dispatched agent's slice of the process-wide A2A host
// (#346): the endpoint and card the dispatch registry advertises, plus
// the teardown that unregisters the dispatch's route. The socket, the
// HTTP server and the route table belong to the [ServerFactory].
type Server struct {
	// Endpoint is the card URL the dispatch serves:
	// http://<a2aURLHost>/agents/<dispatch id>.
	Endpoint string
	// Card is the served AgentCard — the same object registered on the
	// dispatch registry entry, so in-memory discovery can never disagree
	// with what a client dials.
	Card *a2aspec.AgentCard

	factory  *ServerFactory
	id       string
	stopOnce sync.Once
}

// Stop unregisters the dispatch's route from the process host, canceling
// the route's context so any in-flight stream on it ends. Safe to call
// more than once; the host keeps serving the other dispatches.
func (s *Server) Stop(_ context.Context) error {
	s.stopOnce.Do(func() {
		s.factory.unregister(s.id)
	})
	return nil
}

// ServerFactory hosts every in-process A2A server for dispatched agents
// (#70, #346) on the coordinator's behalf: one unix socket per process
// at <data dir>/a2a/<pid>.sock (0700 directory, 0600 socket), started
// lazily on the first [ServerFactory.StartServer], with a mutex-guarded
// route table mapping /agents/<dispatch id> onto each dispatch's
// JSON-RPC handler. It implements the agent package's
// [agent.DispatchServerStarter] seam, which is how the dependency stays
// one-way: a2a imports agent, never the reverse.
type ServerFactory struct {
	dataDir string

	mu         sync.Mutex
	started    bool
	closed     bool
	listener   net.Listener
	sockPath   string
	httpServer *http.Server
	done       chan struct{}
	routes     map[string]*route
}

// route is one dispatch's slice of the host: its JSON-RPC handler and a
// context that dies with the dispatch, so Stop (or Close) ends any
// in-flight stream served for it.
type route struct {
	handler http.Handler
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewServerFactory returns the production server factory rooted at
// dataDir, where the host socket and its directory are created.
func NewServerFactory(dataDir string) *ServerFactory {
	return &ServerFactory{dataDir: dataDir, routes: make(map[string]*route)}
}

// StartServer serves one dispatched agent over A2A (#70): it builds the
// card and the Executor behind a2asrv's JSON-RPC handler, lazily starts
// the process host on the factory's unix socket, and registers the
// dispatch's route. Stop unregisters it; the caller owns the lifecycle
// (the dispatch run).
func (f *ServerFactory) StartServer(ctx context.Context, p ServerParams) (*Server, error) {
	if p.Runner == nil {
		return nil, errors.New("a2a: server requires a runner")
	}
	if p.SessionID == "" {
		return nil, errors.New("a2a: server requires a session id")
	}
	if p.DispatchID == "" {
		return nil, errors.New("a2a: server requires a dispatch id")
	}

	// The card's version is required by the spec; default to the build
	// version when the caller did not pick one.
	cardVersion := p.Version
	if cardVersion == "" {
		cardVersion = version.Version
	}

	endpoint := "http://" + a2aURLHost + agentsPathPrefix + p.DispatchID
	card := BuildAgentCard(CardParams{
		Agent:     config.Agent{Name: p.Name, Description: p.Description},
		Skills:    p.Skills,
		Endpoint:  endpoint,
		Version:   cardVersion,
		Transport: a2aspec.TransportProtocolJSONRPC,
	})

	var opts []Option
	if p.Diff != nil {
		opts = append(opts, WithDiff(p.Diff))
	}
	if p.Todos != nil {
		opts = append(opts, WithTodos(p.Todos))
	}
	if p.InactivityTimeout > 0 {
		opts = append(opts, WithInactivityTimeout(p.InactivityTimeout))
	}
	if p.CancelReason != nil {
		opts = append(opts, WithCancelReason(p.CancelReason))
	}
	opts = append(opts, WithCallTemplate(p.Call))
	executor := NewExecutor(p.Runner, p.SessionID, opts...)

	// The SDK's inactivity guard (#360's outer net): when the backstop
	// is armed, it runs one minute after the executor's — the executor's
	// reason-bearing Failed is expected to land first; this only fires
	// for a wedged executor, and writes its own causeless Failed.
	var handlerOpts []a2asrv.RequestHandlerOption
	if p.InactivityTimeout > 0 {
		handlerOpts = append(handlerOpts, a2asrv.WithAgentInactivityTimeout(p.InactivityTimeout+time.Minute))
	}
	handler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor, handlerOpts...))

	if err := f.ensureHost(ctx); err != nil {
		return nil, err
	}
	if err := f.register(p.DispatchID, handler); err != nil {
		return nil, err
	}
	return &Server{Endpoint: endpoint, Card: card, factory: f, id: p.DispatchID}, nil
}

// StartDispatchServer implements [agent.DispatchServerStarter]: it
// registers the dispatch's route on the process host and returns its
// endpoint, the AgentCard to stamp on the dispatch registry entry, and
// the stop function the dispatch run defers. The card is returned as the
// registry's opaque any so the agent package never imports the a2a
// types.
func (f *ServerFactory) StartDispatchServer(ctx context.Context, p agent.DispatchServerParams) (string, any, func(), error) {
	server, err := f.StartServer(ctx, ServerParams{
		DispatchID:        p.DispatchID,
		Runner:            p.Runner,
		SessionID:         p.SessionID,
		Diff:              p.Diff,
		Todos:             p.Todos,
		Name:              p.Name,
		Description:       p.Description,
		Skills:            p.Skills,
		Version:           version.Version,
		Call:              p.Call,
		InactivityTimeout: p.InactivityTimeout,
		CancelReason:      p.CancelReason,
	})
	if err != nil {
		return "", nil, nil, err
	}
	stop := func() {
		_ = server.Stop(context.Background())
	}
	return server.Endpoint, server.Card, stop, nil
}

// Close shuts the process host down (idempotent): every remaining
// route's context is canceled, in-flight requests drain within the
// caller's context, the listener is closed and the socket file is
// removed. App shutdown owns the call.
func (f *ServerFactory) Close(ctx context.Context) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	if !f.started {
		f.mu.Unlock()
		return nil
	}
	for id, rt := range f.routes {
		delete(f.routes, id)
		rt.cancel()
	}
	srv, listener, path, done := f.httpServer, f.listener, f.sockPath, f.done
	f.mu.Unlock()

	err := srv.Shutdown(ctx)
	if cerr := listener.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) && err == nil {
		err = cerr
	}
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	<-done
	return err
}

// ensureHost lazily starts the process-wide listener: the socket lives
// under the data directory (created 0700) and is named after the pid, so
// a stale file always belongs to a dead process and plain removal before
// the bind is safe; no live-server probing is needed. The socket is
// chmod-ed 0600 right after the bind (non-Windows), so only the same
// user can reach the unauthenticated JSON-RPC surface (#346).
func (f *ServerFactory) ensureHost(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started {
		return nil
	}
	if f.closed {
		return errors.New("a2a: host is closed")
	}

	path, err := a2aSocketPath(f.dataDir)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("a2a: remove stale socket: %w", err)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("a2a: bind unix listener: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = listener.Close()
			return fmt.Errorf("a2a: chmod socket: %w", err)
		}
	}

	f.sockPath = path
	f.listener = listener
	f.httpServer = &http.Server{
		Handler:           http.HandlerFunc(f.serveHTTP),
		ReadHeaderTimeout: 30 * time.Second,
	}
	f.done = make(chan struct{})
	f.started = true
	go func() {
		defer close(f.done)
		// Serve always returns a non-nil error (ErrServerClosed on a
		// clean Close); anything else means the host died early.
		if err := f.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("A2A host server died early", "error", err)
		}
	}()
	return nil
}

// a2aSocketPath returns where the process host binds its socket:
// under the data directory when the path fits the 104-byte sun_path
// limit, otherwise under a per-user, 0700 directory in [os.TempDir].
func a2aSocketPath(dataDir string) (string, error) {
	uid := "unknown"
	if usr, err := user.Current(); err == nil && usr.Uid != "" {
		uid = usr.Uid
	}
	name := fmt.Sprintf("%d.sock", os.Getpid())
	dir := filepath.Join(dataDir, a2aSocketDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("a2a: create socket dir: %w", err)
	}
	path := filepath.Join(dir, name)
	if len(path) <= maxUnixSocketPathLen {
		return path, nil
	}
	dir = filepath.Join(os.TempDir(), "crush-a2a-"+uid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("a2a: create fallback socket dir: %w", err)
	}
	return filepath.Join(dir, name), nil
}

// serveHTTP is the host's single root handler (#346): middleware first,
// rejecting cross-origin, non-JSON and wrong-host requests before any
// dispatch work runs, then the route table maps /agents/<dispatch id>
// onto that dispatch's JSON-RPC handler. The middleware exists because
// the served surface is unauthenticated (#357 adds auth): a browser page
// can CSRF a text/plain POST at any loopback port, DNS-rebind its Host,
// and fold text into a running agent's turn. The route context is
// injected so a Stop or Close cancels the in-flight streams it owns.
func (f *ServerFactory) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		http.Error(w, "a2a: cross-origin requests are not accepted", http.StatusForbidden)
		return
	}
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
		http.Error(w, "a2a: only application/json requests are accepted", http.StatusUnsupportedMediaType)
		return
	}
	if r.Host != a2aURLHost {
		http.Error(w, "a2a: unexpected Host", http.StatusBadRequest)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, agentsPathPrefix)
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	rt, ok := f.routeFor(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rt.handler.ServeHTTP(w, r.WithContext(rt.ctx))
}

// register adds the dispatch's route with a context that dies with it.
func (f *ServerFactory) register(id string, handler http.Handler) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("a2a: host is closed")
	}
	if _, ok := f.routes[id]; ok {
		return fmt.Errorf("a2a: dispatch %s is already served", id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.routes[id] = &route{handler: handler, ctx: ctx, cancel: cancel}
	return nil
}

// unregister removes the dispatch's route and cancels its context.
func (f *ServerFactory) unregister(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rt, ok := f.routes[id]; ok {
		delete(f.routes, id)
		rt.cancel()
	}
}

// routeFor returns the dispatch's route under the host lock.
func (f *ServerFactory) routeFor(id string) (*route, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.routes[id]
	return rt, ok
}

// socketPath returns the unix socket path the factory's host listens
// on; empty before the first StartServer.
func (f *ServerFactory) socketPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sockPath
}

// Resolve returns the AgentCard and endpoint a dispatch registry entry
// carries (#70): in-memory discovery over the dispatch registry — an
// in-process A2A client resolves a dispatch's card from its entry
// without a network hop. ok=false when the entry serves nothing (no
// server, already torn down, or a card stamped by something else).
func Resolve(entry dispatch.Entry) (*a2aspec.AgentCard, string, bool) {
	if entry.Endpoint == "" || entry.AgentCard == nil {
		return nil, "", false
	}
	card, ok := entry.AgentCard.(*a2aspec.AgentCard)
	if !ok || card == nil {
		return nil, "", false
	}
	return card, entry.Endpoint, true
}

// Compile-time proof that the factory satisfies the agent-side seam.
var _ agent.DispatchServerStarter = (*ServerFactory)(nil)
