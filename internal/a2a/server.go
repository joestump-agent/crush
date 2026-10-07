package a2a

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aext"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/errordetails"

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
	// Questions is the dispatched agent's own question service (#352):
	// the executor parks the run in input-required on each question and
	// resolves it with the answer on the same task. Optional; nil means
	// the agent cannot ask.
	Questions QuestionSource
	// Listed puts the route on the host's agent index (#421) with its
	// card, handle, role and context, and tracks its task's state there.
	Listed bool
	// ParentSessionID is the session the dispatch was created from
	// (#399), carried on the index.
	ParentSessionID string
	// TaskStore persists served tasks durably (#354) instead of the
	// SDK's in-process default, so task state survives a restart.
	// Optional; the production store arrives with #355. nil keeps
	// today's in-memory behavior.
	TaskStore taskstore.Store
	// hooks are the executor's test-only seams; zero in production.
	hooks executorHooks
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
// JSON-RPC handler. The host also holds the per-process bearer token
// served calls must carry (#357) and the credential store the dispatch
// client's auth interceptor reads it from. It implements the agent
// package's [agent.DispatchHost] seam, which is how the dependency
// stays one-way: a2a imports agent, never the reverse.
type ServerFactory struct {
	dataDir string

	// contexts is the host's context registry (#350): the A2A context ID
	// each running dispatch owns, mapped to the binding its turns run
	// against. An executor resolves a task's context here at execute
	// time, so a run's end — which unbinds — is visible to the very next
	// message.
	contexts *ContextRegistry

	// token is the host's per-process bearer token (#357), generated
	// when the socket binds, read by the host auth interceptor through
	// authToken and attached to dispatch calls by the client side.
	token string

	// creds scopes the host token per dispatch session for the client
	// side's [a2aclient.AuthInterceptor] (#357).
	creds *a2aclient.InMemoryCredentialsStore

	// taskStore is the process-wide default served tasks persist to
	// (#355): the durable SQLite store rooted at the session database,
	// wired once by the app so the agent package never touches the
	// taskstore types. A ServerParams.TaskStore set per dispatch wins
	// over it (tests); when both are unset, served tasks keep the SDK's
	// in-process default and do not survive a restart.
	taskStore taskstore.Store

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

	// definitions is the card listing (#392): every agent definition
	// published on the host, keyed by its config id. Distinct from the
	// route table, which also carries per-run served agents.
	definitions map[string]*a2aspec.AgentCard

	// index is the agent index (#421): one descriptor per listed
	// dispatch, served at AgentsIndexPath.
	index *agentIndex
	// startedCh is closed once the host is up, so a watcher waiting to
	// dial its index (#421) wakes instead of polling for the socket.
	startedCh chan struct{}
	// indexClient is the one client the index is read through (#421).
	indexClientOnce sync.Once
	indexClient     *http.Client
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
	f := &ServerFactory{
		dataDir:     dataDir,
		contexts:    NewContextRegistry(),
		routes:      make(map[string]*route),
		definitions: make(map[string]*a2aspec.AgentCard),
		creds:       a2aclient.NewInMemoryCredentialsStore(),
		index:       newAgentIndex(),
		startedCh:   make(chan struct{}),
	}
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
	if p.Questions != nil {
		opts = append(opts, WithQuestions(p.Questions))
	}
	if p.Listed {
		dispatchID := p.DispatchID
		opts = append(opts, withOnTurn(func(taskID string) { f.index.setTask(dispatchID, taskID) }))
	}
	opts = append(opts, withHooks(p.hooks))
	// The call template rides the context binding (#350): the executor
	// resolves runner, session, and shaping together, per turn.
	executor := NewExecutor(f.contexts, p.ContextID, opts...)

	// The SDK's inactivity guard (#360's outer net): when the backstop
	// is armed, it runs one minute after the executor's — the executor's
	// reason-bearing Failed is expected to land first; this only fires
	// for a wedged executor, and writes its own causeless Failed.
	var handlerOpts []a2asrv.RequestHandlerOption
	// Capability checks (TCK VER-*/CORE-CAP-*): operations the card
	// does not declare answer the spec's error codes — GetExtendedAgentCard
	// answers UnsupportedOperationError (-32004), not the SDK's
	// not-configured (-32007) — and streaming/push stay gated on what
	// the card declares. The card is read-only once built.
	handlerOpts = append(handlerOpts, a2asrv.WithCapabilityChecks(&card.Capabilities))
	// The auth interceptor (#357): every served call carries the host's
	// bearer token and arrives from the host's own user, or it is
	// rejected before the executor runs.
	handlerOpts = append(handlerOpts, a2asrv.WithCallInterceptors(&hostAuthenticator{factory: f}))
	if p.InactivityTimeout > 0 {
		handlerOpts = append(handlerOpts, a2asrv.WithAgentInactivityTimeout(p.InactivityTimeout+time.Minute))
	}
	store := p.TaskStore
	if store == nil {
		store = f.taskStore
	}
	if p.Listed {
		// A listed dispatch's task writes also move its index
		// descriptor (#421). Without a store of its own the route gets
		// the SDK's default, built the way the SDK builds it.
		if store == nil {
			store = taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
				Authenticator: a2asrv.NewTaskStoreAuthenticator(),
			})
		}
		store = &trackingStore{Store: store, index: f.index, id: p.DispatchID}
	}
	if store != nil {
		handlerOpts = append(handlerOpts, a2asrv.WithTaskStore(store))
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
	handler := cancelRaceHandler{a2asrv.NewHandler(executor, handlerOpts...)}
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
	if p.Listed {
		f.index.add(AgentDescriptor{
			ID:              p.DispatchID,
			Endpoint:        endpoint,
			Card:            card,
			Handle:          p.Name,
			Role:            p.Description,
			ContextID:       p.ContextID,
			ParentSessionID: p.ParentSessionID,
		})
	}
	return &Server{Endpoint: endpoint, Card: card, factory: f, id: p.DispatchID, contextID: p.ContextID}, nil
}

