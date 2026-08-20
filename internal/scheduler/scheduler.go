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
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// MaxTasksPerSession caps the number of scheduled tasks a single session
// may hold, matching Claude Code's limit.
const MaxTasksPerSession = 50

// Task is a single scheduled prompt.
type Task struct {
	ID        string     `json:"id"`
	SessionID string     `json:"sessionId"`
	Prompt    string     `json:"prompt"`
	Cron      string     `json:"cron"`
	Recurring bool       `json:"recurring"`
	Durable   bool       `json:"durable"`
	CreatedAt time.Time  `json:"createdAt"`
	NextRunAt time.Time  `json:"nextRunAt"`
	LastRunAt *time.Time `json:"lastRunAt,omitempty"`
	LastError string     `json:"lastError,omitempty"`
	RunCount  int        `json:"runCount"`
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

// ErrNeverFires is returned when a cron expression parses but can never
// match, such as "0 0 30 2 *" (February 30th).
var ErrNeverFires = errors.New("cron expression is valid but will never fire")

// ErrOneShotInPast is returned when a one-shot (non-recurring) task's
// schedule matched within the last day and does not match again within
// the next one. The intended fire time is then unrecoverable: the
// schedule's next match has jumped to tomorrow, next month, or next
// year, which is never what a one-shot means. The usual cause is an
// agent computing the cron fields against a stale clock (e.g. the
// session-start time in its prompt rather than the actual current
// time), so the error names both the match that slipped by and the
// next one.
var ErrOneShotInPast = errors.New("one-shot schedule's fire time has already passed")

// ErrOneShotDateInPast is the same rejection for a one-shot whose
// slipped match was on an EARLIER DAY: the cron fields name a calendar
// date that is already behind us, so the next match is a month or a
// year out. It is a distinct sentinel because the remedy differs — the
// caller must recompute the day-of-month/month fields, not just the
// minute — and because the two are easy to confuse from the message
// alone (both report a far-future next match).
//
// It wraps ErrOneShotInPast, so callers that only care that a one-shot
// was rejected as stale can keep matching the single sentinel.
var ErrOneShotDateInPast = fmt.Errorf("%w: its day-of-month/month name a date that is already behind us", ErrOneShotInPast)

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

	perSession := make(map[string]int)
	for i := range durable {
		t := durable[i]
		if !t.Durable || t.ID == "" {
			continue
		}
		sched, err := Parse(t.Cron)
		if err != nil {
			slog.Warn("Dropping scheduled task with unparseable cron expression", "id", t.ID, "cron", t.Cron, "error", err)
			continue
		}
		if t.NextRunAt.Before(s.now().Add(-time.Minute)) {
			t.NextRunAt = sched.Next(s.now()).Truncate(time.Minute)
		}
		// A zero next run is always "due", so a task carrying one would
		// fire on every tick forever. Drop it instead.
		if t.NextRunAt.IsZero() {
			slog.Warn("Dropping scheduled task that can never fire again", "id", t.ID, "cron", t.Cron)
			continue
		}
		if perSession[t.SessionID] >= MaxTasksPerSession {
			slog.Warn("Dropping scheduled task over the per-session limit", "id", t.ID, "session_id", t.SessionID)
			continue
		}
		perSession[t.SessionID]++
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

	now := s.now()
	nextRun := sched.Next(now)
	// Reject schedules that can never match ("0 0 30 2 *"). Storing a
	// zero next run would make the task perpetually due.
	if nextRun.IsZero() {
		return Task{}, fmt.Errorf("%w: %s", ErrNeverFires, cronExpr)
	}
	// Truncate to the start of the minute so a task created at HH:MM:50
	// for minute HH:MM fires at the top of the next minute, not 10
	// seconds later. The ticker polls every second, but cron expressions
	// have minute-level granularity — the stored next-run must reflect
	// that so users are not confused by sub-minute firing.
	nextRun = nextRun.Truncate(time.Minute)

	// A one-shot whose intended moment has already slipped by pinned a
	// fire time that is now unrecoverable; accepting it would store a
	// task firing next month or next year when the caller meant "in a
	// few minutes".
	if !recurring {
		if slipped, ok := oneShotSlipped(sched, now, nextRun); ok {
			kind := ErrOneShotInPast
			if !sameDay(slipped, now) {
				kind = ErrOneShotDateInPast
			}
			return Task{}, fmt.Errorf("%w: %q matched %s, which has passed, and does not match again until %s",
				kind, cronExpr, slipped.Format(time.RFC3339), nextRun.Format(time.RFC3339))
		}
	}

	id, err := NewTaskID()
	if err != nil {
		return Task{}, err
	}
	task := &Task{
		ID:        id,
		SessionID: sessionID,
		Prompt:    prompt,
		Cron:      cronExpr,
		Recurring: recurring,
		Durable:   durable,
		CreatedAt: now.UTC(),
		NextRunAt: nextRun,
	}
	s.tasks[id] = task
	if err := s.persistLocked(); err != nil {
		delete(s.tasks, id)
		return Task{}, err
	}
	return *task, nil
}

// oneShotWindow is how far back a slipped match still counts as the
// moment the caller meant, and how far ahead the next match may be
// before the schedule is treated as unrecoverable. A one-shot is a
// pinned instant, so both sides ask the same question: is there a
// matching moment in the last day, and is the next one further off
// than a day?
const oneShotWindow = 24 * time.Hour

// oneShotSlipped reports whether a one-shot's intended fire time has
// already gone by, returning the match that slipped.
//
// Both halves are required, and each rules out a different false
// positive:
//
//   - A match within the last day. Without it, "0 9 25 12 *" created in
//     August — a legitimate one-shot four months out — looks identical
//     to a stale one.
//   - A next match more than a day out. Without it, "*/5 * * * *"
//     created at 06:29 is rejected even though it fires at 06:30: a
//     match did just slip by, but the caller loses nothing.
//
// Together they say: the fields describe a moment that just passed, and
// the schedule will not come back around in time to be what the caller
// asked for. That is the signature of cron fields computed against a
// stale clock, and it holds across midnight — the previous form of this
// check looked back only to midnight, so a schedule pinned to
// yesterday's date sailed through and was stored to fire a year later
// (issue #272).
//
// Next returns the first match strictly after its argument, so a match
// landing exactly on the current minute counts as slipped: robfig
// treats the current minute as consumed, and the stored next run would
// jump a full period.
func oneShotSlipped(sched *Schedule, now, next time.Time) (time.Time, bool) {
	if next.IsZero() || next.Sub(now) <= oneShotWindow {
		return time.Time{}, false
	}
	minute := now.Truncate(time.Minute)
	slipped := sched.Next(now.Add(-oneShotWindow))
	if slipped.IsZero() || slipped.After(minute) {
		return time.Time{}, false
	}
	// Next walks forward, so land on the LAST match in the window rather
	// than the first: "0 22,23 19 8 *" matched twice yesterday, and the
	// message should name 23:00, the moment the caller most likely meant.
	// Bounded by construction — the loop only advances through matches
	// inside a 24h window that the schedule has already left behind.
	for {
		after := sched.Next(slipped)
		if after.IsZero() || after.After(minute) {
			return slipped, true
		}
		slipped = after
	}
}

// sameDay reports whether a and b fall on the same calendar day. Both
// come from the same clock, so they share a location.
func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
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
		// Put it back so memory and disk stay in agreement; otherwise the
		// task is gone from this process but returns on the next restart.
		s.tasks[id] = t
		return Task{}, err
	}
	return deleted, nil
}

