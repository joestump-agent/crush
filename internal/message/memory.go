package message

import (
	"context"
	"database/sql"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

// memoryService is an in-memory implementation of [Service] for
// ephemeral sessions (e.g. Sidekick). Messages never touch SQLite and
// are destroyed when the process exits. Every update flushes
// synchronously — there is no debounce buffer — so Flush and FlushAll
// are no-ops and reads always observe the latest [Service.Update].
// Events publish only to this service's own private broker, never to
// the app-wide message broker.
type memoryService struct {
	*pubsub.Broker[Message]

	mu       sync.RWMutex
	messages map[string]Message
	// order preserves per-session insertion order so List returns
	// messages in creation order even when timestamps collide.
	order map[string][]string
}

// NewInMemoryService returns a [Service] backed entirely by process
// memory. It mirrors the DB-backed service's contract (including
// returning [sql.ErrNoRows] for missing messages) but persists nothing.
func NewInMemoryService() Service {
	return &memoryService{
		Broker:   pubsub.NewBroker[Message](),
		messages: make(map[string]Message),
		order:    make(map[string][]string),
	}
}

func (s *memoryService) Create(ctx context.Context, sessionID string, params CreateMessageParams) (Message, error) {
	if params.Role != Assistant {
		params.Parts = append(params.Parts, Finish{
			Reason: "stop",
		})
	}
	now := time.Now().UnixMilli()
	msg := Message{
		ID:               uuid.New().String(),
		SessionID:        sessionID,
		Role:             params.Role,
		Parts:            params.Parts,
		Model:            params.Model,
		Provider:         params.Provider,
		CreatedAt:        now,
		UpdatedAt:        now,
		IsSummaryMessage: params.IsSummaryMessage,
	}
	stored := msg.Clone()
	s.mu.Lock()
	s.messages[stored.ID] = stored
	s.order[sessionID] = append(s.order[sessionID], stored.ID)
	s.mu.Unlock()
	s.Publish(pubsub.CreatedEvent, msg.Clone())
	return msg, nil
}

func (s *memoryService) Update(ctx context.Context, msg Message) error {
	cloned := msg.Clone()
	cloned.UpdatedAt = time.Now().UnixMilli()
	s.mu.Lock()
	prev, ok := s.messages[msg.ID]
	if !ok {
		s.mu.Unlock()
		return sql.ErrNoRows
	}
	s.messages[msg.ID] = cloned
	s.mu.Unlock()
	// Terminal events — message finished, tool call added or finished,
	// reasoning ended — use the bounded must-deliver path so they never
	// get dropped under channel contention, matching the DB-backed
	// service.
	if shouldFlushNow(&prev, &cloned) {
		s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, cloned)
	} else {
		s.Publish(pubsub.UpdatedEvent, cloned)
	}
	return nil
}

func (s *memoryService) Get(ctx context.Context, id string) (Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msg, ok := s.messages[id]
	if !ok {
		return Message{}, sql.ErrNoRows
	}
	return msg.Clone(), nil
}

func (s *memoryService) List(ctx context.Context, sessionID string) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.order[sessionID]
	messages := make([]Message, 0, len(ids))
	for _, id := range ids {
		if msg, ok := s.messages[id]; ok {
			messages = append(messages, msg.Clone())
		}
	}
	return messages, nil
}

func (s *memoryService) ListUserMessages(ctx context.Context, sessionID string) ([]Message, error) {
	msgs, err := s.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(msgs, func(m Message) bool {
		return m.Role != User
	}), nil
}

func (s *memoryService) ListAllUserMessages(ctx context.Context) ([]Message, error) {
	s.mu.RLock()
	sessionIDs := make([]string, 0, len(s.order))
	for sessionID := range s.order {
		sessionIDs = append(sessionIDs, sessionID)
	}
	s.mu.RUnlock()
	slices.Sort(sessionIDs)
	var all []Message
	for _, sessionID := range sessionIDs {
		msgs, err := s.ListUserMessages(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		all = append(all, msgs...)
	}
	return all, nil
}

func (s *memoryService) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	msg, ok := s.messages[id]
	if !ok {
		s.mu.Unlock()
		return sql.ErrNoRows
	}
	delete(s.messages, id)
	s.order[msg.SessionID] = slices.DeleteFunc(s.order[msg.SessionID], func(o string) bool { return o == id })
	s.mu.Unlock()
	s.Publish(pubsub.DeletedEvent, msg.Clone())
	return nil
}

func (s *memoryService) DeleteSessionMessages(ctx context.Context, sessionID string) error {
	msgs, err := s.List(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, msg := range msgs {
		if err := s.Delete(ctx, msg.ID); err != nil {
			return err
		}
	}
	return nil
}

// Flush implements [Service.Flush]. Updates are synchronous in the
// in-memory store, so there is never pending state to drain.
func (s *memoryService) Flush(ctx context.Context, id string) error {
	return nil
}

// FlushAll implements [Service.FlushAll]. See [memoryService.Flush].
func (s *memoryService) FlushAll(ctx context.Context) error {
	return nil
}
