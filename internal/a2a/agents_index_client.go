package a2a

// The agent index's client side (#421): read the host's index as a
// snapshot, or follow it as a stream of changes. A UI reaches the index
// through an [AgentIndexConn] — the host's own socket in local mode, the
// server's proxy in client/server mode — and never through the dispatch
// registry.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxIndexEventBytes bounds one index event on the wire. A snapshot
// carries a card per dispatch, a few KiB each.
const maxIndexEventBytes = 16 << 20

// AgentIndexConn is how a client reaches an agent index (#421): the HTTP
// client that dials it, the base URL its paths are under, and the bearer
// token to send. Token is empty when something in between — the
// server's proxy — authenticates on the client's behalf.
type AgentIndexConn struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
}

// Started is closed once the host is up and its index can be dialed.
func (f *ServerFactory) Started() <-chan struct{} {
	return f.startedCh
}

// AgentIndexConn returns the connection to this host's own index over
// its unix socket. Before the host has started there is no socket, and
// ok is false.
func (f *ServerFactory) AgentIndexConn() (conn AgentIndexConn, ok bool) {
	f.mu.Lock()
	started, closed := f.started, f.closed
	f.mu.Unlock()
	if !started || closed {
		return AgentIndexConn{}, false
	}
	return AgentIndexConn{
		HTTP:    f.indexHTTPClient(),
		BaseURL: "http://" + a2aURLHost,
		Token:   f.authToken(),
	}, true
}

// indexHTTPClient returns the one client the host's index is read
// through, made on first use: a watcher reconnects and refreshes for the
// life of the process, and a client per dial would leave its idle
// connections open behind it.
func (f *ServerFactory) indexHTTPClient() *http.Client {
	f.indexClientOnce.Do(func() {
		client := f.dispatchHTTPClient()
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.IdleConnTimeout = 90 * time.Second
		}
		f.indexClient = client
	})
	return f.indexClient
}

// request builds a GET for the index.
func (c AgentIndexConn) request(ctx context.Context, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+AgentsIndexPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if c.Token != "" {
		req.Header.Set(bearerAuthorizationHeader, "Bearer "+c.Token)
	}
	return req, nil
}

// ListAgents reads the index once.
func (c AgentIndexConn) ListAgents(ctx context.Context) ([]AgentDescriptor, error) {
	req, err := c.request(ctx, "application/json")
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: agent index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("a2a: agent index: %s", resp.Status)
	}
	var out []AgentDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("a2a: agent index: %w", err)
	}
	return out, nil
}

// WatchAgents follows the index: the first event is the snapshot, then
// one event per descriptor change. The channel closes when the stream
// ends — ctx canceled, the host closed, or the connection dropped — and
// the caller reconnects for a fresh snapshot.
func (c AgentIndexConn) WatchAgents(ctx context.Context) (<-chan AgentIndexEvent, error) {
	req, err := c.request(ctx, "text/event-stream")
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: agent index stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("a2a: agent index stream: %s", resp.Status)
	}
	out := make(chan AgentIndexEvent)
	go streamIndexEvents(ctx, resp, out)
	return out, nil
}

// streamIndexEvents reads an index stream's response into out until the
// stream or ctx ends, then closes the response and out.
func streamIndexEvents(ctx context.Context, resp *http.Response, out chan<- AgentIndexEvent) {
	defer close(out)
	defer resp.Body.Close()
	_ = readIndexEvents(resp.Body, func(ev AgentIndexEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	})
}

// readIndexEvents parses an SSE body into index events, handing each to
// emit until emit returns false or the body ends. Comments (keep-alives)
// and fields other than data are skipped.
func readIndexEvents(body io.Reader, emit func(AgentIndexEvent) bool) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), maxIndexEventBytes)
	var data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		switch {
		case len(line) == 0:
			if data.Len() == 0 {
				continue
			}
			var ev AgentIndexEvent
			err := json.Unmarshal(data.Bytes(), &ev)
			data.Reset()
			if err != nil {
				return fmt.Errorf("a2a: agent index event: %w", err)
			}
			if !emit(ev) {
				return nil
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
