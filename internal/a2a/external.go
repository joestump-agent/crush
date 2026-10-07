package a2a

// External Agent Cards (#434): a runtime a2a agent definition names an
// Agent Card hosted elsewhere, and dispatches to it run through the
// same A2A client folding as a served dispatch. Everything about that
// remote is untrusted, so this file keeps three promises:
//
//   - The card is fetched without credentials, over https (plain http
//     only on loopback), with dial and total timeouts, never following a
//     redirect to another origin.
//   - Every later request is pinned to the card URL's origin: the HTTP
//     transport refuses any other origin outright, and the bearer token
//     is stamped by a call interceptor only for that origin. A card whose
//     service URL lives elsewhere is refused before anything carries the
//     token, so a tampered card cannot redirect it.
//   - The remote's output is data, not instructions: text is stripped of
//     control characters, capped, and scrubbed of the token; artifacts
//     are read as text and never as crush's diff; a request for input or
//     auth is refused by canceling the task, never forwarded.
//
// The host token (#357) never rides an external call: the external
// client has its own interceptor and credential, not the factory's.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
)

const (
	// externalDialTimeout bounds each TCP dial to an external agent.
	externalDialTimeout = 10 * time.Second
	// externalTLSHandshakeTimeout bounds each TLS handshake.
	externalTLSHandshakeTimeout = 10 * time.Second
	// externalCardTimeout is the card fetch's total budget, redirects
	// and body included. Task calls have none: a stream lives as long as
	// the run, bounded by the idle timeout and kill.timeout instead.
	externalCardTimeout = 30 * time.Second
	// externalCancelTimeout bounds the best-effort tasks/cancel sent when
	// a stream ends without a terminal state.
	externalCancelTimeout = 10 * time.Second
	// maxExternalCardBytes caps the card body: a card is a small JSON
	// document, and a hostile endpoint must not stream gigabytes into it.
	maxExternalCardBytes = 1 << 20
	// maxExternalBodyBytes caps every task call's response body, a whole
	// event stream included: the parent keeps at most
	// maxExternalTextBytes of it, so a remote that keeps talking past
	// this is cut off rather than buffered.
	maxExternalBodyBytes = 16 << 20
	// maxExternalEventBytes caps one server-sent event: the SDK joins an
	// event's data lines without a bound, so a stream that sends this
	// much with no event boundary fails instead of growing one string.
	maxExternalEventBytes = 4 << 20
	// maxExternalArtifacts caps the distinct artifacts an external
	// agent's output is collected from; later ones are dropped.
	maxExternalArtifacts = 64
	// maxExternalTextBytes caps the text an external agent hands the
	// parent — findings and failure reasons alike — so a remote cannot
	// flood the parent's context.
	maxExternalTextBytes = 32 * 1024
	// maxExternalRedirects caps same-origin redirects.
	maxExternalRedirects = 5
)

// externalTruncatedMarker closes external text cut at the byte cap.
const externalTruncatedMarker = "\n... (external agent output truncated)"

// WithExternalTransport sets the base HTTP transport external agent
// calls dial through (#434), under the origin pin. It is the test seam
// that lets an httptest TLS server's certificate be trusted; production
// leaves it unset, and each resolved external agent gets a transport of
// its own with dial, TLS-handshake and proxy settings.
func WithExternalTransport(rt http.RoundTripper) ServerFactoryOption {
	return func(f *ServerFactory) { f.externalTransport = rt }
}

// externalBaseTransport returns the transport one external agent's
// requests go out on: the injected test transport, else a fresh one.
// Plain http is allowed only for a loopback card, so for an http origin
// the transport uses no proxy and dials only addresses the host resolves
// to on loopback: a name such as localhost is checked where it resolves,
// not where it reads.
func (f *ServerFactory) externalBaseTransport(pinned origin) http.RoundTripper {
	if f.externalTransport != nil && pinned.scheme != "http" {
		return f.externalTransport
	}
	dialer := &net.Dialer{
		Timeout:   externalDialTimeout,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: externalTLSHandshakeTimeout,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
	}
	if pinned.scheme == "http" {
		lookup := f.externalLookup
		if lookup == nil {
			lookup = net.DefaultResolver.LookupIPAddr
		}
		transport.Proxy = nil
		transport.DialContext = loopbackOnlyDial(dialer, lookup)
	}
	return transport
}

