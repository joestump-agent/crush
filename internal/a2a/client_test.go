package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/dispatch"
	"github.com/charmbracelet/crush/internal/session"
)

// fakeTodoSource emits one snapshot per subscription (#174 stand-in).
type fakeTodoSource struct {
	snapshot dispatch.TodoSnapshot
}

func (f *fakeTodoSource) SubscribeSessionTodos(ctx context.Context, sessionID string) <-chan dispatch.TodoSnapshot {
	ch := make(chan dispatch.TodoSnapshot, 1)
	ch <- f.snapshot
	return ch
}

// The client half of the protocol boundary (#71): StreamDispatch drives a
// served dispatch over the loopback wire — prompt out, SSE events back —
// and returns the terminal outcome with the agent's text, the artifact
// diff, and the observed Working progress count.
func TestStreamDispatchCompletedWithArtifactAndProgress(t *testing.T) {
	runner := &fakeRunner{result: textResult("done, two files changed")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return "--- a/x\n+++ b/x\n@@\n+changed", nil
		},
		Todos: &fakeTodoSource{snapshot: dispatch.TodoSnapshot{
			CurrentTodo: "wiring form validation",
			Todos:       []session.Todo{{Content: "wiring form validation", Status: session.TodoStatusInProgress}},
		}},
		Call: agent.SessionAgentCall{NonInteractive: true, MaxOutputTokens: 512},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	factory := NewServerFactory()
	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	// The call template flowed through the wire (#71): the served turn
	// carries the dispatch's shaping, not a minimal test call.
	require.True(t, runner.gotCall.NonInteractive)
	require.Equal(t, int64(512), runner.gotCall.MaxOutputTokens)
	require.Equal(t, "dispatch-session", runner.gotCall.SessionID)
	require.Equal(t, "fix the bug", runner.gotCall.Prompt)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "done, two files changed", outcome.Text)
	require.Contains(t, outcome.Diff, "+++ b/x")
	require.Greater(t, outcome.WorkingEvents, 0, "the SSE stream carried Working progress events")
}

// A failed run maps to the failed outcome with the failure text from the
// terminal status message.
func TestStreamDispatchFailedRun(t *testing.T) {
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{err: errors.New("provider exploded")},
		SessionID: "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusFailed, outcome.Status)
	require.Contains(t, outcome.Text, "provider exploded")
}

// A panic in the dispatched runner must surface as a failed outcome
// across the wire, not crash the server process (#345).
func TestStreamDispatchRunnerPanicFails(t *testing.T) {
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{panicValue: errors.New("adapter blew up")},
		SessionID: "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusFailed, outcome.Status)
	require.Contains(t, outcome.Text, "panicked")
}

// An out-of-band kill — the ladder or watchdog canceling the agent
// directly, no A2A CancelTask, live server context — must end the SSE
// stream quickly with the canceled outcome and the kill reason, never at
// the caller's deadline (#342).
func TestStreamDispatchOutOfBandCancelEnds(t *testing.T) {
	runner := &blockingCancelRunner{started: make(chan struct{}), kill: make(chan struct{})}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		CancelReason: func() string {
			return "wander kill: hard timeout"
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	go func() {
		<-runner.started
		close(runner.kill)
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	outcome, err := NewServerFactory().StreamDispatch(ctx, agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 5*time.Second, "the stream must end well before the caller's deadline")
	require.Equal(t, DispatchStatusCanceled, outcome.Status)
	require.Equal(t, "wander kill: hard timeout", outcome.Text)
}

// CancelDispatch routes the kill through the protocol's tasks/cancel
// (#348): the reason rides the request metadata, the blocked stream ends
// canceled carrying it, and exactly one cancel call is needed.
func TestCancelDispatchEndsStream(t *testing.T) {
	runner := &blockingCancelRunner{started: make(chan struct{}), kill: make(chan struct{})}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	factory := NewServerFactory()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	taskIDCh := make(chan string, 1)
	outcomeCh := make(chan agent.DispatchTransportOutcome, 1)
	errCh := make(chan error, 1)
	go func() {
		outcome, err := factory.StreamDispatch(ctx, agent.DispatchTransportParams{
			Endpoint: server.Endpoint,
			Card:     server.Card,
			Prompt:   "fix the bug",
			OnTask:   func(taskID string) { taskIDCh <- taskID },
		})
		outcomeCh <- outcome
		errCh <- err
	}()

	<-runner.started
	var taskID string
	select {
	case taskID = <-taskIDCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never named the task")
	}
	require.NotEmpty(t, taskID)
	require.NoError(t, factory.CancelDispatch(ctx, agent.DispatchCancelParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		TaskID:   taskID,
		Reason:   "hard timeout",
	}))

	var outcome agent.DispatchTransportOutcome
	select {
	case outcome = <-outcomeCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never ended after the cancel")
	}
	require.NoError(t, <-errCh)
	require.Equal(t, DispatchStatusCanceled, outcome.Status)
	require.Equal(t, "hard timeout", outcome.Text)
}

// CancelDispatch without a resolvable card or task ID is an error, not
// a silent success — the caller's fallback depends on it.
func TestCancelDispatchRejectsUnusableParams(t *testing.T) {
	require.Error(t, NewServerFactory().CancelDispatch(t.Context(), agent.DispatchCancelParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     "not-a-card",
		TaskID:   "task-1",
		Reason:   "hard timeout",
	}))
	require.Error(t, NewServerFactory().CancelDispatch(t.Context(), agent.DispatchCancelParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     &a2aspec.AgentCard{},
		TaskID:   "",
		Reason:   "hard timeout",
	}))
}