// cancelRaceHandler reports the SDK's two outcomes of one race the same
// way (#352): a tasks/cancel that finds the task's execution still
// registered is routed into that execution's event pipe (a2a-go v2.5.0
// internal/taskexec/local_manager.go Cancel and
// handleCancelWithConcurrentRun). When the execution has already
// delivered its final event — a parked task's input-required — the
// Canceled the executor writes there is never processed. If the pipe was
// still open, the cancel resolves to the execution's own result and the
// SDK answers TaskNotCancelable (promise.go convertToCancelationResult,
// and the TODO at local_manager.go:391-396). If the execution had
// already closed its pipe on its way out, the write fails with
// eventqueue.ErrQueueClosed and the SDK answers an internal error. Both
// mean the same thing — the cancel lost to the execution's exit and the
// task still waits in its prior state — so the closed-pipe case is
// reported as TaskNotCancelable too, and the client's single re-issue
// (cancelTask) covers both. The two differ in one respect. A cancel that
// waited for the execution's result answers only after the execution
// was unregistered. A closed pipe proves only that the execution reached
// its last step: cleanupExecution closes the pipe, then takes the
// manager's lock to unregister. The re-issue arrives a full client round
// trip later; one that still beats that lock fails the same way, and its
// error stands.
type cancelRaceHandler struct {
	a2asrv.RequestHandler
}

// CancelTask implements [a2asrv.RequestHandler].
func (h cancelRaceHandler) CancelTask(ctx context.Context, req *a2aspec.CancelTaskRequest) (*a2aspec.Task, error) {
	task, err := h.RequestHandler.CancelTask(ctx, req)
	if errors.Is(err, eventqueue.ErrQueueClosed) {
		return nil, fmt.Errorf("%w: %w", a2aspec.ErrTaskNotCancelable, err)
	}
	return task, err
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

// WithTaskStore sets the store every served dispatch's tasks persist
// to by default (#355). The app passes the SQLite store over the
// session database, stamped with this process's HostID, so a crash no
// longer ends a dispatch's task history; the startup reconcile fails
// what the dead process left running. A per-dispatch ServerParams
// TaskStore still wins, which is the seam tests use.
func WithTaskStore(store taskstore.Store) ServerFactoryOption {
	return func(f *ServerFactory) { f.taskStore = store }
}

// StartDispatchServer implements [agent.DispatchHost]: it
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
		Questions:         p.Questions,
		Listed:            p.Listed,
		ParentSessionID:   p.ParentSessionID,
	})
	if err != nil {
		return "", nil, nil, err
	}
	stop := func() {
		_ = server.Stop(context.Background())
	}
	return server.Endpoint, server.Card, stop, nil
}

