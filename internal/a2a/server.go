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
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

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
// path. The macOS sun_path field is 104 bytes and must also hold the
// trailing NUL, so a 104-byte path fails bind with EINVAL there; Linux
// allows 108. The limit is 103 so a path that fits here binds on both
// platforms.
const maxUnixSocketPathLen = 103

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

// traceparentHeader is the W3C Trace Context header the dispatch client
// sends and the server propagator lifts into the request context (#364).
const traceparentHeader = "traceparent"

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
	// ContextID is the A2A context the dispatch's tasks carry (#350):
	// the client sends it as Message.ContextID and the executor resolves
	// it through the host's context registry to this dispatch's runner,
	// session, and call template. Production sets it to SessionID — the
	// A2A context IS the task session. Required: the binding lives from
	// StartServer until the dispatch's Server stops (handle lifetime =
	// run lifetime), and a task naming an unknown, foreign, or unbound
	// context is rejected.
	ContextID string
	// Diff collects the completion artifact. Optional; the a2a.GitDiff
	// or dispatch.WorkspaceProvider.Diff contracts both fit.
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
	// Usage reads the dispatched session's final usage once its run has
	// ended (#364). The executor attaches it to every post-run terminal
	// status under the usage/v1 extension. Optional; nil emits no usage
	// metadata.
	Usage func(ctx context.Context) (agent.Usage, error)
	// TaskStore persists served tasks durably (#354) instead of the
	// SDK's in-process default, so task state survives a restart.
	// Optional; the production store arrives with #355. nil keeps
	// today's in-memory behavior.
	TaskStore taskstore.Store
}

// Server is one dispatched agent's slice of the process-wide A2A host
// (#346): the endpoint and card the dispatch registry advertises, plus
// the teardown that unregisters the dispatch's route and unbinds its
// context (#350). The socket, the HTTP server, the route table, and the
// context registry belong to the [ServerFactory].
type Server struct {
	// Endpoint is the card URL the dispatch serves:
	// http://<a2aURLHost>/agents/<dispatch id>.
	Endpoint string
	// Card is the served AgentCard — the same object registered on the
	// dispatch registry entry, so in-memory discovery can never disagree
	// with what a client dials.
	Card *a2aspec.AgentCard

	factory   *ServerFactory
	id        string
	contextID string
	stopOnce  sync.Once
}