// An unreachable or bogus endpoint is a transport error before any
// terminal state, never a silent success.
func TestStreamDispatchTransportErrors(t *testing.T) {
	factory := NewServerFactory()

	// No card to build a client from.
	_, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     "not a card",
		Prompt:   "x",
	})
	require.ErrorContains(t, err, "no resolvable agent card")

	// A card advertising an endpoint nothing serves.
	dead := BuildAgentCard(CardParams{Agent: config.Agent{Name: "dead-agent"}, Endpoint: "http://127.0.0.1:1", Transport: a2aspec.TransportProtocolJSONRPC})
	_, err = factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     dead,
		Prompt:   "x",
	})
	require.Error(t, err)

	// An empty prompt never leaves the client.
	_, err = factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: "http://127.0.0.1:1",
		Card:     dead,
	})
	require.ErrorContains(t, err, "prompt is empty")
}

// An 11 MB diff survives the wire end to end (#361): the executor's
// chunks each stay under the SDK's SSE line cap, and the client
// reassembles them byte for byte onto the outcome.
func TestStreamDispatchLargeDiffRoundTrips(t *testing.T) {
	line := strings.Repeat("-", 4096) + "\n"
	var b strings.Builder
	for len(b.String()) < 11*1024*1024 {
		b.WriteString(line)
	}
	diff := b.String()

	runner := &fakeRunner{result: textResult("big change")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return diff, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "change everything",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Len(t, outcome.Diff, len(diff))
	require.Equal(t, diff, outcome.Diff, "the reassembled diff matches byte for byte")
	require.False(t, outcome.DiffTruncated)
	require.Empty(t, outcome.DiffError)
}

// A diff-capture error crosses the wire on the dispatch-result artifact:
// the run still completes, and the outcome carries the error instead of
// a diff (#361).
func TestStreamDispatchDiffErrorStillCompletes(t *testing.T) {
	runner := &fakeRunner{result: textResult("done, but the diff blew up")}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return "", errors.New("not a git repo")
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	outcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "not a git repo", outcome.DiffError)
	require.Empty(t, outcome.Diff)
}

// The production dispatch client carries no total Timeout (#344): the
// SDK's default three-minute http.Client.Timeout bounds the whole
// exchange including the SSE body, killing every served dispatch that
// runs longer. The per-phase bounds stay, so a dead server still fails
// fast.
func TestDispatchClientHasNoTotalTimeout(t *testing.T) {
	client := dispatchHTTPClient()
	require.Zero(t, client.Timeout, "a total Timeout would re-create the three-minute kill")

	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Greater(t, transport.ResponseHeaderTimeout, time.Duration(0))
	require.Greater(t, transport.TLSHandshakeTimeout, time.Duration(0))
}

// A served run outlives a short injected client deadline (#344): the
// response headers arrive inside the deadline, the SSE body streams
// past it, and the terminal outcome still lands.
func TestStreamDispatchOutlivesShortClientDeadline(t *testing.T) {
	runner := &fakeRunner{result: textResult("eventually done"), delay: time.Second}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Todos:     &fakeTodoSource{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	factory := NewServerFactory(WithHTTPClient(&http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 200 * time.Millisecond,
		},
	}))
	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "eventually done", outcome.Text)
}

