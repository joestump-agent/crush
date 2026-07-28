package session

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
// ephemeral sessions (e.g. Sidekick). Sessions never touch SQLite, so
// they are invisible to the DB-backed session list and are destroyed
// when the process exits. Events publish only to this service's own
// private broker, never to the app-wide session broker.
type memoryService struct {
	*pubsub.Broker[Session]

	mu       sync.RWMutex
	sessions map[string]Session
	// order preserves insertion order so List/GetLast tie-breaks are
	// deterministic when UpdatedAt timestamps collide.
	order []string
}

// NewInMemoryService returns a [Service] backed entirely by process
// memory. It mirrors the DB-backed service's contract (including
// returning [sql.ErrNoRows] for missing sessions) but persists nothing
// and emits no telemetry events.
func NewInMemoryService() Service {
	return &memoryService{
		Broker:   pubsub.NewBroker[Session](),
		sessions: make(map[string]Session),
	}
}

func (s *memoryService) create(id, parentSessionID, title string) Session {
	now := time.Now().UnixMilli()
	session := Session{
		ID:              id,
		ParentSessionID: parentSessionID,
		Title:           title,
		Todos:           []Todo{},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.mu.Lock()
	if _, exists := s.sessions[id]; !exists {
		s.order = append(s.order, id)
	}
	s.sessions[id] = session
	s.mu.Unlock()
	s.Publish(pubsub.CreatedEvent, session)
	return session
}

func (s *memoryService) Create(ctx context.Context, title string) (Session, error) {
	return s.create(uuid.New().String(), "", title), nil
}

func (s *memoryService) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	return s.create("title-"+parentSessionID, parentSessionID, "Generate a title"), nil
}

func (s *memoryService) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	return s.create(toolCallID, parentSessionID, title), nil
}

func (s *memoryService) Get(ctx context.Context, id string) (Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[id]
	if !ok {
		return Session{}, sql.ErrNoRows
	}
	return session, nil
}

func (s *memoryService) GetLast(ctx context.Context) (Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var last Session
	found := false
	for _, id := range s.order {
		session := s.sessions[id]
		if !found || session.UpdatedAt >= last.UpdatedAt {
			last = session
			found = true
		}
	}
	if !found {
		return Session{}, sql.ErrNoRows
	}
	return last, nil
}

func (s *memoryService) List(ctx context.Context) ([]Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sessions := make([]Session, 0, len(s.sessions))
	for _, id := range s.order {
		session := s.sessions[id]
		if session.ParentSessionID != "" {
			continue
		}
		sessions = append(sessions, session)
	}
	// Newest-first, matching the DB-backed ListSessions ordering.
	slices.SortStableFunc(sessions, func(a, b Session) int {
		switch {
		case a.UpdatedAt > b.UpdatedAt:
			return -1
		case a.UpdatedAt < b.UpdatedAt:
			return 1
		default:
			return 0
		}
	})
	return sessions, nil
}

// save stores the mutated session, bumps UpdatedAt, and publishes an
// UpdatedEvent on the private broker. mutate runs with the lock held.
func (s *memoryService) save(id string, mutate func(*Session)) (Session, error) {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return Session{}, sql.ErrNoRows
	}
	mutate(&session)
	session.UpdatedAt = time.Now().UnixMilli()
	s.sessions[id] = session
	s.mu.Unlock()
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

func (s *memoryService) Save(ctx context.Context, session Session) (Session, error) {
	return s.save(session.ID, func(stored *Session) {
		stored.Title = session.Title
		stored.PromptTokens = session.PromptTokens
		stored.CompletionTokens = session.CompletionTokens
		stored.EstimatedUsage = session.EstimatedUsage
		stored.SummaryMessageID = session.SummaryMessageID
		stored.Cost = session.Cost
		stored.Todos = session.Todos
		stored.Channel = session.Channel
	})
}

func (s *memoryService) SetChannel(ctx context.Context, sessionID, channel string) (Session, error) {
	return s.save(sessionID, func(stored *Session) {
		stored.Channel = channel
	})
}

func (s *memoryService) UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error {
	_, err := s.save(sessionID, func(stored *Session) {
		stored.Title = title
		stored.PromptTokens = promptTokens
		stored.CompletionTokens = completionTokens
		stored.Cost = cost
	})
	return err
}

func (s *memoryService) Rename(ctx context.Context, id string, title string) error {
	// The DB-backed Rename does not touch updated_at; mirror that here
	// by skipping save's timestamp bump.
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return sql.ErrNoRows
	}
	session.Title = title
	s.sessions[id] = session
	s.mu.Unlock()
	s.Publish(pubsub.UpdatedEvent, session)
	return nil
}

func (s *memoryService) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return sql.ErrNoRows
	}
	delete(s.sessions, id)
	s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })
	s.mu.Unlock()
	s.Publish(pubsub.DeletedEvent, session)
	return nil
}

func (s *memoryService) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return agentToolSessionID(messageID, toolCallID)
}

func (s *memoryService) ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool) {
	return parseAgentToolSessionID(sessionID)
}

func (s *memoryService) IsAgentToolSession(sessionID string) bool {
	_, _, ok := parseAgentToolSessionID(sessionID)
	return ok
}