// loopbackOnlyDial resolves the host itself and dials it only when every
// address it resolves to is loopback (#434), so plain http — allowed for
// a loopback card alone — never leaves the machine.
func loopbackOnlyDial(dialer *net.Dialer, lookup func(ctx context.Context, host string) ([]net.IPAddr, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("a2a: %s resolves to no address", host)
		}
		for _, ip := range ips {
			if !ip.IP.IsLoopback() {
				return nil, fmt.Errorf("a2a: refusing plain http to %s: it resolves to %s, not a loopback address", host, ip.IP)
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// ResolveExternalAgent implements [agent.ExternalAgentResolver] (#434).
// It fetches the card without credentials and checks, in order: the
// card URL itself (https, or http on loopback, no userinfo), a JSON-RPC
// interface on the card URL's origin, and — when a token is set — an
// HTTP bearer security scheme. Only then is a client built that can
// carry the token, and that client only ever talks to the card's
// origin. No error it returns contains the token: none was sent yet.
func (f *ServerFactory) ResolveExternalAgent(ctx context.Context, p agent.ExternalAgentParams) (agent.ExternalAgent, error) {
	cardURL, err := config.ValidateAgentCardURL(p.CardURL)
	if err != nil {
		return nil, fmt.Errorf("a2a: agent card URL %w", err)
	}
	source := cardURL.String()
	pinned := originOf(cardURL)
	base := f.externalBaseTransport(pinned)

	card, err := fetchExternalCard(ctx, base, pinned, source)
	if err != nil {
		return nil, err
	}
	iface, err := selectExternalInterface(card, pinned, source)
	if err != nil {
		return nil, err
	}
	if !p.Token.IsZero() && !declaresBearerScheme(card) {
		return nil, fmt.Errorf("a2a: agent card %s declares no HTTP bearer security scheme; refusing to send the configured bearer token", source)
	}

	httpClient := &http.Client{
		Transport: &pinnedTransport{
			base:          base,
			origin:        pinned,
			maxBody:       maxExternalBodyBytes,
			maxEventBytes: maxExternalEventBytes,
		},
		CheckRedirect: pinnedRedirects(pinned),
	}
	// The client sees only the pinned interface, so the SDK's transport
	// selection cannot wander to another URL the card listed.
	pinnedCard := *card
	pinnedCard.SupportedInterfaces = []*a2aspec.AgentInterface{iface}
	client, err := a2aclient.NewFromCard(ctx, &pinnedCard,
		a2aclient.WithJSONRPCTransport(httpClient),
		a2aclient.WithCallInterceptors(&externalAuthInterceptor{origin: pinned, token: p.Token}),
	)
	if err != nil {
		return nil, fmt.Errorf("a2a: agent card %s offers no supported transport: %s", source, untrustedText(err.Error(), p.Token))
	}
	return &externalAgent{
		client:   client,
		http:     httpClient,
		source:   source,
		endpoint: iface.URL,
		token:    p.Token,
	}, nil
}

// fetchExternalCard resolves the card with the SDK's resolver over an
// uncredentialed, origin-pinned client: a total timeout, a capped body,
// and no redirect off the card's origin.
func fetchExternalCard(ctx context.Context, base http.RoundTripper, pinned origin, source string) (*a2aspec.AgentCard, error) {
	client := &http.Client{
		Transport:     &pinnedTransport{base: base, origin: pinned, maxBody: maxExternalCardBytes},
		CheckRedirect: pinnedRedirects(pinned),
		Timeout:       externalCardTimeout,
	}
	card, err := agentcard.NewResolver(client).Resolve(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("a2a: resolve agent card %s: %s", source, untrustedText(err.Error(), agent.Secret{}))
	}
	return card, nil
}

// selectExternalInterface picks the card's first JSON-RPC interface on
// the card URL's origin. A card whose JSON-RPC service lives only on
// another origin is refused: the request, and its credential, go where
// the user pointed, or nowhere.
func selectExternalInterface(card *a2aspec.AgentCard, pinned origin, source string) (*a2aspec.AgentInterface, error) {
	var offered []string
	foreign := ""
	for _, iface := range card.SupportedInterfaces {
		if iface == nil {
			continue
		}
		offered = append(offered, string(iface.ProtocolBinding))
		if !strings.EqualFold(string(iface.ProtocolBinding), string(a2aspec.TransportProtocolJSONRPC)) {
			continue
		}
		u, err := url.Parse(iface.URL)
		if err != nil || !u.IsAbs() || u.Hostname() == "" {
			continue
		}
		if at := originOf(u); at != pinned {
			if foreign == "" {
				foreign = at.String()
			}
			continue
		}
		selected := *iface
		selected.ProtocolBinding = a2aspec.TransportProtocolJSONRPC
		return &selected, nil
	}
	if foreign != "" {
		return nil, fmt.Errorf("a2a: agent card %s names its JSON-RPC service on another origin (%s); refusing it so a tampered card cannot redirect requests or credentials", source, untrustedText(foreign, agent.Secret{}))
	}
	return nil, fmt.Errorf("a2a: agent card %s offers no supported transport: want %s on %s, card offers [%s]",
		source, a2aspec.TransportProtocolJSONRPC, pinned, untrustedText(strings.Join(offered, ", "), agent.Secret{}))
}

// declaresBearerScheme reports whether the card declares an HTTP bearer
// security scheme, the only scheme a definition's auth can satisfy.
func declaresBearerScheme(card *a2aspec.AgentCard) bool {
	for _, scheme := range card.SecuritySchemes {
		if s, ok := scheme.(a2aspec.HTTPAuthSecurityScheme); ok && strings.EqualFold(s.Scheme, "bearer") {
			return true
		}
	}
	return false
}

// origin is a URL's scheme, host, and port, normalized: the unit every
// external request is pinned to.
type origin struct {
	scheme string
	host   string
}

// originOf returns u's origin, lowercased, with the scheme's default
// port filled in so https://h and https://h:443 compare equal.
func originOf(u *url.URL) origin {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return origin{scheme: scheme, host: net.JoinHostPort(strings.ToLower(u.Hostname()), port)}
}

func (o origin) String() string { return o.scheme + "://" + o.host }

// pinnedTransport is the round tripper under every external request
// (#434): a request to any origin but the pinned one is refused before
// it is sent, whatever produced it — a tampered card, a redirect, or a
// future SDK path. maxBody, when positive, caps response bodies, and
// maxEventBytes, when positive, caps each server-sent event in a
// text/event-stream body.
type pinnedTransport struct {
	base          http.RoundTripper
	origin        origin
	maxBody       int64
	maxEventBytes int64
}

func (t *pinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if at := originOf(req.URL); at != t.origin {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("a2a: refusing a request to %s: this external agent is pinned to %s", at, t.origin)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if t.maxBody > 0 {
		resp.Body = http.MaxBytesReader(nil, resp.Body, t.maxBody)
	}
	if t.maxEventBytes > 0 && strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = &sseEventLimit{body: resp.Body, limit: t.maxEventBytes, lineEmpty: true}
	}
	return resp, nil
}

// errSSEEventTooLarge ends an event stream that sent too much without an
// event boundary.
var errSSEEventTooLarge = fmt.Errorf("a2a: external agent sent a server-sent event over %d bytes", maxExternalEventBytes)

// sseEventLimit wraps an event-stream body and fails the read once more
// than limit bytes arrive without an event boundary — a blank line, in
// any of the LF, CRLF and CR line endings the format allows.
type sseEventLimit struct {
	body  io.ReadCloser
	limit int64
	// since counts the bytes read since the last event boundary.
	since int64
	// lineEmpty reports that the current line has no characters yet;
	// a line ending that finds it set is a boundary.
	lineEmpty bool
	// afterCR reports that the previous byte was a CR, so an LF right
	// after it belongs to the same line ending.
	afterCR bool
}

func (s *sseEventLimit) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	for _, b := range p[:n] {
		switch {
		case b == '\n' && s.afterCR:
			s.afterCR = false
			continue
		case b == '\n' || b == '\r':
			s.afterCR = b == '\r'
			if s.lineEmpty {
				s.since = 0
				continue
			}
			s.lineEmpty = true
		default:
			s.afterCR = false
			s.lineEmpty = false
		}
		s.since++
		if s.since > s.limit {
			return 0, errSSEEventTooLarge
		}
	}
	return n, err
}

func (s *sseEventLimit) Close() error { return s.body.Close() }

// CloseIdleConnections forwards to the base transport, so closing the
// client releases its pooled connections.
func (t *pinnedTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// pinnedRedirects is the redirect policy for external requests: follow
// a few redirects within the pinned origin, never one off it.
func pinnedRedirects(pinned origin) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxExternalRedirects {
			return fmt.Errorf("stopped after %d redirects", maxExternalRedirects)
		}
		if at := originOf(req.URL); at != pinned {
			return fmt.Errorf("refusing a cross-origin redirect to %s", at)
		}
		return nil
	}
}

// externalAuthInterceptor stamps the external agent's bearer credential
// on every call (#434) — the client interceptor seam #357 introduced,
// with a credential of its own: the host token never rides an external
// call. A call whose base URL is off the pinned origin is refused before
// it is sent, token or not.
type externalAuthInterceptor struct {
	a2aclient.PassthroughInterceptor
	origin origin
	token  agent.Secret
}

// Before implements [a2aclient.CallInterceptor].
func (i *externalAuthInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	u, err := url.Parse(req.BaseURL)
	if err != nil || originOf(u) != i.origin {
		return ctx, nil, fmt.Errorf("a2a: refusing a call off the external agent's origin %s", i.origin)
	}
	if i.token.IsZero() {
		return ctx, nil, nil
	}
	if req.ServiceParams == nil {
		req.ServiceParams = a2aclient.ServiceParams{}
	}
	// Replace, never append: one credential, whatever the context held.
	delete(req.ServiceParams, bearerAuthorizationHeader)
	req.ServiceParams[strings.ToLower(bearerAuthorizationHeader)] = []string{"Bearer " + i.token.Reveal()}
	return ctx, nil, nil
}

// externalAgent implements [agent.ExternalAgent]: one resolved card and
// the pinned, credentialed client that drives it.
type externalAgent struct {
	client   *a2aclient.Client
	http     *http.Client
	source   string
	endpoint string
	token    agent.Secret
}

var _ agent.ExternalAgent = (*externalAgent)(nil)

// Compile-time proof the factory resolves external agents.
var _ agent.ExternalAgentResolver = (*ServerFactory)(nil)

func (a *externalAgent) Source() string   { return a.source }
func (a *externalAgent) Endpoint() string { return a.endpoint }

// Close releases the client's pooled connections.
func (a *externalAgent) Close() { a.http.CloseIdleConnections() }

// Stream implements [agent.ExternalAgent]. It sends the prompt as a new
// task — with no context ID, so the remote assigns its own and crush's
// session IDs stay local, and with no extensions activated — and folds
// the stream with the served dispatch's machinery: task-ID tracking and
// resubscribe on a dropped stream (#349). It ends on the first of:
//
//   - a terminal state, or a bare message reply: the natural outcome,
//     which wins over a kill that raced it;
//   - a kill or the idle timeout: the stream is cut, the task canceled
//     with the reason (#348), and the outcome is canceled with it;
//   - input-required or auth-required: refused, never forwarded — the
//     task is canceled and the outcome fails with the reason.
//
// Every text and error that leaves here is sanitized, capped, and
// scrubbed of the token.
func (a *externalAgent) Stream(ctx context.Context, p agent.ExternalDispatchParams) (agent.DispatchTransportOutcome, error) {
	if p.Prompt == "" {
		return agent.DispatchTransportOutcome{}, errors.New("a2a: dispatch prompt is empty")
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	watch := startStreamWatch(p.IdleTimeout, p.Kill, cancelStream)
	defer watch.stop()

	s := &dispatchStream{
		params:   agent.DispatchTransportParams{OnTask: p.OnTask},
		client:   a.client,
		external: &externalFold{},
		onEvent:  watch.touch,
	}
	req := &a2aspec.SendMessageRequest{
		Message: a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart(p.Prompt)),
	}
	err := s.consume(streamCtx, a.client.SendStreamingMessage(streamCtx, req))
	watch.stop()

	switch reason := watch.reason(); {
	case s.outcome.Status != "":
		return a.finish(s), nil
	case reason != "":
		a.cancelRemote(ctx, s.taskID, reason)
		return agent.DispatchTransportOutcome{
			Status:        DispatchStatusCanceled,
			Text:          reason,
			WorkingEvents: s.outcome.WorkingEvents,
		}, nil
	case s.question != nil:
		refusal := pauseRefusal(s.external.pausedOn)
		a.cancelRemote(ctx, s.taskID, refusal)
		return agent.DispatchTransportOutcome{
			Status:        DispatchStatusFailed,
			Text:          refusal,
			WorkingEvents: s.outcome.WorkingEvents,
		}, nil
	case err != nil:
		// The stream is gone and resume could not bring it back: the
		// remote task would run on unsupervised, its result unread.
		a.cancelRemote(ctx, s.taskID, "crush lost the stream")
		return agent.DispatchTransportOutcome{}, errors.New(untrustedText(err.Error(), a.token))
	default:
		a.cancelRemote(ctx, s.taskID, "the stream ended without a terminal state")
		return agent.DispatchTransportOutcome{}, errors.New("a2a: external agent stream ended without a terminal state")
	}
}

// pauseRefusal is the failure reason for an external task that paused
// for input or auth: crush never forwards an external agent's requests
// to the user's question or permission handlers.
func pauseRefusal(state a2aspec.TaskState) string {
	what := "input"
	if state == a2aspec.TaskStateAuthRequired {
		what = "authentication"
	}
	return fmt.Sprintf("the external agent asked for %s; crush does not forward an external agent's input, permission, or auth requests, so its task was canceled. Re-dispatch with a self-contained prompt", what)
}

// finish assembles the natural outcome: the terminal status text, then
// any artifact text, as one untrusted, capped, token-free text. Nothing
// else the remote sent survives: no diff, usage, or todo progress.
func (a *externalAgent) finish(s *dispatchStream) agent.DispatchTransportOutcome {
	text := s.outcome.Text
	if artifacts := s.external.text(); artifacts != "" {
		if text != "" {
			text += "\n\n"
		}
		text += artifacts
	}
	return agent.DispatchTransportOutcome{
		Status:        s.outcome.Status,
		Text:          untrustedText(text, a.token),
		WorkingEvents: s.outcome.WorkingEvents,
	}
}

// cancelRemote sends a best-effort tasks/cancel carrying reason (#348)
// for a task the stream no longer follows. It runs on a bounded context
// detached from the caller's, so a kill that ended the caller's context
// still reaches the remote.
func (a *externalAgent) cancelRemote(ctx context.Context, taskID a2aspec.TaskID, reason string) {
	if taskID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), externalCancelTimeout)
	defer cancel()
	_, err := cancelTask(ctx, a.client, &a2aspec.CancelTaskRequest{
		ID: taskID,
		Metadata: map[string]any{
			CancelReasonMetadataKey: CancelReason{Reason: reason},
		},
	})
	if err != nil {
		slog.Warn("External agent task cancel failed", "endpoint", a.endpoint, "task_id", taskID, "error", untrustedText(err.Error(), a.token))
	}
}