// PublishAgentDefinition implements [agent.AgentCatalog] (#392): it
// publishes one stable card per agent definition on the process host,
// at /agents/<ID>. The route is a live protocol surface — a message on
// it is rejected with the standard no-running-agent rejection until the
// definition's entry point binds a run to it — so the card listing and
// the served protocol can never disagree. Publishing an already-known
// ID keeps the first card and is not an error.
func (f *ServerFactory) PublishAgentDefinition(ctx context.Context, p agent.AgentDefinitionCard) error {
	if p.ID == "" {
		return errors.New("a2a: agent definition requires an id")
	}
	f.mu.Lock()
	if _, ok := f.definitions[p.ID]; ok {
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()

	endpoint := "http://" + a2aURLHost + agentsPathPrefix + p.ID
	card := BuildAgentCard(CardParams{
		Agent:     config.Agent{ID: p.ID, Name: p.Name, Description: p.Description},
		Endpoint:  endpoint,
		Version:   version.Version,
		Transport: a2aspec.TransportProtocolJSONRPC,
	})

	// The definition route carries no run: its executor resolves no
	// context (nil registry), so every message is rejected without a
	// runner until the definition's entry point serves a turn on it.
	executor := NewExecutor(nil, "")
	handler := a2asrv.NewHandler(executor)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(handler))

	if err := f.ensureHost(ctx); err != nil {
		return err
	}

	// Claim the route and the card under one lock: a concurrent publish
	// of the same definition loses cleanly — the first card wins and
	// definition routes are never replaced — which is the documented
	// no-op, not an error.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("a2a: host is closed")
	}
	if _, ok := f.routes[p.ID]; ok {
		return nil
	}
	if _, ok := f.definitions[p.ID]; ok {
		return nil
	}
	routeCtx, cancel := context.WithCancel(context.Background())
	f.routes[p.ID] = &route{handler: mux, ctx: routeCtx, cancel: cancel}
	if f.definitions == nil {
		f.definitions = make(map[string]*a2aspec.AgentCard)
	}
	f.definitions[p.ID] = card
	return nil
}

// AgentCards lists every agent-definition card published on the host
// (#392): the host's card listing. Per-run served agents (dispatches,
// sub-agent turns) are instances, not definitions — they are not part
// of the listing.
func (f *ServerFactory) AgentCards() []*a2aspec.AgentCard {
	f.mu.Lock()
	defer f.mu.Unlock()
	cards := make([]*a2aspec.AgentCard, 0, len(f.definitions))
	for _, card := range f.definitions {
		cards = append(cards, card)
	}
	return cards
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

	// Index streams (#421) never end on their own; end them first, or
	// Shutdown waits on them until ctx runs out.
	f.index.closeAll()
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
// under the data directory (created 0700) and is named after the pid —
// and, on the temp-dir fallback, a hash of the data directory — so a
// stale file always belongs to a dead process and plain removal before
// the bind is safe; no live-server probing is needed. The socket is
// chmod-ed 0600 right after the bind (non-Windows) and a per-process
// bearer token is minted for it (#357): only a caller holding the token
// — and, where the platform reports peer credentials, only one running
// as the same user — reaches the JSON-RPC surface.
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

	// The bearer token is minted when the host binds (#357): 32 bytes
	// of crypto/rand held only in memory, never persisted or logged.
	token, err := newHostToken()
	if err != nil {
		_ = listener.Close()
		return err
	}

	f.sockPath = path
	f.listener = listener
	f.token = token
	f.httpServer = &http.Server{
		Handler:           http.HandlerFunc(f.serveHTTP),
		ReadHeaderTimeout: 30 * time.Second,
		// The connection context carries the socket peer's uid, where
		// the platform reports one, for the auth interceptor (#357).
		ConnContext: withPeerCredentials,
	}
	f.done = make(chan struct{})
	f.started = true
	close(f.startedCh)
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
//
// Both paths are unique per (process, data directory). The primary path
// gets that from its directory; the fallback directory is shared by
// every data directory, so its file name carries a short hash of the
// data directory next to the pid. Without it, two hosts in one process
// rooted at different data directories would share one fallback path,
// and the second bind would remove the first host's live socket and
// take over its traffic.
//
// The fallback exists only to fit the path limit, so every component
// stays short: the user directory names a long uid by its hash, and the
// data directory hash is 8 hex characters.
func a2aSocketPath(dataDir string) (string, error) {
	uid := "unknown"
	if usr, err := user.Current(); err == nil && usr.Uid != "" {
		uid = usr.Uid
	}
	dir := filepath.Join(dataDir, a2aSocketDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("a2a: create socket dir: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.sock", os.Getpid()))
	if len(path) <= maxUnixSocketPathLen {
		return path, nil
	}
	dir = filepath.Join(os.TempDir(), "crush-a2a-"+fallbackUserTag(uid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("a2a: create fallback socket dir: %w", err)
	}
	name := fmt.Sprintf("%d-%s.sock", os.Getpid(), shortHash(fallbackDataDirKey(dataDir), 4))
	return filepath.Join(dir, name), nil
}

// maxFallbackUserTagLen is the longest uid the fallback directory names
// as is: any POSIX uid fits.
const maxFallbackUserTagLen = 12

// fallbackUserTag names the user in the fallback socket directory: the
// uid itself when it is short, otherwise 12 hex characters of its hash.
// A Windows SID runs to about 45 characters, which on its own nearly
// fills the 108-byte AF_UNIX path under the user's temp directory.
func fallbackUserTag(uid string) string {
	if len(uid) <= maxFallbackUserTagLen {
		return uid
	}
	return shortHash(uid, maxFallbackUserTagLen/2)
}

// fallbackDataDirKey is the data directory as the fallback socket name
// hashes it: absolute, so two spellings of one directory share a socket
// the way they share the primary path.
func fallbackDataDirKey(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return filepath.Clean(dataDir)
	}
	return abs
}

// shortHash returns the first n bytes of s's SHA-256, hex-encoded.
func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:n])
}

