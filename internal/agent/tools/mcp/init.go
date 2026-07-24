// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Crush application.
package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func parseLevel(level mcp.LoggingLevel) slog.Level {
	switch level {
	case "info":
		return slog.LevelInfo
	case "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// ClientSession wraps an mcp.ClientSession with a context cancel function so
// that the context created during session establishment is properly cleaned up
// on close.
type ClientSession struct {
	*mcp.ClientSession
	cancel context.CancelFunc
	// channel reports whether this server is an active channel (it declared
	// the claude/channel capability and was opted in via --channels).
	channel bool
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	s.cancel()
	return s.ClientSession.Close()
}

var (
	sessions = csync.NewMap[string, *ClientSession]()
	states   = csync.NewMap[string, ClientInfo]()

	// nameLocks serializes per-server lifecycle mutations — init, disable,
	// renew, refresh. Without it, two concurrent renewals for the same server
	// interleave: the loser's error path tears down the winner's healthy,
	// just-installed session, or the second success overwrites the first
	// session without closing it. Different servers never contend.
	nameLocks = csync.NewMap[string, *sync.Mutex]()
	// nameLocksMu makes mutex creation in nameLock atomic:
	// csync.Map.GetOrSet is check-then-act, so two first-op callers
	// could otherwise mint different mutexes and proceed unserialized.
	nameLocksMu sync.Mutex
	broker      = pubsub.NewBroker[Event]()
	initOnce    sync.Once
	initDone    = make(chan struct{})
)

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateError
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
	// EventChannelMessage is published when a channel server pushes a
	// notifications/claude/channel event. ChannelMessage carries the rendered,
	// escaped <channel> element ready for injection into the session.
	EventChannelMessage
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
	// ChannelMessage is set only for EventChannelMessage: the fully rendered
	// and escaped <channel>...</channel> element to inject into the session.
	ChannelMessage string
	ChannelMeta    map[string]string
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time
	// Channel reports whether this server is an active channel (declared the
	// claude/channel capability and opted in via --channels).
	Channel bool
}

// SubscribeEvents returns a channel for MCP events
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return broker.Subscribe(ctx)
}

// GetStates returns the current state of all MCP clients
func GetStates() map[string]ClientInfo {
	return states.Copy()
}

// GetState returns the state of a specific MCP client
func GetState(name string) (ClientInfo, bool) {
	return states.Get(name)
}