// externalFold collects an external agent's artifacts as text (#434):
// each artifact by ID in arrival order, appended or replaced as its
// updates say, under one byte budget. Nothing in it is ever read as a
// diff or written anywhere.
type externalFold struct {
	order []a2aspec.ArtifactID
	texts map[a2aspec.ArtifactID]string
	size  int
	// pausedOn is the state the task parked on — input-required or
	// auth-required — set when the stream pauses.
	pausedOn a2aspec.TaskState
}

// artifactUpdate folds one artifact update event.
func (f *externalFold) artifactUpdate(ev *a2aspec.TaskArtifactUpdateEvent) {
	if ev == nil {
		return
	}
	f.artifact(ev.Artifact, ev.Append)
}

// artifact folds one artifact's text parts, appending to the artifact's
// text so far when appendTo is set.
func (f *externalFold) artifact(art *a2aspec.Artifact, appendTo bool) {
	if art == nil {
		return
	}
	if f.texts == nil {
		f.texts = make(map[a2aspec.ArtifactID]string)
	}
	var b strings.Builder
	for _, part := range art.Parts {
		if part == nil {
			continue
		}
		if t := part.Text(); t != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
	}
	current, seen := f.texts[art.ID]
	if !seen {
		if len(f.order) >= maxExternalArtifacts {
			return
		}
		f.order = append(f.order, art.ID)
	}
	text := b.String()
	if appendTo {
		text = current + text
	}
	// The budget is shared: past it, text is cut rather than buffered,
	// and the final cap marks the truncation.
	if room := maxExternalTextBytes + 1 - (f.size - len(current)); len(text) > room {
		text = truncateUTF8(text, max(room, 0))
	}
	f.size += len(text) - len(current)
	f.texts[art.ID] = text
}

