package a2a

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	a2aspec "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/charmbracelet/crush/internal/agent"
)

// Dispatch transport status tokens (#71) — the protocol-side vocabulary
// the a2a package hands the coordinator, mapped there onto
// dispatch.Status. Plain strings because the agent package cannot import
// this one.
const (
	// DispatchStatusCompleted maps TaskStateCompleted.
	DispatchStatusCompleted = "completed"
	// DispatchStatusFailed maps TaskStateFailed and TaskStateRejected.
	DispatchStatusFailed = "failed"
	// DispatchStatusCanceled maps TaskStateCanceled.
	DispatchStatusCanceled = "canceled"
)

// newDispatchClient builds the A2A client for one dispatch: in-memory
// discovery from the dispatch's registered card, with the JSON-RPC
// transport wired to httpClient — the factory's injected client in
// tests, the factory's unix-socket client in production (#346).
func newDispatchClient(ctx context.Context, card *a2aspec.AgentCard, httpClient *http.Client) (*a2aclient.Client, error) {
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithJSONRPCTransport(httpClient))
	if err != nil {
		return nil, fmt.Errorf("a2a: client from card: %w", err)
	}
	return client, nil
}

// StreamDispatch drives one dispatch's initial task over the A2A
// protocol (#71): it builds a client from the dispatch's registered
// AgentCard — in-memory discovery, no directory hop — sends the prompt as
// a streaming message, and consumes the SSE event stream to its terminal
// state. The returned outcome carries the agent's final text (the
// terminal status message), the artifact (the diff), and how many
// Working progress events were observed on the wire.
//
// In-process the agent block keeps rendering from the dispatch
// registry's todo collector — the same snapshots the server re-emits as
// Working TaskStatusUpdateEvents (#174) — so the client counts but does
// not re-publish progress: no parallel progress path. The count proves
// the stream in tests and becomes the progress feed when #72/#73 move
// the agent out of the process.
func (f *ServerFactory) StreamDispatch(ctx context.Context, p agent.DispatchTransportParams) (agent.DispatchTransportOutcome, error) {
	card, ok := p.Card.(*a2aspec.AgentCard)
	if !ok || card == nil {
		return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch %s has no resolvable agent card", p.Endpoint)
	}
	if p.Prompt == "" {
		return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch prompt is empty")
	}
	httpClient := f.httpClient
	if httpClient == nil {
		httpClient = f.dispatchHTTPClient()
	}
	client, err := newDispatchClient(ctx, card, httpClient)
	if err != nil {
		return agent.DispatchTransportOutcome{}, err
	}

	req := &a2aspec.SendMessageRequest{
		Message: a2aspec.NewMessage(a2aspec.MessageRoleUser, a2aspec.NewTextPart(p.Prompt)),
	}

	var outcome agent.DispatchTransportOutcome
	for ev, err := range client.SendStreamingMessage(ctx, req) {
		if err != nil {
			// A stream error after a terminal state is the transport
			// winding down; before one, it is the dispatch's failure.
			if outcome.Status == "" {
				return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch stream: %w", err)
			}
			return outcome, nil
		}
		switch e := ev.(type) {
		case *a2aspec.TaskStatusUpdateEvent:
			applyStatusUpdate(&outcome, e)
		case *a2aspec.TaskArtifactUpdateEvent:
			applyArtifactUpdate(&outcome, e)
		case *a2aspec.Task:
			// The task snapshot may carry the terminal state directly
			// (a consumer that missed the status event); fold it in.
			if isTerminalTaskState(e.Status.State) && outcome.Status == "" {
				applyTaskSnapshot(&outcome, e)
			}
		}
	}
	if outcome.Status == "" {
		return agent.DispatchTransportOutcome{}, fmt.Errorf("a2a: dispatch stream ended without a terminal state")
	}
	return outcome, nil
}

