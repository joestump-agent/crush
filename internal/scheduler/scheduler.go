// Package scheduler implements session-scoped scheduled tasks ("cron
// jobs") for the CronCreate, CronList, and CronDelete agent tools. The
// model and semantics deliberately mirror Claude Code's cron tools and
// the Codex scheduled-tasks proposal: strict 5-field local-time cron
// expressions, one-shot versus recurring tasks, 8-character IDs, at most
// 50 tasks per session, in-memory session tasks, and opt-in durable
// tasks persisted to disk.
package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxTasksPerSession caps the number of scheduled tasks a single session
// may hold, matching Claude Code's limit.
const MaxTasksPerSession = 50

// maxLookahead bounds the search for the next fire time, mirroring the
// 5-year cap in the Codex scheduled-tasks prototype.
const maxLookahead = 5 * 366 * 24 * time.Hour

// Task is a single scheduled prompt.
type Task struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"sessionId"`
	Prompt     string     `json:"prompt"`
	Cron       string     `json:"cron"`
	Recurring  bool       `json:"recurring"`
	Durable    bool       `json:"durable"`
	CreatedAt  time.Time  `json:"createdAt"`
	NextRunAt  time.Time  `json:"nextRunAt"`
	LastRunAt  *time.Time `json:"lastRunAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
	RunCount   int        `json:"runCount"`
	workingDir string
}

// NewTaskID returns a random 8-character hex ID, matching the ID shape
// used by both Claude Code and the Codex scheduled-tasks prototype.
func NewTaskID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("failed to generate task ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrTaskNotFound is returned when a delete targets an unknown task ID.
var ErrTaskNotFound = errors.New("no scheduled task with that ID")

// ErrTooManyTasks is returned when a session already holds the maximum
// number of scheduled tasks.
var ErrTooManyTasks = errors.New("session already has the maximum of 50 scheduled tasks")

// Store keeps scheduled tasks for all sessions. Session tasks live only
// in memory; durable tasks are additionally persisted to a JSON file so
// they survive restarts.
type Store struct {
	mu       sync.RWMutex
	tasks    map[string]*Task // keyed by ID
	filePath string           // durable persistence path; "" disables durable tasks
	now      func() time.Time // for tests
}

// NewStore returns a Store persisting durable tasks at filePath. An
// empty filePath keeps every task in memory only.
func NewStore(filePath string) *Store {
	return &Store{
		tasks:    make(map[string]*Task),
		filePath: filePath,
		now:      time.Now,
	}
}

// Load reads durable tasks from the store's persistence file. Missing
// files are not an error. Tasks whose next run is far in the past are
// rescheduled from now rather than firing a backlog of missed runs —
// there is no catch-up for missed fires.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read scheduled tasks: %w", err)
	}

	var durable []Task
	if err := json.Unmarshal(data, &durable); err != nil {
		return fmt.Errorf("failed to parse scheduled tasks: %w", err)
	}

	for i := range durable {
		t := durable[i]
		if !t.Durable {
			continue
		}
		sched, err := Parse(t.Cron)
		if err != nil {
			continue
		}
		if t.NextRunAt.Before(s.now().Add(-time.Minute)) {
			t.NextRunAt = sched.Next(s.now())
		}
		task := t
		s.tasks[task.ID] = &task
	}
	return nil
}

// Create validates and registers a new task for sessionID.
func (s *Store) Create(sessionID, cronExpr, prompt string, recurring, durable bool) (Task, error) {
	if prompt == "" {
		return Task{}, errors.New("prompt is required")
	}
	sched, err := Parse(cronExpr)
	if err != nil {
		return Task{}, err
	}
	if durable && s.filePath == "" {
		return Task{}, errors.New("durable tasks are not available: no persistence path configured")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, t := range s.tasks {
		if t.SessionID == sessionID {
			count++
		}
	}
	if count >= MaxTasksPerSession {
		return Task{}, ErrTooManyTasks
	}

	id, err := NewTaskID()
	if err != nil {
		return Task{}, err
	}
	now := s.now()
	task := &Task{
		ID:        id,
		SessionID: sessionID,
		Prompt:    prompt,
		Cron:      cronExpr,
		Recurring: recurring,
		Durable:   durable,
		CreatedAt: now.UTC(),
		NextRunAt: sched.Next(now),
	}
	s.tasks[id] = task
	if err := s.persistLocked(); err != nil {
		delete(s.tasks, id)
		return Task{}, err
	}
	return *task, nil
}

// List returns the tasks belonging to sessionID, ordered by next run.
func (s *Store) List(sessionID string) []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Task
	for _, t := range s.tasks {
		if t.SessionID == sessionID {
			out = append(out, *t)
		}
	}
	sortTasks(out)
	return out
}

