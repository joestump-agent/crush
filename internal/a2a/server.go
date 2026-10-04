package a2a

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
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

// shutdownTimeout bounds a dispatch server's graceful shutdown: the HTTP
// server stops accepting, in-flight requests drain, and the Serve
// goroutine is reaped even when a client holds a stream open.
const shutdownTimeout = 5 * time.Second

// ServerParams are the inputs for serving one dispatched agent over A2A
// (#70): the runner drives the agent's session, Diff and Todos feed the
// executor's artifact and progress events, and Name/Description/Skills
// shape the served AgentCard.
type ServerParams struct {
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
}

// Server is one dispatched agent's in-process A2A server (#70): JSON-RPC
// over HTTP on an ephemeral loopback port, the Executor behind a2asrv,
// and the AgentCard at the well-known path. Phase 1 is loopback-only —
// the server exists so the coordinator (and, in #71, the DispatchAgent
// client) speaks the protocol boundary in-process; process isolation
// (#72) and remote workers (#73) come later.
type Server struct {
	// Endpoint is the base URL the server listens on; the card's
	// supported interface points here.
	Endpoint string
	// Card is the served AgentCard — the same object registered on the
	// dispatch registry entry, so in-memory discovery and the well-known
	// HTTP path can never disagree.
	Card *a2aspec.AgentCard

	listener net.Listener
	server   *http.Server
	done     chan struct{}
	stopOnce sync.Once
}

// StartServer stands up the loopback A2A server for one dispatch: it
// binds first (so the card advertises the real, already-bound endpoint),
// builds the card, wires the Executor behind a2asrv's JSON-RPC handler
// with the agent card at the well-known path, and serves on a goroutine.
// Stop shuts it down; the caller owns the lifecycle (the dispatch run).
func StartServer(ctx context.Context, p ServerParams) (*Server, error) {
	if p.Runner == nil {
		return nil, errors.New("a2a: server requires a runner")
	}
	if p.SessionID == "" {
		return nil, errors.New("a2a: server requires a session id")
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("a2a: bind loopback listener: %w", err)
	}

	// The card's version is required by the spec; default to the build
	// version when the caller did not pick one.
	cardVersion := p.Version
	if cardVersion == "" {
		cardVersion = version.Version
	}

	endpoint := "http://" + listener.Addr().String()
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
	handler := a2asrv.NewHandler(executor, handlerOpts...)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(handler))

	srv := &Server{
		Endpoint: endpoint,
		Card:     card,
		listener: listener,
		server:   &http.Server{Handler: mux},
		done:     make(chan struct{}),
	}
	go func() {
		defer close(srv.done)
		// Serve always returns a non-nil error (ErrServerClosed on a
		// clean Stop); anything else means the server died early.
		if err := srv.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return
		}
	}()
	return srv, nil
}

// Stop shuts the server down and waits for its Serve goroutine to exit,
// so a torn-down dispatch leaks neither the goroutine nor the port. Safe
// to call more than once.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		err = s.server.Shutdown(ctx)
		<-s.done
	})
	return err
}

// ServerFactory builds in-process A2A servers for dispatched agents
// (#70) on the coordinator's behalf. It implements the agent package's
// [agent.DispatchServerStarter] seam, which is how the dependency stays
// one-way: a2a imports agent, never the reverse.
type ServerFactory struct{}

// NewServerFactory returns the production server factory.
func NewServerFactory() *ServerFactory { return &ServerFactory{} }

// StartDispatchServer implements [agent.DispatchServerStarter]: it
// starts the loopback server and returns its endpoint, the AgentCard to
// register on the dispatch registry entry, and the stop function the
// dispatch run defers. The card is returned as the registry's opaque
// any so the agent package never imports the a2a types.
func (f *ServerFactory) StartDispatchServer(ctx context.Context, p agent.DispatchServerParams) (string, any, func(), error) {
	server, err := StartServer(ctx, ServerParams{
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
	})
	if err != nil {
		return "", nil, nil, err
	}
	stop := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Stop(shutdownCtx)
	}
	return server.Endpoint, server.Card, stop, nil
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