// snapshot folds a task snapshot: its artifacts replace everything
// collected so far — the snapshot is the server's accumulated state —
// and a terminal state lands when none has.
func (f *externalFold) snapshot(outcome *agent.DispatchTransportOutcome, task *a2aspec.Task) {
	if task == nil {
		return
	}
	f.order, f.texts, f.size = nil, nil, 0
	for _, art := range task.Artifacts {
		f.artifact(art, false)
	}
	if isTerminalTaskState(task.Status.State) && outcome.Status == "" {
		applyTaskSnapshot(outcome, task)
	}
}

// reply folds a bare message reply: the exchange is over, and the
// message is the result.
func (f *externalFold) reply(outcome *agent.DispatchTransportOutcome, msg *a2aspec.Message) {
	if outcome.Status != "" {
		return
	}
	outcome.Status = DispatchStatusCompleted
	outcome.Text = statusUpdateMessageText(&a2aspec.TaskStatusUpdateEvent{Status: a2aspec.TaskStatus{Message: msg}})
}

// text returns the collected artifact text, artifacts separated by a
// blank line.
func (f *externalFold) text() string {
	parts := make([]string, 0, len(f.order))
	for _, id := range f.order {
		if t := f.texts[id]; t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// untrustedText prepares remote-controlled text for the parent: invalid
// UTF-8 and control characters — terminal escapes, carriage returns,
// bidirectional overrides — are dropped, the token is scrubbed in case
// the remote echoed it back, and the result is capped.
func untrustedText(s string, token agent.Secret) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r):
			return -1
		}
		return r
	}, s)
	if !token.IsZero() {
		s = strings.ReplaceAll(s, token.Reveal(), "[REDACTED]")
	}
	if len(s) > maxExternalTextBytes {
		s = truncateUTF8(s, maxExternalTextBytes) + externalTruncatedMarker
	}
	return s
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// streamWatch ends an external stream (#434) when it stays silent for
// the idle timeout or when its run is killed, recording why. The idle
// timeout trips the run's kill switch with dispatch.ReasonIdleTimeout,
// so the coordinator reads every kill from one witness; a kill that is
// already recorded wins.
type streamWatch struct {
	touched chan struct{}
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once

	mu    sync.Mutex
	cause string
}

