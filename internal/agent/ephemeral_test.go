package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func newEphemeralTestAgent(text string) *EphemeralAgent {
	model := &finishStreamModel{text: text}
	fakeModel := Model{
		Model: model,
		CatwalkCfg: catwalk.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
	return NewEphemeralAgent(SessionAgentOptions{
		LargeModel:   fakeModel,
		SmallModel:   fakeModel,
		SystemPrompt: "system",
		IsYolo:       true,
	})
}

// TestEphemeralAgentRun drives a full turn through an EphemeralAgent
// and verifies the ephemeral contract: messages land only in the
// private in-memory store and the session title is never generated.
func TestEphemeralAgentRun(t *testing.T) {
	t.Parallel()
	ea := newEphemeralTestAgent("done")

	sess, err := ea.Sessions.Create(t.Context(), "Sidekick")
	require.NoError(t, err)

	result, err := ea.Run(t.Context(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "hello",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	msgs, err := ea.Messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "expected user + assistant messages")
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, message.Assistant, msgs[1].Role)
	require.Equal(t, "done", msgs[1].Content().Text)

	// Title generation is skipped for ephemeral sessions: the session
	// keeps the title it was created with.
	after, err := ea.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "Sidekick", after.Title)

	require.Equal(t, 0, ea.QueuedPrompts(sess.ID))
	require.False(t, ea.IsSessionBusy(sess.ID))
}

// TestEphemeralAgentBusyRejectsInsteadOfQueueing verifies that a busy
// ephemeral session rejects a second call with ErrSessionBusy rather
// than queueing it: Sidekick runs are fire-and-forget, never queued.
func TestEphemeralAgentBusyRejectsInsteadOfQueueing(t *testing.T) {
	t.Parallel()
	ea := newEphemeralTestAgent("done")
	sa := ea.SessionAgent.(*sessionAgent)

	sess, err := ea.Sessions.Create(t.Context(), "Sidekick")
	require.NoError(t, err)

	// Make the session look busy: an earlier prompt is active.
	sa.activeRequests.Set(sess.ID, func() {})
	defer sa.activeRequests.Del(sess.ID)

	result, err := ea.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "follow-up",
	})
	require.ErrorIs(t, err, ErrSessionBusy)
	require.Nil(t, result)
	require.Equal(t, 0, ea.QueuedPrompts(sess.ID),
		"busy ephemeral session must not enqueue the call")
}

// TestEphemeralAgentBusyClosesAcceptReservation verifies the rejected
// busy-path call still releases its accept reservation so acceptedRuns
// does not leak.
func TestEphemeralAgentBusyClosesAcceptReservation(t *testing.T) {
	t.Parallel()
	ea := newEphemeralTestAgent("done")
	sa := ea.SessionAgent.(*sessionAgent)

	sess, err := ea.Sessions.Create(t.Context(), "Sidekick")
	require.NoError(t, err)

	sa.activeRequests.Set(sess.ID, func() {})
	defer sa.activeRequests.Del(sess.ID)

	accept := sa.BeginAccepted(sess.ID)
	_, err = ea.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "follow-up",
		Accepted:  accept,
	})
	require.ErrorIs(t, err, ErrSessionBusy)

	count, _ := sa.acceptedRuns.Get(sess.ID)
	require.Zero(t, count, "accept reservation must be released on busy rejection")
}

// TestEphemeralAgentIndependentBusyTracking verifies that ephemeral
// agents track busy state independently of any other agent instance.
func TestEphemeralAgentIndependentBusyTracking(t *testing.T) {
	t.Parallel()
	ea := newEphemeralTestAgent("done")
	other := newEphemeralTestAgent("done")

	const sid = "shared-session-id"
	other.SessionAgent.(*sessionAgent).activeRequests.Set(sid, func() {})

	require.True(t, other.IsSessionBusy(sid))
	require.False(t, ea.IsSessionBusy(sid),
		"busy state must not leak across agent instances")
	require.False(t, ea.IsBusy())
}

// TestEphemeralAgentGenerateTitleIsNoOp verifies GenerateTitle never
// renames an ephemeral session, even when invoked directly.
func TestEphemeralAgentGenerateTitleIsNoOp(t *testing.T) {
	t.Parallel()
	ea := newEphemeralTestAgent("A Generated Title")

	sess, err := ea.Sessions.Create(t.Context(), "Sidekick")
	require.NoError(t, err)

	ea.GenerateTitle(t.Context(), sess.ID, "some prompt")

	after, err := ea.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "Sidekick", after.Title)
}

// TestEphemeralAgentForcesEphemeralOptions verifies the constructor
// enforces the ephemeral contract regardless of caller-supplied
// options.
func TestEphemeralAgentForcesEphemeralOptions(t *testing.T) {
	t.Parallel()
	ea := newEphemeralTestAgent("done")
	sa := ea.SessionAgent.(*sessionAgent)

	require.True(t, sa.ephemeral)
	require.True(t, sa.disableAutoSummarize, "ephemeral agents never auto-summarize")
	require.Nil(t, sa.notify, "ephemeral agents never publish notifications")
}