// steppedTodoSource emits each snapshot after its delay from the
// subscription's start (#349), so a test can place todo Working events
// deterministically before or after a mid-run stream cut.
type steppedTodoSource struct {
	delays    []time.Duration
	snapshots []dispatch.TodoSnapshot
}

func (s *steppedTodoSource) SubscribeSessionTodos(ctx context.Context, _ string) <-chan dispatch.TodoSnapshot {
	ch := make(chan dispatch.TodoSnapshot, len(s.snapshots))
	go func() {
		defer close(ch)
		for i, snap := range s.snapshots {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.delays[i]):
			}
			select {
			case ch <- snap:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// cuttingTransport wraps the loopback transport for the resume tests
// (#349): the first SendStreamingMessage response body is cut after its
// first SSE event, and with failAfterCut every later request is severed
// before dialing — the give-up case. The JSON-RPC method of each
// request is sniffed to tell the stream from the resubscribe and
// tasks/get calls that follow it.
type cuttingTransport struct {
	base         http.RoundTripper
	failAfterCut bool

	mu  sync.Mutex
	cut bool
}

func (c *cuttingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	if c.cut && c.failAfterCut {
		c.mu.Unlock()
		return nil, errors.New("transport severed after the stream cut")
	}
	c.mu.Unlock()

	method := jsonrpcRequestMethod(req)

	c.mu.Lock()
	firstStream := method == "SendStreamingMessage" && !c.cut
	if firstStream {
		c.cut = true
	}
	c.mu.Unlock()

	resp, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if firstStream {
		resp.Body = &cutAfterFirstEventBody{ReadCloser: resp.Body}
	}
	return resp, nil
}

// jsonrpcRequestMethod reads a JSON-RPC request's method field and puts
// the body back for the real transport.
func jsonrpcRequestMethod(req *http.Request) string {
	if req.Body == nil {
		return ""
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return ""
	}
	var envelope struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &envelope)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return envelope.Method
}

// cutAfterFirstEventBody is an SSE response body that delivers exactly
// the stream's first event and then dies with io.ErrUnexpectedEOF —
// the dropped connection a resume recovers from (#349). Bytes past the
// first event boundary are lost with the connection.
type cutAfterFirstEventBody struct {
	io.ReadCloser

	primed bool
	buf    []byte
}

func (b *cutAfterFirstEventBody) Read(p []byte) (int, error) {
	if !b.primed {
		b.primed = true
		var collected []byte
		chunk := make([]byte, 4096)
		for {
			n, err := b.ReadCloser.Read(chunk)
			collected = append(collected, chunk[:n]...)
			if i := bytes.Index(collected, []byte("\n\n")); i >= 0 {
				b.buf = collected[:i+2]
				_ = b.Close()
				break
			}
			if err != nil {
				b.buf = collected
				break
			}
		}
	}
	if len(b.buf) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// resumeTestParams builds one dispatch server's params: a two-second
// run carrying a diff, with two todo snapshots at 500ms and 1s — both
// after the first resume backoff (250ms), so the replayed stream
// carries both Working events and only the pre-cut initial one is lost
// with the connection.
func resumeTestParams() ServerParams {
	return ServerParams{
		Runner:    &fakeRunner{result: textResult("resumed to the end"), delay: 2 * time.Second},
		SessionID: "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return "--- a/x\n+++ b/x\n@@\n+changed", nil
		},
		Todos: &steppedTodoSource{
			delays: []time.Duration{500 * time.Millisecond, time.Second},
			snapshots: []dispatch.TodoSnapshot{
				{CurrentTodo: "wiring the retry loop", Todos: []session.Todo{
					{Content: "wiring the retry loop", Status: session.TodoStatusInProgress},
				}},
				{CurrentTodo: "folding the replay", Todos: []session.Todo{
					{Content: "wiring the retry loop", Status: session.TodoStatusCompleted},
					{Content: "folding the replay", Status: session.TodoStatusInProgress},
				}},
			},
		},
	}
}

// A stream cut mid-run does not fail the dispatch (#349): the client
// resumes through tasks/resubscribe, folds the replayed snapshot plus
// the live events, and lands the same terminal outcome an uncut run
// reaches — text, diff, and a Working count that loses only the events
// the connection dropped and never counts one twice. The first event's
// task ID reaches OnTask exactly once, and tasks/get answers that ID
// with the stored task afterwards.
func TestStreamDispatchResumesAfterDrop(t *testing.T) {
	server, err := StartServer(t.Context(), resumeTestParams())
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	control, err := StartServer(t.Context(), resumeTestParams())
	require.NoError(t, err)
	t.Cleanup(func() { _ = control.Stop(context.Background()) })

	onTask := make(chan string, 2)
	cutFactory := NewServerFactory(WithHTTPClient(&http.Client{
		Transport: &cuttingTransport{base: http.DefaultTransport},
	}))
	outcome, err := cutFactory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
		OnTask:   func(taskID string) { onTask <- taskID },
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "resumed to the end", outcome.Text)
	require.Equal(t, "--- a/x\n+++ b/x\n@@\n+changed", outcome.Diff)
	require.Empty(t, outcome.DiffError)
	require.False(t, outcome.DiffTruncated)
	require.Equal(t, 2, outcome.WorkingEvents,
		"the resumed run counts exactly the two post-cut todo Working events")

	controlOutcome, err := NewServerFactory().StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: control.Endpoint,
		Card:     control.Card,
		Prompt:   "fix the bug",
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, controlOutcome.Status)
	require.Equal(t, 3, controlOutcome.WorkingEvents,
		"the uncut run counts the initial Working plus both todo snapshots")
	require.LessOrEqual(t, outcome.WorkingEvents, controlOutcome.WorkingEvents,
		"the resume must never double-count a Working event")

	require.Len(t, onTask, 1)
	taskID := <-onTask
	require.NotEmpty(t, taskID)

	status, err := cutFactory.GetDispatchTask(t.Context(), agent.GetDispatchTaskParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		TaskID:   taskID,
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, status.Status)
	require.Equal(t, "resumed to the end", status.Text)
}

