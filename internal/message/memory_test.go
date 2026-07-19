package message

import (
	"context"
	"database/sql"
	"testing"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestInMemoryMessageServiceCRUD(t *testing.T) {
	t.Parallel()
	messages := NewInMemoryService()
	const sessionID = "ephemeral-session"

	user, err := messages.Create(t.Context(), sessionID, CreateMessageParams{
		Role:  User,
		Parts: []ContentPart{TextContent{Text: "hello"}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, user.ID)
	// Non-assistant messages get a synthetic finish part, matching the
	// DB-backed service.
	require.True(t, user.IsFinished())

	assistant, err := messages.Create(t.Context(), sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{},
	})
	require.NoError(t, err)
	require.False(t, assistant.IsFinished())

	assistant.AppendContent("hi there")
	require.NoError(t, messages.Update(t.Context(), assistant))

	fetched, err := messages.Get(t.Context(), assistant.ID)
	require.NoError(t, err)
	require.Equal(t, "hi there", fetched.Content().Text)

	listed, err := messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, user.ID, listed[0].ID)
	require.Equal(t, assistant.ID, listed[1].ID)

	userMsgs, err := messages.ListUserMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, userMsgs, 1)
	require.Equal(t, user.ID, userMsgs[0].ID)

	allUserMsgs, err := messages.ListAllUserMessages(t.Context())
	require.NoError(t, err)
	require.Len(t, allUserMsgs, 1)

	require.NoError(t, messages.Delete(t.Context(), user.ID))
	listed, err = messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	require.NoError(t, messages.DeleteSessionMessages(t.Context(), sessionID))
	listed, err = messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	require.Empty(t, listed)
}

func TestInMemoryMessageServiceMissingMessage(t *testing.T) {
	t.Parallel()
	messages := NewInMemoryService()

	_, err := messages.Get(t.Context(), "nope")
	require.ErrorIs(t, err, sql.ErrNoRows)

	require.ErrorIs(t, messages.Update(t.Context(), Message{ID: "nope"}), sql.ErrNoRows)
	require.ErrorIs(t, messages.Delete(t.Context(), "nope"), sql.ErrNoRows)
}

func TestInMemoryMessageServiceFlushIsNoOp(t *testing.T) {
	t.Parallel()
	messages := NewInMemoryService()

	require.NoError(t, messages.Flush(t.Context(), "anything"))
	require.NoError(t, messages.FlushAll(t.Context()))
}

func TestInMemoryMessageServicePublishesToOwnBroker(t *testing.T) {
	t.Parallel()
	messages := NewInMemoryService()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := messages.Subscribe(ctx)

	created, err := messages.Create(t.Context(), "session", CreateMessageParams{
		Role:  User,
		Parts: []ContentPart{TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	ev := <-events
	require.Equal(t, pubsub.CreatedEvent, ev.Type)
	require.Equal(t, created.ID, ev.Payload.ID)

	created.AppendContent(" world")
	require.NoError(t, messages.Update(t.Context(), created))

	ev = <-events
	require.Equal(t, pubsub.UpdatedEvent, ev.Type)
	require.Equal(t, created.ID, ev.Payload.ID)
}

// TestInMemoryMessageServiceUpdateIsSynchronous verifies reads observe
// the latest Update without an explicit Flush — the in-memory store has
// no debounce buffer.
func TestInMemoryMessageServiceUpdateIsSynchronous(t *testing.T) {
	t.Parallel()
	messages := NewInMemoryService()

	msg, err := messages.Create(t.Context(), "session", CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{},
	})
	require.NoError(t, err)

	msg.AppendContent("streamed")
	require.NoError(t, messages.Update(t.Context(), msg))

	fetched, err := messages.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "streamed", fetched.Content().Text)
}