// Close closes all MCP clients. This should be called during application shutdown.
func Close(ctx context.Context) error {
	var wg sync.WaitGroup
	for name, session := range sessions.Seq2() {
		wg.Go(func() {
			done := make(chan error, 1)
			go func() {
				done <- session.Close()
			}()
			select {
			case err := <-done:
				if err != nil &&
					!errors.Is(err, io.EOF) &&
					!errors.Is(err, context.Canceled) &&
					err.Error() != "signal: killed" {
					slog.Warn("Failed to shutdown MCP client", "name", name, "error", err)
				}
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
	broker.Shutdown()
	return nil
}

// Initialize initializes MCP clients based on the provided configuration.
func Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore) {
	slog.Info("Initializing MCP clients")
	var wg sync.WaitGroup
	// Initialize states for all configured MCPs
	for name, m := range cfg.Config().MCP {
		if m.Disabled {
			updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping disabled MCP", "name", name)
			continue
		}

		// Set initial starting state
		wg.Add(1)
		go func(name string, m config.MCPConfig) {
			defer func() {
				wg.Done()
				if r := recover(); r != nil {
					var err error
					switch v := r.(type) {
					case error:
						err = v
					case string:
						err = fmt.Errorf("panic: %s", v)
					default:
						err = fmt.Errorf("panic: %v", v)
					}
					updateState(name, StateError, err, nil, Counts{})
					slog.Error("Panic in MCP client initialization", "error", err, "name", name)
				}
			}()

			if err := initClient(ctx, cfg, name, m, cfg.Resolver()); err != nil {
				slog.Debug("Failed to initialize MCP client", "name", name, "error", err)
			}
		}(name, m)
	}
	wg.Wait()
	initOnce.Do(func() { close(initDone) })
}

// WaitForInit blocks until MCP initialization is complete, i.e. until
// Initialize has finished and closed initDone. If Initialize is never called,
// initDone is never closed, so this blocks until ctx is cancelled and then
// returns ctx.Err(). Callers must therefore pass a context that is eventually
// cancelled (Initialize is spawned unconditionally during app startup, so in
// practice initDone always closes).
func WaitForInit(ctx context.Context) error {
	select {
	case <-initDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InitializeSingle initializes a single MCP client by name.
func InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	m, exists := cfg.Config().MCP[name]
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if m.Disabled {
		updateState(name, StateDisabled, nil, nil, Counts{})
		slog.Debug("Skipping disabled MCP", "name", name)
		return nil
	}

	return initClient(ctx, cfg, name, m, cfg.Resolver())
}

// nameLock returns the mutex serializing lifecycle mutations for one server.
func nameLock(name string) *sync.Mutex {
	nameLocksMu.Lock()
	defer nameLocksMu.Unlock()
	if mu, ok := nameLocks.Get(name); ok {
		return mu
	}
	mu := &sync.Mutex{}
	nameLocks.Set(name, mu)
	return mu
}

// initClient initializes a single MCP client with the given configuration.
func initClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver) error {
	mu := nameLock(name)
	mu.Lock()
	defer mu.Unlock()

	// Set initial starting state.
	updateState(name, StateStarting, nil, nil, Counts{})

	// createSession handles its own timeout internally.
	session, err := createSession(ctx, name, m, resolver, ChannelOptIn(m, cfg.Overrides().EnabledChannels, name))
	if err != nil {
		return err
	}

	toolCount, err := registerSessionTools(ctx, cfg, name, session)
	if err != nil {
		slog.Error("Error listing tools", "error", err)
		updateState(name, StateError, err, session, Counts{})
		return err
	}

	prompts, err := getPrompts(ctx, session)
	if err != nil {
		slog.Error("Error listing prompts", "error", err)
		updateState(name, StateError, err, session, Counts{})
		return err
	}

	updatePrompts(name, prompts)
	// A repeated init (e.g. enable called twice) must not overwrite a live
	// session without closing it — that leaks the child process and pipes.
	if old, ok := sessions.Take(name); ok && old != session {
		closeSession(name, old)
	}
	sessions.Set(name, session)

	updateState(name, StateConnected, nil, session, Counts{
		Tools:   toolCount,
		Prompts: len(prompts),
	})

	return nil
}

// DisableSingle disables and closes a single MCP client by name.
func DisableSingle(cfg *config.ConfigStore, name string) error {
	mu := nameLock(name)
	mu.Lock()
	defer mu.Unlock()

	if session, ok := sessions.Take(name); ok {
		closeSession(name, session)
	}

	// Clear tools, prompts, and resources for this MCP.
	updateTools(cfg, name, nil)
	updatePrompts(name, nil)
	updateResources(name, nil)

	// Update state to disabled.
	updateState(name, StateDisabled, nil, nil, Counts{})

	slog.Info("Disabled mcp client", "name", name)
	return nil
}

func getOrRenewClient(ctx context.Context, cfg *config.ConfigStore, name string) (*ClientSession, error) {
	// The whole ping→renew→re-register sequence runs under the per-name lock,
	// making renewal single-flight: a caller that raced a renewal re-pings the
	// fresh session the winner installed and returns it, instead of tearing it
	// down and building another.
	mu := nameLock(name)
	mu.Lock()
	defer mu.Unlock()

	sess, ok := sessions.Get(name)
	if !ok {
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}

	m := cfg.Config().MCP[name]
	state, _ := states.Get(name)

	timeout := mcpTimeout(m)
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := sess.Ping(pingCtx, nil)
	if err == nil {
		return sess, nil
	}
	// Report the failure against the exact session that failed the ping, so
	// only that session is closed and deregistered.
	updateState(name, StateError, maybeTimeoutErr(err, timeout), sess, state.Counts)

	fresh, err := createSession(ctx, name, m, cfg.Resolver(), ChannelOptIn(m, cfg.Overrides().EnabledChannels, name))
	if err != nil {
		return nil, err
	}

	// The StateError transition above cleared this server's tools and prompts
	// from the registry (and closed the dead session). Re-list and re-register
	// both on the fresh session; otherwise the agent reconnects but the LLM's
	// tool list stays empty and the next call fails with "tool not found",
	// and the commands menu keeps stale prompts.
	counts := state.Counts
	counts.Tools, err = registerSessionTools(ctx, cfg, name, fresh)
	if err != nil {
		updateState(name, StateError, err, fresh, state.Counts)
		return nil, err
	}

	prompts, err := getPrompts(ctx, fresh)
	if err != nil {
		slog.Warn("Error re-listing prompts after MCP renewal", "name", name, "error", err)
	}
	updatePrompts(name, prompts)
	counts.Prompts = len(prompts)

	sessions.Set(name, fresh)
	updateState(name, StateConnected, nil, fresh, counts)
	return fresh, nil
}

// closeSession closes an MCP session, logging only unexpected errors. EOF,
// context cancellation, and a killed child are the ordinary result of tearing
// a session down and are not worth surfacing.
func closeSession(name string, s *ClientSession) {
	if s == nil || s.ClientSession == nil {
		return
	}
	if err := s.Close(); err != nil &&
		!errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) &&
		err.Error() != "signal: killed" {
		slog.Warn("Error closing MCP session", "name", name, "error", err)
	}
}

// updateState updates the state of an MCP client and publishes an event
func updateState(name string, state State, err error, client *ClientSession, counts Counts) {
	info := ClientInfo{
		Name:    name,
		State:   state,
		Error:   err,
		Client:  client,
		Counts:  counts,
		Channel: client != nil && client.channel,
	}
	switch state {
	case StateConnected:
		info.ConnectedAt = time.Now()
	case StateError:
		// A session that has errored is dead to us: close it so the child
		// process and its stdio pipes are released, and clear its tool/prompt
		// registrations so the agent stops advertising capabilities it can no
		// longer call. Crucially, close exactly the session that errored (the
		// client argument): if the registry already holds a DIFFERENT session
		// — a newer, healthy one another path installed — leave it and its
		// registrations alone. Closing "whatever is in the map" here used to
		// let a stale error transition tear down a healthy replacement.
		switch {
		case client != nil:
			if cur, ok := sessions.Get(name); ok && cur == client {
				sessions.Del(name)
				allTools.Del(name)
				updatePrompts(name, nil)
				updateResources(name, nil)
			}
			closeSession(name, client)
		default:
			// No specific session errored (e.g. connect itself failed);
			// anything still registered under this name is unusable.
			if old, ok := sessions.Take(name); ok {
				closeSession(name, old)
			}
			allTools.Del(name)
			updatePrompts(name, nil)
			updateResources(name, nil)
		}
		// Never publish a dead session on the state.
		info.Client = nil
		info.Channel = false
	}
	states.Set(name, info)

	// Publish state change event
	broker.Publish(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
}

func createSession(ctx context.Context, name string, m config.MCPConfig, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error) {
	timeout := mcpTimeout(m)
	mcpCtx, cancel := context.WithCancel(ctx)
	cancelTimer := time.AfterFunc(timeout, cancel)

	transport, err := createTransport(mcpCtx, m, resolver)
	if err != nil {
		updateState(name, StateError, err, nil, Counts{})
		slog.Error("Error creating MCP client", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	// Wrap the transport so channel notifications can be intercepted. The gate
	// starts closed and is only opened below once the server is confirmed to
	// declare the channel capability and to have been opted in via --channels,
	// so a non-channel or non-enabled server can never inject content.
	channelGate := &atomic.Bool{}
	transport = &channelTransport{inner: transport, name: name, gate: channelGate}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "crush",
			Version: version.Version,
			Title:   "Crush",
		},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventToolsListChanged,
					Name: name,
				})
			},
			PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
				broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventPromptsListChanged,
					Name: name,
				})
			},
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				broker.Publish(pubsub.UpdatedEvent, Event{
					Type: EventResourcesListChanged,
					Name: name,
				})
			},
			LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
				level := parseLevel(req.Params.Level)
				slog.Log(ctx, level, "MCP log", "name", name, "logger", req.Params.Logger, "data", req.Params.Data)
			},
		},
	)

	session, err := client.Connect(mcpCtx, transport, nil)
	if err != nil {
		err = maybeStdioErr(err, transport)
		updateState(name, StateError, maybeTimeoutErr(err, timeout), nil, Counts{})
		slog.Error("MCP client failed to initialize", "error", err, "name", name)
		cancel()
		cancelTimer.Stop()
		return nil, err
	}

	cancelTimer.Stop()
	slog.Debug("MCP client initialized", "name", name)

	// Open the channel gate only for a server that both declares the
	// claude/channel capability and was opted in via --channels. Listed in MCP
	// config is not enough; this enforces the "listed is not enabled" model.
	isChannel := channelOptIn && hasChannelCapability(session.InitializeResult())
	if isChannel {
		channelGate.Store(true)
		slog.Info("MCP channel enabled", "name", name)
	}

	return &ClientSession{ClientSession: session, cancel: cancel, channel: isChannel}, nil
}