// Remove deletes a task by ID regardless of which session owns it or
// whether it is durable. It is used to retire tasks whose owning session
// no longer exists; Delete is the session-scoped, user-facing path.
func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[id]; !ok {
		return
	}
	delete(s.tasks, id)
	if err := s.persistLocked(); err != nil {
		slog.Error("Failed to persist scheduled tasks after removal", "id", id, "error", err)
	}
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
	t.LastError = ""
	if !t.Recurring {
		delete(s.tasks, id)
		s.persistBestEffort(id)
		return
	}
	s.rescheduleLocked(t, now)
	s.persistBestEffort(id)
}

// rescheduleLocked advances a recurring task to its next fire time,
// deleting it if it can never fire again. Callers must hold s.mu.
//
// Deleting is the only safe response to an unschedulable task: a zero
// NextRunAt is always in the past, so DueTasks would hand the task back
// on every tick and the scheduler would spin firing it.
func (s *Store) rescheduleLocked(t *Task, now time.Time) {
	sched, err := Parse(t.Cron)
	if err != nil {
		slog.Warn("Deleting scheduled task with unparseable cron expression", "id", t.ID, "cron", t.Cron, "error", err)
		delete(s.tasks, t.ID)
		return
	}
	next := sched.Next(now)
	if next.IsZero() {
		slog.Warn("Deleting scheduled task that can never fire again", "id", t.ID, "cron", t.Cron)
		delete(s.tasks, t.ID)
		return
	}
	t.NextRunAt = next.Truncate(time.Minute)
}

// persistBestEffort writes durable tasks, logging rather than returning
// a failure. Callers must hold s.mu.
func (s *Store) persistBestEffort(id string) {
	if err := s.persistLocked(); err != nil {
		slog.Error("Failed to persist scheduled tasks", "id", id, "error", err)
	}
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
		s.rescheduleLocked(t, now)
	} else {
		delete(s.tasks, id)
	}
	s.persistBestEffort(id)
}

// DropSession removes every task belonging to a session, durable ones
// included.
//
// It is called when a session turns out to no longer exist, which makes
// its durable tasks unrunnable garbage: keeping them would leave the
// scheduler retrying a session that can never come back, logging an
// error and rescheduling on every fire, forever.
func (s *Store) DropSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dropped := false
	for id, t := range s.tasks {
		if t.SessionID == sessionID {
			delete(s.tasks, id)
			dropped = true
		}
	}
	if dropped {
		s.persistBestEffort("")
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

// sortTasks orders tasks by next run, breaking ties on ID. The tiebreak
// matters: tasks are held in a map, and several tasks routinely share a
// next-run time (anything created in the same minute), so without it the
// order CronList prints varies between calls.
func sortTasks(tasks []Task) {
	slices.SortFunc(tasks, func(a, b Task) int {
		if c := a.NextRunAt.Compare(b.NextRunAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}