// startStreamWatch arms the watch: cancel cuts the stream. A zero idle
// timeout and a nil kill switch leave nothing to watch for.
func startStreamWatch(idle time.Duration, kill agent.RunKill, cancel context.CancelFunc) *streamWatch {
	w := &streamWatch{
		touched: make(chan struct{}, 1),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	var killed <-chan struct{}
	if kill != nil {
		killed = kill.Killed()
	}
	go func() {
		defer close(w.done)
		var idleC <-chan time.Time
		var timer *time.Timer
		if idle > 0 {
			timer = time.NewTimer(idle)
			defer timer.Stop()
			idleC = timer.C
		}
		for {
			select {
			case <-w.quit:
				return
			case <-w.touched:
				if timer != nil {
					timer.Reset(idle)
				}
			case <-idleC:
				reason := dispatch.ReasonIdleTimeout
				if kill != nil {
					kill.Kill(reason)
					reason = kill.Reason()
				}
				w.end(reason, cancel)
				return
			case <-killed:
				w.end(kill.Reason(), cancel)
				return
			}
		}
	}()
	return w
}

// touch records stream activity; it never blocks.
func (w *streamWatch) touch() {
	select {
	case w.touched <- struct{}{}:
	default:
	}
}

// end records the reason and cuts the stream.
func (w *streamWatch) end(reason string, cancel context.CancelFunc) {
	w.mu.Lock()
	w.cause = reason
	w.mu.Unlock()
	cancel()
}

// stop ends the watch and waits for it. Safe to call more than once.
func (w *streamWatch) stop() {
	w.once.Do(func() { close(w.quit) })
	<-w.done
}

// reason returns why the watch cut the stream, empty when it did not.
func (w *streamWatch) reason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cause
}