// applyStatusUpdate folds one status update into the outcome: Working
// counts as progress; terminal states set the outcome's status and text.
func applyStatusUpdate(outcome *agent.DispatchTransportOutcome, ev *a2aspec.TaskStatusUpdateEvent) {
	switch ev.Status.State {
	case a2aspec.TaskStateWorking:
		outcome.WorkingEvents++
	case a2aspec.TaskStateCompleted:
		outcome.Status = DispatchStatusCompleted
		outcome.Text = statusUpdateMessageText(ev)
	case a2aspec.TaskStateFailed, a2aspec.TaskStateRejected:
		outcome.Status = DispatchStatusFailed
		outcome.Text = statusUpdateMessageText(ev)
	case a2aspec.TaskStateCanceled:
		outcome.Status = DispatchStatusCanceled
		outcome.Text = statusUpdateMessageText(ev)
	}
}

// applyTaskSnapshot folds a terminal task snapshot into the outcome.
func applyTaskSnapshot(outcome *agent.DispatchTransportOutcome, task *a2aspec.Task) {
	switch task.Status.State {
	case a2aspec.TaskStateCompleted:
		outcome.Status = DispatchStatusCompleted
	case a2aspec.TaskStateFailed, a2aspec.TaskStateRejected:
		outcome.Status = DispatchStatusFailed
	case a2aspec.TaskStateCanceled:
		outcome.Status = DispatchStatusCanceled
	}
	if text := statusUpdateMessageText(&a2aspec.TaskStatusUpdateEvent{Status: task.Status}); text != "" {
		outcome.Text = text
	}
}

// applyArtifactUpdate folds one artifact update into the outcome (#361):
// the named diff artifact reassembles by artifact ID — capped at
// maxReassembledDiffBytes, anything past the cap marks the outcome
// truncated — and the dispatch-result artifact decodes into the outcome's
// typed fields. Unrecognized artifacts are ignored.
func applyArtifactUpdate(outcome *agent.DispatchTransportOutcome, ev *a2aspec.TaskArtifactUpdateEvent) {
	if ev == nil || ev.Artifact == nil {
		return
	}
	switch ev.Artifact.ID {
	case DiffArtifactID:
		for _, part := range ev.Artifact.Parts {
			if part == nil {
				continue
			}
			text := part.Text()
			if len(outcome.Diff)+len(text) > maxReassembledDiffBytes {
				outcome.DiffTruncated = true
				break
			}
			outcome.Diff += text
		}
	case ResultArtifactID:
		for _, part := range ev.Artifact.Parts {
			if decoded, ok := decodeDispatchOutcome(part); ok {
				outcome.DiffError = decoded.DiffError
			}
		}
	}
}

// isTerminalTaskState reports whether the state ends the task.
func isTerminalTaskState(state a2aspec.TaskState) bool {
	switch state {
	case a2aspec.TaskStateCompleted, a2aspec.TaskStateFailed, a2aspec.TaskStateCanceled, a2aspec.TaskStateRejected:
		return true
	default:
		return false
	}
}

// statusMessageText extracts the text of a status update's message.
func statusUpdateMessageText(ev *a2aspec.TaskStatusUpdateEvent) string {
	if ev == nil || ev.Status.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range ev.Status.Message.Parts {
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
	return b.String()
}

// dispatchHTTPClient builds the HTTP client dispatch streams dial
// through: its transport maps every dial onto the factory's unix socket,
// ignoring the resolved address, so the card's http://crush-a2a endpoint
// is a routing label rather than a dial target (#346). There is
// deliberately no proxy (ProxyFromEnvironment would route an http:// URL
// at an HTTP proxy, breaking the unix dial) and no overall timeout: the
// SDK's own 3-minute total timeout is dropped, matching the per-phase
// bounds the dispatch client carries.
func (f *ServerFactory) dispatchHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: 10 * time.Second}
				return dialer.DialContext(ctx, "unix", f.socketPath())
			},
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		},
	}
}

// Compile-time proof the factory also satisfies the transport seam.
var _ agent.DispatchTransport = (*ServerFactory)(nil)
