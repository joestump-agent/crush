package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// External agents (#434): a runtime a2a agent definition replaces the
// in-process dispatched agent with an A2A Agent Card hosted elsewhere.
// The coordinator resolves the definition's bearer token, hands it to
// the A2A host with the card URL, and drives the returned ExternalAgent
// through the same client path as a served dispatch. The host owns the
// card checks, the origin pin, and the credential; the registry never
// sees the token.

// redactedSecret is what a Secret renders as, wherever it is printed.
const redactedSecret = "[REDACTED]"

// Secret is a credential value that never prints itself (#434): fmt,
// slog and encoding/json all render it as [REDACTED], so a stray log
// line or a %v of a struct carrying it cannot leak the value. Reveal
// returns it for the one place that must send it.
type Secret struct {
	value string
}

// NewSecret wraps a resolved credential.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the credential itself. Only the code that puts it on
// the wire, or scrubs it from text, calls it.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret holds no credential.
func (s Secret) IsZero() bool { return s.value == "" }

// String implements fmt.Stringer with the redacted form.
func (Secret) String() string { return redactedSecret }

// GoString implements fmt.GoStringer with the redacted form.
func (Secret) GoString() string { return redactedSecret }

// LogValue implements slog.LogValuer with the redacted form.
func (Secret) LogValue() slog.Value { return slog.StringValue(redactedSecret) }

// MarshalJSON renders the redacted form, never the credential.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redactedSecret) }

// ExternalAgentParams names one external agent to resolve (#434).
type ExternalAgentParams struct {
	// CardURL is the definition's Agent Card URL.
	CardURL string
	// Token is the resolved bearer credential; zero when the definition
	// sets no auth. A non-zero token requires the card to declare an
	// HTTP bearer security scheme, and rides only requests to the card's
	// own origin.
	Token Secret
}

// ExternalAgentResolver is the external-card half of the A2A host
// (#434): resolve a runtime a2a definition's card at dispatch time and
// return the pinned client that drives it. Implemented by the
// production a2a.ServerFactory; coordinators assert to it the way they
// do to [DispatchCanceler], and refuse external dispatches when the
// wired host does not implement it.
type ExternalAgentResolver interface {
	// ResolveExternalAgent fetches the card, checks it — https, a
	// supported transport on the card's own origin, a bearer scheme when
	// a token is set — and returns the agent. Every refusal is an error
	// that names the card and never the token.
	ResolveExternalAgent(ctx context.Context, params ExternalAgentParams) (ExternalAgent, error)
}

// ExternalAgent is one resolved external Agent Card and the client that
// talks to it (#434). The client is pinned to the card's origin and
// carries the credential the resolver was given; nothing outside the
// implementation can read the credential back.
type ExternalAgent interface {
	// Source is the card URL the agent was resolved from: the
	// DispatchResult's source.
	Source() string
	// Endpoint is the JSON-RPC service URL the card named, on the
	// card's own origin.
	Endpoint() string
	// Stream sends the prompt as a new task and consumes it to its end:
	// a terminal state, a refused pause, a kill, or a silent stream.
	// The outcome's text is the remote agent's untrusted output,
	// sanitized and capped; it never carries diffs, usage, or todos.
	Stream(ctx context.Context, params ExternalDispatchParams) (DispatchTransportOutcome, error)
	// Close releases the client's idle connections once the dispatch
	// has ended.
	Close()
}

// ExternalDispatchParams is one external dispatch's slice of the A2A
// client (#434).
type ExternalDispatchParams struct {
	// Prompt is the task text sent to the external agent.
	Prompt string
	// OnTask, when set, receives the remote task's ID once the stream's
	// first event names it, exactly like a served dispatch (#349).
	OnTask func(taskID string)
	// IdleTimeout ends a stream that delivers no event for this long:
	// the run is killed with dispatch.ReasonIdleTimeout and its task is
	// canceled. 0 disables it.
	IdleTimeout time.Duration
	// Kill is the run's kill switch. The transport watches it: a kill
	// ends the stream and cancels the remote task with the kill's
	// reason, and the idle timeout trips it. Nil means the run cannot
	// be killed from outside.
	Kill RunKill
}

// RunKill is a dispatched run's kill switch as a transport sees it
// (#434): the first reason wins, and Killed closes with it.
type RunKill interface {
	// Kill records reason as the run's kill reason unless one is
	// already recorded.
	Kill(reason string)
	// Killed returns a channel closed by the first kill.
	Killed() <-chan struct{}
	// Reason returns the recorded kill reason, empty until a kill.
	Reason() string
}
