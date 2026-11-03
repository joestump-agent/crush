package log

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// NewHTTPClient creates an HTTP client with debug logging enabled when debug mode is on.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &HTTPRoundTripLogger{
			Transport: http.DefaultTransport,
		},
	}
}

// HTTPRoundTripLogger is an http.RoundTripper that logs requests and responses.
type HTTPRoundTripLogger struct {
	Transport http.RoundTripper
}

// RoundTrip implements http.RoundTripper interface with logging.
func (h *HTTPRoundTripLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	var err error
	var save io.ReadCloser
	save, req.Body, err = drainBody(req.Body)
	if err != nil {
		slog.Error(
			"HTTP request failed",
			"method", req.Method,
			"url", req.URL,
			"error", err,
		)
		return nil, err
	}
	requestBody, err := io.ReadAll(save)
	if err != nil {
		return nil, err
	}
	var requestOptions struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(requestBody, &requestOptions)

	if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		slog.Debug(
			"HTTP Request",
			"method", req.Method,
			"url", req.URL,
			"body", bodyToString(io.NopCloser(bytes.NewReader(requestBody))),
		)
	}

	start := time.Now()
	resp, err := h.Transport.RoundTrip(req)
	duration := time.Since(start)
	if err != nil {
		slog.Error(
			"HTTP request failed",
			"method", req.Method,
			"url", req.URL,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
		return resp, err
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" || requestOptions.Stream {
		if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
			slog.Debug("HTTP Response", "status_code", resp.StatusCode, "status", resp.Status,
				"headers", formatHeaders(resp.Header), "content_length", resp.ContentLength,
				"duration_ms", duration.Milliseconds())
			if resp.Body != nil && resp.Body != http.NoBody {
				resp.Body = &loggedStreamBody{ReadCloser: resp.Body}
			}
		}
		return resp, nil
	}

	save, resp.Body, err = drainBody(resp.Body)
	if err != nil {
		slog.Error("Failed to drain response body", "error", err)
		return resp, err
	}
	if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		slog.Debug(
			"HTTP Response",
			"status_code", resp.StatusCode,
			"status", resp.Status,
			"headers", formatHeaders(resp.Header),
			"body", bodyToString(save),
			"content_length", resp.ContentLength,
			"duration_ms", duration.Milliseconds(),
		)
	}
	return resp, nil
}

const maxLoggedStreamBytes = 64 * 1024

type loggedStreamBody struct {
	io.ReadCloser
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
	once      sync.Once
}

func (b *loggedStreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	remaining := maxLoggedStreamBytes - b.buf.Len()
	if n > remaining {
		b.truncated = true
	}
	if n > 0 && remaining > 0 {
		_, _ = b.buf.Write(p[:min(n, remaining)])
	}
	b.mu.Unlock()
	if err != nil {
		b.log()
	}
	return n, err
}

func (b *loggedStreamBody) Close() error {
	err := b.ReadCloser.Close()
	b.log()
	return err
}

func (b *loggedStreamBody) log() {
	b.once.Do(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		slog.Debug("HTTP Response stream", "body", b.buf.String(), "truncated", b.truncated)
	})
}

func bodyToString(body io.ReadCloser) string {
	if body == nil {
		return ""
	}
	src, err := io.ReadAll(body)
	if err != nil {
		slog.Error("Failed to read body", "error", err)
		return ""
	}
	var b bytes.Buffer
	if json.Indent(&b, bytes.TrimSpace(src), "", "  ") != nil {
		// not json probably
		return string(src)
	}
	return b.String()
}

// formatHeaders formats HTTP headers for logging, filtering out sensitive information.
func formatHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		// Filter out sensitive headers
		if strings.Contains(lowerKey, "authorization") ||
			strings.Contains(lowerKey, "api-key") ||
			strings.Contains(lowerKey, "token") ||
			strings.Contains(lowerKey, "secret") {
			filtered[key] = []string{"[REDACTED]"}
		} else {
			filtered[key] = values
		}
	}
	return filtered
}

func drainBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	if b == nil || b == http.NoBody {
		return http.NoBody, http.NoBody, nil
	}
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, b, err
	}
	if err = b.Close(); err != nil {
		return nil, b, err
	}
	return io.NopCloser(&buf), io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}
