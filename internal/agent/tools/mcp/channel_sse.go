package mcp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The standalone SSE stream is the hanging GET that a streamable-HTTP MCP
// server uses to push server-initiated notifications. For channel-enabled
// servers every doorbell rides on it, so its state is health: a session that
// initializes fine but never opens (or loses) the stream is dead weight —
// the SDK answers pings over ordinary POSTs and cannot tell the difference.
//
// Interception note: channel notifications must be filtered below the go-sdk
// connection layer for streamable-HTTP transports. The SDK only starts the
// standalone SSE stream by type-asserting the connection returned from
// Transport.Connect to its internal connection type; wrapping the connection
// (as channelConn does for other transports) silently defeats that assert,
// so the stream is never opened and every doorbell is rejected server-side
// with "stream not connected or already closed".

// channelStreamHealth records the observable state of a channel server's
// standalone SSE stream, maintained by the filter round-tripper.
type channelStreamHealth struct {
	opened    atomic.Bool  // a standalone GET ever returned 200 text/event-stream
	active    atomic.Bool  // the most recent stream body is still being read
	closedAt  atomic.Int64 // unix milliseconds of the last stream body EOF/error
	firstOpen atomic.Int64 // unix milliseconds of the first successful stream open
}

// healthy reports whether the notification stream looks connected. A stream
// that closed recently is still considered healthy: the go-sdk reconnects it
// on its own, and health must not flap while that loop runs. closedGrace is
// how long a closed stream is given to come back before it counts as down.
func (h *channelStreamHealth) healthy(closedGrace time.Duration) bool {
	if !h.opened.Load() {
		return false
	}
	if h.active.Load() {
		return true
	}
	closed := h.closedAt.Load()
	if closed == 0 {
		return true
	}
	return time.Since(time.UnixMilli(closed)) < closedGrace
}

// channelStreamStates tracks stream health per channel MCP name. Entries are
// created when the filter is installed and consulted by the channel health
// check; a session is recreated when the stream has been down too long.
var channelStreamStates = csync.NewMap[string, *channelStreamHealth]()

// channelStreamClosedGrace is how long a closed stream is presumed to be
// mid-reconnect before the channel health check treats it as down. The SDK's
// reconnect loop retries with exponential backoff up to MaxRetries (5), so
// this comfortably exceeds a normal reconnect cycle.
const channelStreamClosedGrace = 2 * channelHealthCheckInterval

// installChannelSSEFilter wires the SSE-filtering round-tripper into a
// streamable-HTTP transport and registers the server's stream health state.
func installChannelSSEFilter(t *mcp.StreamableClientTransport, name string, gate *channelGate) {
	inner := http.RoundTripper(http.DefaultTransport)
	if t.HTTPClient != nil && t.HTTPClient.Transport != nil {
		inner = t.HTTPClient.Transport
	}
	health := &channelStreamHealth{}
	channelStreamStates.Set(name, health)
	t.HTTPClient = &http.Client{Transport: &channelSSEFilter{
		inner:  inner,
		name:   name,
		gate:   gate,
		health: health,
	}}
}

// channelSSEFilter is an http.RoundTripper that watches event-stream
// response bodies. It filters notifications/claude/channel events out of
// every stream (doorbells may ride any SSE body, in practice the standalone
// GET) and dispatches them through the channel gate, while recording stream
// health for the health check.
type channelSSEFilter struct {
	inner  http.RoundTripper
	name   string
	gate   *channelGate
	health *channelStreamHealth
}

func (f *channelSSEFilter) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.inner.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	standalone := req.Method == http.MethodGet &&
		req.Header.Get("Accept") == "text/event-stream"
	if resp.StatusCode != http.StatusOK ||
		!isEventStreamContentType(resp.Header.Get("Content-Type")) {
		return resp, nil
	}
	if standalone {
		now := time.Now().UnixMilli()
		f.health.opened.Store(true)
		f.health.active.Store(true)
		f.health.firstOpen.CompareAndSwap(0, now)
	}
	resp.Body = &channelSSEBody{
		ctx:        req.Context(),
		body:       resp.Body,
		filter:     f,
		standalone: standalone,
	}
	return resp, nil
}

