package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestInMemoryServiceCRUD(t *testing.T) {
	t.Parallel()
	sessions := NewInMemoryService()

	created, err := sessions.Create(t.Context(), "Ephemeral Session")
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "Ephemeral Session", created.Title)
	require.NotZero(t, created.CreatedAt)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, fetched.ID)

	fetched.PromptTokens = 100
	fetched.CompletionTokens = 50
	fetched.Cost = 0.25
	fetched.Todos = []Todo{{Content: "task", Status: TodoStatusPending, ActiveForm: "tasking"}}
	saved, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.Equal(t, int64(100), saved.PromptTokens)
	require.Equal(t, int64(50), saved.CompletionTokens)
	require.InDelta(t, 0.25, saved.Cost, 1e-9)
	require.Len(t, saved.Todos, 1)

	require.NoError(t, sessions.Rename(t.Context(), created.ID, "Renamed"))
	renamed, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "Renamed", renamed.Title)

	require.NoError(t, sessions.Delete(t.Context(), created.ID))
	_, err = sessions.Get(t.Context(), created.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestInMemoryServiceMissingSession(t *testing.T) {
	t.Parallel()
	sessions := NewInMemoryService()

	_, err := sessions.Get(t.Context(), "nope")
	require.ErrorIs(t, err, sql.ErrNoRows)

	_, err = sessions.GetLast(t.Context())
	require.ErrorIs(t, err, sql.ErrNoRows)

	_, err = sessions.Save(t.Context(), Session{ID: "nope"})
	require.ErrorIs(t, err, sql.ErrNoRows)

	require.ErrorIs(t, sessions.Rename(t.Context(), "nope", "title"), sql.ErrNoRows)
	require.ErrorIs(t, sessions.Delete(t.Context(), "nope"), sql.ErrNoRows)
}

func TestInMemoryServiceListExcludesChildren(t *testing.T) {
	t.Parallel()
	sessions := NewInMemoryService()

	parent, err := sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	_, err = sessions.CreateTaskSession(t.Context(), "tool-call-1", parent.ID, "child")
	require.NoError(t, err)
	_, err = sessions.CreateTitleSession(t.Context(), parent.ID)
	require.NoError(t, err)

	listed, err := sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, parent.ID, listed[0].ID)

	last, err := sessions.GetLast(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, last.ID)
}

func TestInMemoryServiceUpdateTitleAndUsage(t *testing.T) {
	t.Parallel()
	sessions := NewInMemoryService()

	created, err := sessions.Create(t.Context(), "untitled")
	require.NoError(t, err)

	require.NoError(t, sessions.UpdateTitleAndUsage(t.Context(), created.ID, "titled", 10, 20, 0.5))
	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "titled", fetched.Title)
	require.Equal(t, int64(10), fetched.PromptTokens)
	require.Equal(t, int64(20), fetched.CompletionTokens)
	require.InDelta(t, 0.5, fetched.Cost, 1e-9)
}

func TestInMemoryServicePublishesToOwnBroker(t *testing.T) {
	t.Parallel()
	sessions := NewInMemoryService()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := sessions.Subscribe(ctx)

	created, err := sessions.Create(t.Context(), "watched")
	require.NoError(t, err)

	ev := <-events
	require.Equal(t, pubsub.CreatedEvent, ev.Type)
	require.Equal(t, created.ID, ev.Payload.ID)
}

func TestInMemoryServiceAgentToolSessionIDs(t *testing.T) {
	t.Parallel()
	sessions := NewInMemoryService()

	id := sessions.CreateAgentToolSessionID("msg-1", "tc-1")
	require.Equal(t, "msg-1$$tc-1", id)

	messageID, toolCallID, ok := sessions.ParseAgentToolSessionID(id)
	require.True(t, ok)
	require.Equal(t, "msg-1", messageID)
	require.Equal(t, "tc-1", toolCallID)

	require.True(t, sessions.IsAgentToolSession(id))
	require.False(t, sessions.IsAgentToolSession("plain-id"))
}
