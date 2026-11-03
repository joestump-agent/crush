package log

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPRoundTripLoggerStreamsResponse(t *testing.T) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
	}{
		{"event stream", "text/event-stream; charset=utf-8", `{}`},
		{"stream request", "application/json", `{"stream":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, "first\n")
				w.(http.Flusher).Flush()
				<-release
				_, _ = io.WriteString(w, "last\n")
			}))
			defer server.Close()
			defer close(release)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan string, 1)
			restResult := make(chan string, 1)
			errs := make(chan error, 1)
			go func() {
				resp, err := NewHTTPClient().Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer resp.Body.Close()
				reader := bufio.NewReader(resp.Body)
				first, err := reader.ReadString('\n')
				if err != nil {
					errs <- err
					return
				}
				result <- first
				rest, err := io.ReadAll(reader)
				if err != nil {
					errs <- err
					return
				}
				restResult <- string(rest)
			}()

			select {
			case first := <-result:
				if first != "first\n" {
					t.Fatalf("first chunk = %q", first)
				}
			case err := <-errs:
				t.Fatal(err)
			case <-time.After(3 * time.Second):
				t.Fatal("client did not receive the response before the stream finished")
			}
			release <- struct{}{}
			select {
			case rest := <-restResult:
				if rest != "last\n" {
					t.Fatalf("remaining chunk = %q", rest)
				}
			case err := <-errs:
				t.Fatal(err)
			case <-time.After(3 * time.Second):
				t.Fatal("client did not receive the rest of the stream")
			}
		})
	}
}

func TestLoggedStreamBodyBoundsAndCloses(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	source := &testStreamSource{Reader: strings.NewReader(strings.Repeat("x", maxLoggedStreamBytes+10))}
	body := &loggedStreamBody{ReadCloser: source}
	data, err := io.ReadAll(body)
	if err != nil || len(data) != maxLoggedStreamBytes+10 {
		t.Fatalf("read %d bytes, error = %v", len(data), err)
	}
	if err := body.Close(); err != nil || !source.closed {
		t.Fatalf("close error = %v, source closed = %v", err, source.closed)
	}
	if body.buf.Len() != maxLoggedStreamBytes || !body.truncated {
		t.Fatalf("logged bytes = %d, truncated = %v", body.buf.Len(), body.truncated)
	}
	if strings.Count(logs.String(), "HTTP Response stream") != 1 || !strings.Contains(logs.String(), "truncated=true") {
		t.Fatal("stream must be logged once with the truncation flag")
	}
}

func TestLoggedStreamBodyPreservesErrors(t *testing.T) {
	want := errors.New("read failure")
	source := &testStreamSource{readErr: want}
	body := &loggedStreamBody{ReadCloser: source}
	_, err := body.Read(make([]byte, 1))
	if !errors.Is(err, want) {
		t.Fatalf("read error = %v, want %v", err, want)
	}
	if err := body.Close(); err != nil || !source.closed {
		t.Fatalf("close error = %v, source closed = %v", err, source.closed)
	}
}

func TestLoggedStreamBodyEarlyClose(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	want := errors.New("close failure")
	source := &testStreamSource{Reader: strings.NewReader("remaining"), closeErr: want}
	body := &loggedStreamBody{ReadCloser: source}
	if err := body.Close(); !errors.Is(err, want) || !source.closed {
		t.Fatalf("close error = %v, source closed = %v", err, source.closed)
	}
	if strings.Count(logs.String(), "HTTP Response stream") != 1 {
		t.Fatal("early close must log once")
	}
}

type testStreamSource struct {
	io.Reader
	readErr  error
	closeErr error
	closed   bool
}

func (s *testStreamSource) Read(p []byte) (int, error) {
	if s.readErr != nil {
		return 0, s.readErr
	}
	return s.Reader.Read(p)
}

func (s *testStreamSource) Close() error {
	s.closed = true
	return s.closeErr
}

func TestHTTPRoundTripLogger(t *testing.T) {
	// Create a test server that returns a 500 error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Header", "test-value")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Internal server error", "code": 500}`))
	}))
	defer server.Close()

	// Create HTTP client with logging
	client := NewHTTPClient()

	// Make a request
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL,
		strings.NewReader(`{"test": "data"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Verify response
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected status code 500, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != `{"error": "Internal server error", "code": 500}` {
		t.Fatalf("response body = %q, error = %v", body, err)
	}
}

func TestFormatHeaders(t *testing.T) {
	headers := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer secret-token"},
		"X-API-Key":     []string{"api-key-123"},
		"User-Agent":    []string{"test-agent"},
	}

	formatted := formatHeaders(headers)

	// Check that sensitive headers are redacted
	if formatted["Authorization"][0] != "[REDACTED]" {
		t.Error("Authorization header should be redacted")
	}
	if formatted["X-API-Key"][0] != "[REDACTED]" {
		t.Error("X-API-Key header should be redacted")
	}

	// Check that non-sensitive headers are preserved
	if formatted["Content-Type"][0] != "application/json" {
		t.Error("Content-Type header should be preserved")
	}
	if formatted["User-Agent"][0] != "test-agent" {
		t.Error("User-Agent header should be preserved")
	}
}