func isEventStreamContentType(ct string) bool {
	const prefix = "text/event-stream"
	if len(ct) < len(prefix) {
		return false
	}
	ct = ct[:len(prefix)]
	for i := 0; i < len(ct); i++ {
		c := ct[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// maxUnboundedBuffer caps how many bytes are held waiting for an event
// boundary. A doorbell is capped far below this (parseChannelParams enforces
// maxChannelContentBytes), so anything this large is not a channel event and
// is forwarded to the SDK untouched rather than buffered forever.
const maxUnboundedBuffer = 1 << 20

// channelSSEBody filters one event-stream body. Complete events are checked
// for the channel notification method; matching events are dispatched to the
// gate and stripped, everything else passes through unchanged and in order.
type channelSSEBody struct {
	ctx        context.Context
	body       io.ReadCloser
	filter     *channelSSEFilter
	standalone bool

	out     bytes.Buffer // filtered bytes not yet consumed by the reader
	buf     []byte       // raw bytes awaiting an event boundary
	eof     bool
	readErr error
}

// Read returns filtered stream data, blocking on the underlying body only
// when no filtered bytes are available.
func (b *channelSSEBody) Read(p []byte) (int, error) {
	for b.out.Len() == 0 {
		if b.eof {
			if b.readErr != nil {
				return 0, b.readErr
			}
			return 0, io.EOF
		}
		if err := b.pump(); err != nil {
			return 0, err
		}
	}
	return b.out.Read(p)
}

// pump reads more raw bytes from the stream, extracts any complete events,
// and appends the filtered result to the output buffer.
func (b *channelSSEBody) pump() error {
	chunk := make([]byte, 4096)
	n, err := b.body.Read(chunk)
	if n > 0 {
		b.buf = append(b.buf, chunk[:n]...)
		b.extractEvents()
	}
	if err == nil {
		return nil
	}
	// Stream ended: flush anything left (a trailing event without a final
	// blank line) before propagating EOF/error to the reader.
	b.out.Write(b.buf)
	b.buf = nil
	b.eof = true
	b.readErr = err
	if b.standalone {
		b.filter.health.active.Store(false)
		b.filter.health.closedAt.Store(time.Now().UnixMilli())
	}
	return nil
}

// extractEvents drains complete events from buf. An SSE event ends at a
// blank line; both \n\n and \r\n\r\n are accepted as delimiters. If buf
// grows past maxUnboundedBuffer without a boundary the data is forwarded
// as-is: it cannot be a valid doorbell (those are size-capped) and holding
// it would stall the stream.
func (b *channelSSEBody) extractEvents() {
	for len(b.buf) > 0 {
		end := eventBoundary(b.buf)
		if end < 0 {
			if len(b.buf) > maxUnboundedBuffer {
				b.out.Write(b.buf)
				b.buf = nil
			}
			return
		}
		event := b.buf[:end]
		b.buf = b.buf[end:]
		b.handleEvent(event)
	}
}

// eventBoundary returns the length of the first event in data (including the
// terminating blank line), or -1 if no complete event is buffered.
func eventBoundary(data []byte) int {
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		return i + 4
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2
	}
	return -1
}

// handleEvent inspects one complete SSE event. Events whose data decodes to
// a notifications/claude/channel request are dispatched through the channel
// gate and dropped from the stream; all other events pass through to the
// SDK untouched.
func (b *channelSSEBody) handleEvent(event []byte) {
	raw := eventData(event)
	if raw == nil || !utf8.Valid(raw) || !bytes.Contains(raw, []byte(channelNotificationMethod)) {
		b.out.Write(event)
		return
	}
	msg, err := jsonrpc.DecodeMessage(raw)
	if err != nil {
		b.out.Write(event)
		return
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok || req.IsCall() || req.Method != channelNotificationMethod {
		b.out.Write(event)
		return
	}
	if parsed := b.filter.gate.accept(req.Params); parsed != nil {
		publishChannelMessage(b.ctx, b.filter.name, parsed)
	}
}

// eventData joins the data lines of an SSE event into the JSON-RPC payload.
// Returns nil for comments (keepalives) and events without data.
func eventData(event []byte) []byte {
	var lines [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found || string(name) != "data" {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		lines = append(lines, value)
	}
	if len(lines) == 0 {
		return nil
	}
	return bytes.Join(lines, []byte("\n"))
}

// Close implements io.Closer.
func (b *channelSSEBody) Close() error {
	if b.standalone {
		b.filter.health.active.Store(false)
		b.filter.health.closedAt.Store(time.Now().UnixMilli())
	}
	return b.body.Close()
}

// channelStreamReportable logs a compact stream-health summary for a channel
// server, used by the health check when the stream looks down.
func channelStreamReportable(name string, h *channelStreamHealth) {
	firstOpen := h.firstOpen.Load()
	slog.Warn("MCP channel notification stream not connected",
		"name", name,
		"everOpened", h.opened.Load(),
		"firstOpenUnixMilli", firstOpen)
}