// A run that finishes before the first resume attempt — the stream cut
// at the task's first event, the execution ended within the 250ms
// backoff — finds no active execution to resubscribe: the wrapped
// ErrTaskNotFound forks to tasks/get, whose stored task folds the
// terminal status, the text, and the artifacts into the outcome.
func TestStreamDispatchRecoversViaGetTask(t *testing.T) {
	runner := &fakeRunner{result: textResult("finished while cut"), delay: 100 * time.Millisecond}
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    runner,
		SessionID: "dispatch-session",
		Diff: func(ctx context.Context) (string, error) {
			return "--- a/y\n+++ b/y\n@@\n+late", nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	onTask := make(chan string, 2)
	factory := NewServerFactory(WithHTTPClient(&http.Client{
		Transport: &cuttingTransport{base: http.DefaultTransport},
	}))
	outcome, err := factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
		OnTask:   func(taskID string) { onTask <- taskID },
	})
	require.NoError(t, err)
	require.Equal(t, DispatchStatusCompleted, outcome.Status)
	require.Equal(t, "finished while cut", outcome.Text)
	require.Equal(t, "--- a/y\n+++ b/y\n@@\n+late", outcome.Diff)
	require.Len(t, onTask, 1)
	require.NotEmpty(t, <-onTask)
}

// When every resume attempt is severed — a wedged network after the
// stream dropped — the dispatch fails with the original stream error
// plus the exhaustion reason, after the full backoff ladder ran. The
// coordinator's cancel-before-teardown (#344) reaps the run from there.
func TestStreamDispatchGivesUpAfterRetries(t *testing.T) {
	server, err := StartServer(t.Context(), ServerParams{
		Runner:    &fakeRunner{result: textResult("never seen"), delay: 2 * time.Second},
		SessionID: "dispatch-session",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	onTask := make(chan string, 2)
	start := time.Now()
	factory := NewServerFactory(WithHTTPClient(&http.Client{
		Transport: &cuttingTransport{base: http.DefaultTransport, failAfterCut: true},
	}))
	_, err = factory.StreamDispatch(t.Context(), agent.DispatchTransportParams{
		Endpoint: server.Endpoint,
		Card:     server.Card,
		Prompt:   "fix the bug",
		OnTask:   func(taskID string) { onTask <- taskID },
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "a2a: dispatch stream")
	require.ErrorContains(t, err, "resume attempts exhausted")
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 5*time.Second,
		"the 250ms, 1s, 4s backoff ladder must run before giving up")
	require.Less(t, elapsed, 15*time.Second, "the give-up must not hang past the ladder")
	require.Len(t, onTask, 1, "the task ID was still reported before the resume began")
}