// serveHTTP is the host's single root handler (#346): middleware first,
// rejecting cross-origin, non-JSON, wrong-host and unsupported-protocol-version
// requests before any dispatch work runs, then the route table maps
// /agents/<dispatch id> onto that dispatch's JSON-RPC handler. The middleware
// exists because a browser page can CSRF a text/plain POST at any loopback
// port, DNS-rebind its Host, and fold text into a running agent's turn;
// requests that pass it still have to authenticate (#357): the route's call
// interceptor demands the host's bearer token — and the socket peer's own uid
// where the platform reports it — before the executor runs. The route context
// is injected so a Stop or Close cancels the in-flight streams it owns.
func (f *ServerFactory) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// The agent index (#421) is a GET with no JSON body, so it takes its
	// own copy of the checks below rather than the content-type gate.
	if r.URL.Path == AgentsIndexPath {
		f.serveAgentsIndex(w, r)
		return
	}
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
	if v := r.Header.Get(a2aspec.SvcParamVersion); v != "" && v != string(a2aspec.Version) {
		writeVersionNotSupported(w, r)
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
	// The route context replaces the request's connection context, so
	// the socket peer's uid the ConnContext hook read (#357) is copied
	// across the swap before it serves.
	serveCtx := rt.ctx
	if uid, ok := peerUIDFromContext(r.Context()); ok {
		serveCtx = context.WithValue(serveCtx, peerUIDContextKey{}, uid)
	}
	rt.handler.ServeHTTP(w, r.WithContext(serveCtx))
}

// writeVersionNotSupported answers a request carrying an A2A-Version the
// host does not serve with the spec's VersionNotSupportedError envelope
// (-32009), before the SDK handler runs: the answer is a plain JSON-RPC
// error even when the request asked for a stream, so a client cannot miss
// the rejection inside SSE framing.
func writeVersionNotSupported(w http.ResponseWriter, r *http.Request) {
	id := json.RawMessage("null")
	if body, err := io.ReadAll(r.Body); err == nil {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(body, &req) == nil && len(req.ID) > 0 {
			id = req.ID
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	info := errordetails.NewErrorInfo(
		a2aspec.ErrorReason(a2aspec.ErrVersionNotSupported),
		"a2a-protocol.org",
		map[string]string{"timestamp": time.Now().UTC().Format(time.RFC3339)},
	)
	envelope := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int               `json:"code"`
			Message string            `json:"message"`
			Data    []json.RawMessage `json:"data,omitempty"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	envelope.Error.Code = -32009
	envelope.Error.Message = a2aspec.ErrVersionNotSupported.Error()
	if data, err := json.Marshal(info); err == nil {
		envelope.Error.Data = []json.RawMessage{data}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envelope)
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
	// A listed dispatch stays on the index, no longer served (#421).
	f.index.unserve(id)
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

// authToken returns the host's bearer token (#357); empty before the
// first StartServer binds the socket.
func (f *ServerFactory) authToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

// SocketPath is socketPath's exported form: a caller customizing its
// own dispatch client through WithHTTPClient (#344) dials this path to
// stay on the host's socket instead of the endpoint's routing label.
func (f *ServerFactory) SocketPath() string {
	return f.socketPath()
}

// AuthToken is authToken's exported form: an in-process caller proxying
// the host — the TCK harness (#363) — injects it as the bearer
// credential the card's security scheme demands.
func (f *ServerFactory) AuthToken() string {
	return f.authToken()
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
// Compile-time proof that the factory satisfies the agent-side host
// seam (the transport half is proven in client.go).
var _ agent.DispatchHost = (*ServerFactory)(nil)