// Stop unregisters the dispatch's route from the process host and
// unbinds its A2A context (#350), canceling the route's context so any
// in-flight stream on it ends. Safe to call more than once; the host
// keeps serving the other dispatches.
func (s *Server) Stop(_ context.Context) error {
	s.stopOnce.Do(func() {
		s.factory.unregister(s.id, s.contextID)
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

	// contexts is the host's context registry (#350): the A2A context ID
	// each running dispatch owns, mapped to the binding its turns run
	// against. An executor resolves a task's context here at execute
	// time, so a run's end — which unbinds — is visible to the very next
	// message.
	contexts *ContextRegistry

	// httpClient, when set, replaces the factory's unix-socket dispatch
	// client in StreamDispatch — the test injection seam (#344), used
	// to bound phases of the wire protocol independently of the
	// defaults.
	httpClient *http.Client

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
func NewServerFactory(dataDir string, opts ...ServerFactoryOption) *ServerFactory {
	f := &ServerFactory{dataDir: dataDir, contexts: NewContextRegistry(), routes: make(map[string]*route)}
	for _, opt := range opts {
		opt(f)
	}
	return f
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
	if p.ContextID == "" {
		return nil, errors.New("a2a: server requires a context id")
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
	if p.Usage != nil {
		opts = append(opts, WithUsage(p.Usage))
	}
	// The call template rides the context binding (#350): the executor
	// resolves runner, session, and shaping together, per turn.
	executor := NewExecutor(f.contexts, p.ContextID, opts...)

	// The SDK's inactivity guard (#360's outer net): when the backstop
	// is armed, it runs one minute after the executor's — the executor's
	// reason-bearing Failed is expected to land first; this only fires
	// for a wedged executor, and writes its own causeless Failed.
	var handlerOpts []a2asrv.RequestHandlerOption
	if p.InactivityTimeout > 0 {
		handlerOpts = append(handlerOpts, a2asrv.WithAgentInactivityTimeout(p.InactivityTimeout+time.Minute))
	}
	if p.TaskStore != nil {
		handlerOpts = append(handlerOpts, a2asrv.WithTaskStore(p.TaskStore))
	}
	// The traceparent propagator (#364): the W3C traceparent the parent's
	// client interceptor sent moves into the request context, where the
	// executor reads it for the dispatch's server-side log lines and the
	// usage payload's trace_id.
	handlerOpts = append(handlerOpts, a2asrv.WithCallInterceptors(a2aext.NewServerPropagator(&a2aext.ServerPropagatorConfig{
		HeaderPredicate: func(_ context.Context, key string) bool {
			return strings.EqualFold(key, traceparentHeader)
		},
	})))
	handler := a2asrv.NewHandler(executor, handlerOpts...)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(handler))

	if err := f.ensureHost(ctx); err != nil {
		return nil, err
	}
	if err := f.register(p.DispatchID, mux); err != nil {
		return nil, err
	}
	// The context is bound for exactly the dispatch's lifetime (#350):
	// unbound when the dispatch's server stops, so the next message on
	// the context is rejected.
	f.contexts.Bind(p.ContextID, ContextBinding{
		Runner:    p.Runner,
		SessionID: p.SessionID,
		Call:      p.Call,
		Started:   time.Now(),
	})
	return &Server{Endpoint: endpoint, Card: card, factory: f, id: p.DispatchID, contextID: p.ContextID}, nil
}

// ServerFactoryOption customizes the server factory built by
// NewServerFactory.
type ServerFactoryOption func(*ServerFactory)

// WithHTTPClient injects the HTTP client StreamDispatch dials with
// (#344). Production leaves it unset and dials the factory's unix
// socket; tests use it to shorten a phase and prove the run outlives
// the transport's own deadline.
func WithHTTPClient(client *http.Client) ServerFactoryOption {
	return func(f *ServerFactory) { f.httpClient = client }
}

// StartDispatchServer implements [agent.DispatchServerStarter]: it
// registers the dispatch's route on the process host and returns its
// endpoint, the AgentCard to stamp on the dispatch registry entry, and
// the stop function the dispatch run defers. The card is returned as the
// registry's opaque any so the agent package never imports the a2a
// types.
func (f *ServerFactory) StartDispatchServer(ctx context.Context, p agent.DispatchServerParams) (string, any, func(), error) {
	server, err := f.StartServer(ctx, ServerParams{
		DispatchID: p.DispatchID,
		Runner:     p.Runner,
		SessionID:  p.SessionID,
		// The A2A context is the task session ID (#350): no translation
		// table, the client sends this value as Message.ContextID.
		ContextID:         p.SessionID,
		Diff:              p.Diff,
		Todos:             p.Todos,
		Name:              p.Name,
		Description:       p.Description,
		Skills:            p.Skills,
		Version:           version.Version,
		Call:              p.Call,
		InactivityTimeout: p.InactivityTimeout,
		CancelReason:      p.CancelReason,
		Usage:             p.Usage,
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
	// Every remaining binding dies with the host (#350): no dispatch is
	// running anymore, so no context resolves to anything.
	f.contexts.clear()
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

// unregister removes the dispatch's route and its context binding
// (#350), canceling the route's context.
func (f *ServerFactory) unregister(id, contextID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rt, ok := f.routes[id]; ok {
		delete(f.routes, id)
		rt.cancel()
	}
	f.contexts.Unbind(contextID)
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

// SocketPath is socketPath's exported form: a caller customizing its
// own dispatch client through WithHTTPClient (#344) dials this path to
// stay on the host's socket instead of the endpoint's routing label.
func (f *ServerFactory) SocketPath() string {
	return f.socketPath()
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

// ContextBinding is one running dispatch's entry in the host's context
// registry (#350): everything an arriving task's A2A ContextID resolves
// to while the dispatch runs — the runner that drives its session, the
// ephemeral session the context maps onto (the A2A context IS the task
// session), the call template its turns run with, and when the dispatch
// bound it.
type ContextBinding struct {
	Runner    Runner
	SessionID string
	Call      agent.SessionAgentCall
	Started   time.Time
}

// ContextRegistry maps an A2A context ID onto the dispatch binding that
// owns it (#350). The host holds one registry; a dispatch binds its
// context when its server starts and the binding is removed when the run
// ends (handle lifetime = run lifetime), so a message sent after the run
// ended — or one naming a context that belongs to another dispatch —
// resolves to nothing and the executor rejects it.
type ContextRegistry struct {
	mu       sync.RWMutex
	bindings map[string]ContextBinding
}

// NewContextRegistry returns an empty context registry.
func NewContextRegistry() *ContextRegistry {
	return &ContextRegistry{bindings: make(map[string]ContextBinding)}
}

// Bind registers the binding for contextID, replacing any previous one.
func (r *ContextRegistry) Bind(contextID string, binding ContextBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bindings == nil {
		r.bindings = make(map[string]ContextBinding)
	}
	r.bindings[contextID] = binding
}

// Unbind removes the context's binding; the next message on it resolves
// to nothing.
func (r *ContextRegistry) Unbind(contextID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bindings, contextID)
}

// Lookup resolves the binding registered for contextID.
func (r *ContextRegistry) Lookup(contextID string) (ContextBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	binding, ok := r.bindings[contextID]
	return binding, ok
}

// clear drops every binding. The host calls it when the whole factory
// shuts down; no dispatch outlives it.
func (r *ContextRegistry) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings = make(map[string]ContextBinding)
}

// Compile-time proof that the factory satisfies the agent-side seam.
var _ agent.DispatchServerStarter = (*ServerFactory)(nil)