// ListAll returns every task in the store, ordered by next run.
func (s *Store) ListAll() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, *t)
	}
	sortTasks(out)
	return out
}

// Delete removes the task with the given ID. The task must belong to
// sessionID: a session cannot delete another session's tasks.
func (s *Store) Delete(sessionID, id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok || t.SessionID != sessionID {
		return Task{}, ErrTaskNotFound
	}
	deleted := *t
	delete(s.tasks, id)
	if err := s.persistLocked(); err != nil {
		return Task{}, err
	}
	return deleted, nil
}

// DueTasks returns every task whose next run time has passed as of now.
func (s *Store) DueTasks() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := s.now()
	var out []Task
	for _, t := range s.tasks {
		if !t.NextRunAt.After(now) {
			out = append(out, *t)
		}
	}
	sortTasks(out)
	return out
}

// MarkFired records a successful firing: recurring tasks reschedule
// themselves, one-shots delete themselves.
func (s *Store) MarkFired(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return
	}
	now := s.now()
	t.LastRunAt = &now
	t.RunCount++
	if !t.Recurring {
		delete(s.tasks, id)
		_ = s.persistLocked()
		return
	}
	sched, err := Parse(t.Cron)
	if err != nil {
		delete(s.tasks, id)
		_ = s.persistLocked()
		return
	}
	t.NextRunAt = sched.Next(now)
	_ = s.persistLocked()
}

// MarkError records a firing failure and reschedules recurring tasks so
// a transient error does not kill the job.
func (s *Store) MarkError(id string, fireErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return
	}
	now := s.now()
	t.LastError = fireErr.Error()
	if t.Recurring {
		if sched, err := Parse(t.Cron); err == nil {
			t.NextRunAt = sched.Next(now)
		}
	} else {
		delete(s.tasks, id)
	}
	_ = s.persistLocked()
}

// DropSession removes all in-memory (non-durable) tasks belonging to a
// session. Durable tasks are kept so they can resume when the session is
// reloaded.
func (s *Store) DropSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, t := range s.tasks {
		if t.SessionID == sessionID && !t.Durable {
			delete(s.tasks, id)
		}
	}
}

// persistLocked writes durable tasks to disk. Callers must hold s.mu.
func (s *Store) persistLocked() error {
	if s.filePath == "" {
		return nil
	}

	var durable []Task
	for _, t := range s.tasks {
		if t.Durable {
			durable = append(durable, *t)
		}
	}
	sortTasks(durable)

	data, err := json.MarshalIndent(durable, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode scheduled tasks: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.filePath), 0o755); err != nil {
		return fmt.Errorf("failed to create scheduled tasks directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.filePath), ".scheduled_tasks-*.json")
	if err != nil {
		return fmt.Errorf("failed to create scheduled tasks temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("failed to write scheduled tasks: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("failed to write scheduled tasks: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("failed to secure scheduled tasks file: %w", err)
	}
	if err := os.Rename(tmpName, s.filePath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("failed to replace scheduled tasks file: %w", err)
	}
	return nil
}

func sortTasks(tasks []Task) {
	for i := 1; i < len(tasks); i++ {
		for j := i; j > 0 && tasks[j].NextRunAt.Before(tasks[j-1].NextRunAt); j-- {
			tasks[j], tasks[j-1] = tasks[j-1], tasks[j]
		}
	}
}