// maybeStdioErr if a stdio mcp prints an error in non-json format, it'll fail
// to parse, and the cli will then close it, causing the EOF error.
// so, if we got an EOF err, and the transport is STDIO, we try to exec it
// again with a timeout and collect the output so we can add details to the
// error.
// this happens particularly when starting things with npx, e.g. if node can't
// be found or some other error like that.
func maybeStdioErr(err error, transport mcp.Transport) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	// Every transport is wrapped in a channelTransport before Connect; the
	// stdio transport we're probing for is the inner one. Without this unwrap
	// the assertion below never matches and stdio startup failures report a
	// bare EOF instead of the child's actual output.
	if cw, ok := transport.(*channelTransport); ok {
		transport = cw.inner
	}
	ct, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
}

func maybeTimeoutErr(err error, timeout time.Duration) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

func createTransport(ctx context.Context, m config.MCPConfig, resolver config.VariableResolver) (mcp.Transport, error) {
	switch m.Type {
	case config.MCPStdio:
		command, err := resolver.ResolveValue(m.Command)
		if err != nil {
			return nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		args, err := m.ResolvedArgs(resolver)
		if err != nil {
			return nil, err
		}
		envs, err := m.ResolvedEnv(resolver)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, home.Long(command), args...)
		cmd.Env = append(os.Environ(), envs...)
		// Run the child in its own process group and kill the whole group when
		// the session context is cancelled. A stdio server often spawns its own
		// children (signal-mcp launches signal-cli); os/exec's default
		// cancellation kills only the direct child, orphaning the rest with
		// PPID 1 — production accumulated 15+ such zombies over two days.
		configureStdioProcess(cmd)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil
	case config.MCPHttp:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp http config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers: headers,
			},
		}
		transport := &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}
		// Enable OAuth for HTTP servers that don't have a static
		// Authorization header. This allows browser-based OAuth flows
		// (e.g. Cairn, other MCP servers using OIDC) to work
		// automatically: on 401, the SDK opens a browser for the user
		// to authenticate, captures the callback, and retries.
		if !hasAuthHeader(headers) {
			transport.OAuthHandler = newMCPOAuthHandler(url)
		}
		return transport, nil
	case config.MCPSSE:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp sse config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers: headers,
			},
		}
		return &mcp.SSEClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

type headerRoundTripper struct {
	headers map[string]string
}

// hasAuthHeader reports whether the headers map contains an
// Authorization key (case-insensitive). When true, the server is
// using static bearer-token auth and the OAuth handler is skipped.
func hasAuthHeader(headers map[string]string) bool {
	for k := range headers {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

func (rt headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func mcpTimeout(m config.MCPConfig) time.Duration {
	return time.Duration(cmp.Or(m.Timeout, 15)) * time.Second
}

func stdioCheck(old *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	// old.Args already includes argv0; passing it whole would duplicate the
	// program name in the re-exec.
	var args []string
	if len(old.Args) > 1 {
		args = old.Args[1:]
	}
	cmd := exec.CommandContext(ctx, old.Path, args...)
	cmd.Env = old.Env
	out, err := cmd.CombinedOutput()
	if err == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: %s", err, string(out))
}
