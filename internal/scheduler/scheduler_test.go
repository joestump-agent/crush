package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore("")
	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 30, 0, 0, time.Local)
	}
	return store
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()

	store := testStore(t)

	_, err := store.Create("s1", "not a cron", "do something", true, false)
	require.Error(t, err)

	_, err = store.Create("s1", "* * * * *", "", true, false)
	require.Error(t, err)

	// Durable requires a persistence path.
	_, err = store.Create("s1", "* * * * *", "do something", true, true)
	require.Error(t, err)

	task, err := store.Create("s1", "* * * * *", "do something", true, false)
	require.NoError(t, err)
	require.Len(t, task.ID, 8)
	require.Equal(t, "s1", task.SessionID)
	require.True(t, task.Recurring)
	require.False(t, task.Durable)
	require.True(t, task.NextRunAt.After(task.CreatedAt.Local()) || task.NextRunAt.Equal(store.now().Truncate(time.Minute).Add(time.Minute)))
}

func TestCreateEnforcesSessionLimit(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	for range MaxTasksPerSession {
		_, err := store.Create("s1", "* * * * *", "p", true, false)
		require.NoError(t, err)
	}
	_, err := store.Create("s1", "* * * * *", "p", true, false)
	require.ErrorIs(t, err, ErrTooManyTasks)

	// A different session is unaffected.
	_, err = store.Create("s2", "* * * * *", "p", true, false)
	require.NoError(t, err)
}

func TestListScopedToSession(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	_, err := store.Create("s1", "* * * * *", "first", true, false)
	require.NoError(t, err)
	_, err = store.Create("s1", "0 9 * * *", "second", true, false)
	require.NoError(t, err)
	_, err = store.Create("s2", "* * * * *", "other session", true, false)
	require.NoError(t, err)

	tasks := store.List("s1")
	require.Len(t, tasks, 2)
	for _, task := range tasks {
		require.Equal(t, "s1", task.SessionID)
	}
	// Sorted by next run.
	require.True(t, !tasks[0].NextRunAt.After(tasks[1].NextRunAt))

	require.Len(t, store.List("unknown"), 0)
	require.Len(t, store.ListAll(), 3)
}

func TestDelete(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	task, err := store.Create("s1", "* * * * *", "p", true, false)
	require.NoError(t, err)

	deleted, err := store.Delete("s1", task.ID)
	require.NoError(t, err)
	require.Equal(t, task.ID, deleted.ID)
	require.Len(t, store.ListAll(), 0)

	_, err = store.Delete("s1", task.ID)
	require.ErrorIs(t, err, ErrTaskNotFound)
}

func TestDeleteRejectsCrossSession(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	task, err := store.Create("s1", "* * * * *", "p", true, false)
	require.NoError(t, err)

	_, err = store.Delete("s2", task.ID)
	require.ErrorIs(t, err, ErrTaskNotFound)
	require.Len(t, store.ListAll(), 1)
}

func TestDueTasksAndMarkFiredOneShot(t *testing.T) {
	t.Parallel()

	// Created at 10:28 with a one-shot pinned to 10:29 today.
	store := testStore(t)
	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 28, 0, 0, time.Local)
	}
	task, err := store.Create("s1", "29 10 27 7 *", "p", false, false)
	require.NoError(t, err)
	require.Len(t, store.DueTasks(), 0)

	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 29, 30, 0, time.Local)
	}
	due := store.DueTasks()
	require.Len(t, due, 1)
	require.Equal(t, task.ID, due[0].ID)

	// One-shot deletes itself after firing.
	store.MarkFired(task.ID)
	require.Len(t, store.ListAll(), 0)
}

func TestMarkFiredRecurringReschedules(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 28, 0, 0, time.Local)
	}
	task, err := store.Create("s1", "29 10 27 7 *", "p", true, false)
	require.NoError(t, err)

	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 30, 0, 0, time.Local)
	}
	store.MarkFired(task.ID)

	tasks := store.ListAll()
	require.Len(t, tasks, 1)
	require.Equal(t, 1, tasks[0].RunCount)
	require.NotNil(t, tasks[0].LastRunAt)
	require.True(t, tasks[0].NextRunAt.After(store.now()))
}

func TestMarkErrorReschedulesRecurring(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 28, 0, 0, time.Local)
	}
	task, err := store.Create("s1", "29 10 27 7 *", "p", true, false)
	require.NoError(t, err)

	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 30, 0, 0, time.Local)
	}
	store.MarkError(task.ID, errors.New("boom"))

	tasks := store.ListAll()
	require.Len(t, tasks, 1)
	require.Equal(t, "boom", tasks[0].LastError)
	require.True(t, tasks[0].NextRunAt.After(store.now()))
}

func TestMarkErrorDeletesOneShot(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 28, 0, 0, time.Local)
	}
	task, err := store.Create("s1", "29 10 27 7 *", "p", false, false)
	require.NoError(t, err)

	store.MarkError(task.ID, errors.New("boom"))
	require.Len(t, store.ListAll(), 0)
}

// DropSession is only called once a session has turned out not to
// exist, which makes its durable tasks unrunnable: every fire would look
// the session up, fail, and reschedule. So it drops durable tasks too,
// and the drop is persisted so they do not come back on restart.
func TestDropSessionDropsDurableTasksToo(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	store := NewStore(path)

	_, err := store.Create("s1", "* * * * *", "session task", true, false)
	require.NoError(t, err)
	_, err = store.Create("s1", "* * * * *", "durable task", true, true)
	require.NoError(t, err)
	survivor, err := store.Create("s2", "* * * * *", "other session", true, true)
	require.NoError(t, err)

	store.DropSession("s1")

	remaining := store.ListAll()
	require.Len(t, remaining, 1)
	require.Equal(t, survivor.ID, remaining[0].ID, "only the other session's task should survive")

	// The durable task must be gone from disk as well, or a restart
	// resurrects a task pinned to a session that no longer exists.
	reloaded := NewStore(path)
	require.NoError(t, reloaded.Load())
	after := reloaded.ListAll()
	require.Len(t, after, 1)
	require.Equal(t, survivor.ID, after[0].ID)
}

func TestDurablePersistenceRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")

	store := NewStore(path)
	durableTask, err := store.Create("s1", "0 9 * * *", "durable", true, true)
	require.NoError(t, err)
	_, err = store.Create("s1", "* * * * *", "in-memory only", true, false)
	require.NoError(t, err)

	// Only the durable task lands on disk.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var persisted []Task
	require.NoError(t, json.Unmarshal(data, &persisted))
	require.Len(t, persisted, 1)
	require.Equal(t, durableTask.ID, persisted[0].ID)

	// A fresh store loads the durable task back.
	reloaded := NewStore(path)
	require.NoError(t, reloaded.Load())
	tasks := reloaded.ListAll()
	require.Len(t, tasks, 1)
	require.Equal(t, durableTask.ID, tasks[0].ID)
	require.Equal(t, "durable", tasks[0].Prompt)
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	t.Parallel()

	store := NewStore(filepath.Join(t.TempDir(), "does-not-exist.json"))
	require.NoError(t, store.Load())
}

func TestDeletePersists(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	store := NewStore(path)

	task, err := store.Create("s1", "0 9 * * *", "durable", true, true)
	require.NoError(t, err)
	_, err = store.Delete("s1", task.ID)
	require.NoError(t, err)

	reloaded := NewStore(path)
	require.NoError(t, reloaded.Load())
	require.Len(t, reloaded.ListAll(), 0)
}

func TestSchedulerTickFiresDueTasks(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 28, 0, 0, time.Local)
	}
	task, err := store.Create("s1", "29 10 27 7 *", "p", true, false)
	require.NoError(t, err)

	var fired atomic.Int32
	sched := NewScheduler(store, func(_ context.Context, firedTask Task) error {
		require.Equal(t, task.ID, firedTask.ID)
		fired.Add(1)
		return nil
	})

	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 30, 0, 0, time.Local)
	}
	sched.Tick(context.Background())
	require.Equal(t, int32(1), fired.Load())

	// The recurring task rescheduled, so it is no longer due.
	sched.Tick(context.Background())
	require.Equal(t, int32(1), fired.Load())
}

func TestSchedulerTickRecordsErrors(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 28, 0, 0, time.Local)
	}
	_, err := store.Create("s1", "29 10 27 7 *", "p", true, false)
	require.NoError(t, err)

	sched := NewScheduler(store, func(_ context.Context, _ Task) error {
		return errors.New("fire failed")
	})

	store.now = func() time.Time {
		return time.Date(2026, 7, 27, 10, 30, 0, 0, time.Local)
	}
	sched.Tick(context.Background())

	tasks := store.ListAll()
	require.Len(t, tasks, 1)
	require.Equal(t, "fire failed", tasks[0].LastError)
}

// A schedule that parses but can never match must be rejected at
// creation. Next reports it as the zero time, and a zero NextRunAt is
// always in the past, so accepting one would produce a task that
// DueTasks hands back on every single tick.
func TestCreateRejectsScheduleThatNeverFires(t *testing.T) {
	t.Parallel()

	store := testStore(t)

	_, err := store.Create("s1", "0 0 30 2 *", "February 30th", true, false)
	require.ErrorIs(t, err, ErrNeverFires)
	require.Empty(t, store.ListAll(), "a rejected task must not be stored")
}

// A one-shot pinned to a minute that already passed must be rejected:
// its next match has jumped to tomorrow or next year, which is never
// what "remind me in 3 minutes" means. This is the signature of cron
// fields computed against a stale clock. Recurring tasks are exempt —
// "0 9 * * *" created at 10:30 legitimately fires tomorrow.
func TestCreateRejectsOneShotInPast(t *testing.T) {
	t.Parallel()

	// testStore's clock is 2026-07-27 10:30 local.
	store := testStore(t)

	// 09:15 today already passed: the next match is a year out.
	_, err := store.Create("s1", "15 9 27 7 *", "stale-clock one-shot", false, false)
	require.ErrorIs(t, err, ErrOneShotInPast)
	require.Empty(t, store.ListAll(), "a rejected task must not be stored")

	// 10:45 today is still ahead: accepted, fires today.
	task, err := store.Create("s1", "45 10 27 7 *", "future one-shot", false, false)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 7, 27, 10, 45, 0, 0, time.Local), task.NextRunAt)

	// The current minute counts as already passed: robfig treats it as
	// consumed, so a fully-pinned one-shot for 10:30 created at 10:30
	// would otherwise store a next run a year out.
	_, err = store.Create("s1", "30 10 27 7 *", "this minute", false, false)
	require.ErrorIs(t, err, ErrOneShotInPast)

	// A daily recurring schedule whose time passed today is fine.
	_, err = store.Create("s1", "0 9 * * *", "recurring 9am", true, false)
	require.NoError(t, err)
}

// The guard has to survive midnight. A one-shot pinned to YESTERDAY's
// date used to sail through, because the old check only looked back as
// far as this morning's midnight: the next match was next year, which
// is trivially "not earlier today", so the task was accepted and stored
// to fire in 2027 (issue #272). The rejection now carries the more
// specific ErrOneShotDateInPast, because the fix is to recompute the
// date fields rather than just the minute.
func TestCreateRejectsOneShotPinnedToAnEarlierDate(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	// One minute past midnight, 2026-07-28 — the reported scenario: a
	// session that crossed midnight while the agent was computing.
	store.now = func() time.Time {
		return time.Date(2026, 7, 28, 0, 1, 0, 0, time.Local)
	}

	_, err := store.Create("s1", "22 22 27 7 *", "yesterday's date", false, false)
	require.ErrorIs(t, err, ErrOneShotDateInPast)
	require.ErrorIs(t, err, ErrOneShotInPast, "the specific error must still satisfy the general one")
	require.Empty(t, store.ListAll(), "a rejected task must not be stored")

	// The message must name BOTH the match that slipped and the next
	// one. Reporting only the next match reads as the scheduler jumping
	// a year for no reason, which is how issue #272 got misdiagnosed as
	// a timezone bug.
	require.Contains(t, err.Error(), "2026-07-27T22:22:00")
	require.Contains(t, err.Error(), "2027-07-27T22:22:00")

	// When several matches slipped inside the window, the message names
	// the last one — the moment the caller most likely meant.
	_, err = store.Create("s1", "0 22,23 27 7 *", "twice yesterday", false, false)
	require.ErrorIs(t, err, ErrOneShotDateInPast)
	require.Contains(t, err.Error(), "2026-07-27T23:00:00")
	require.NotContains(t, err.Error(), "2026-07-27T22:00:00")

	// Same clock, but the slip was earlier the same day: the general
	// error, not the date-specific one.
	store.now = func() time.Time {
		return time.Date(2026, 7, 28, 23, 0, 0, 0, time.Local)
	}
	_, err = store.Create("s1", "22 22 28 7 *", "earlier today", false, false)
	require.ErrorIs(t, err, ErrOneShotInPast)
	require.NotErrorIs(t, err, ErrOneShotDateInPast)
}

// The guard must not reject a one-shot the caller can still use. Both
// halves of the check earn their place here: a schedule that comes back
// around within the day loses nothing to a slipped match, and a date
// months out has no recent match to be confused by.
func TestCreateAcceptsOneShotsThatStillFireSoon(t *testing.T) {
	t.Parallel()

	// testStore's clock is 2026-07-27 10:30 local.
	store := testStore(t)

	// Matched a minute ago AND matches again in a minute: the caller
	// wanted "the next one", and gets it. The midnight-based guard used
	// to reject this outright.
	task, err := store.Create("s1", "*/5 * * * *", "next five-minute boundary", false, false)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 7, 27, 10, 35, 0, 0, time.Local), task.NextRunAt)

	// Matched at 20:00 yesterday, matches again at 20:00 tonight —
	// inside the day, so "8pm" still means tonight.
	task, err = store.Create("s1", "0 20 * * *", "8pm tonight", false, false)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local), task.NextRunAt)

	// A legitimate far-future one-shot: no match in the last day, so
	// nothing slipped. Christmas is four months out, not stale.
	task, err = store.Create("s1", "0 9 25 12 *", "christmas morning", false, false)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 12, 25, 9, 0, 0, 0, time.Local), task.NextRunAt)
}

// Store.Create takes no caller-supplied timestamp: the only clock is
// s.now, which NewStore wires to time.Now. Issue #272 suspected a
// prompt-derived time leaking in through a caller; this pins that there
// is no seam for one to enter through, so the suspicion cannot silently
// become true later.
func TestStoreClockIsTimeNow(t *testing.T) {
	t.Parallel()

	store := NewStore("")
	require.WithinDuration(t, time.Now(), store.now(), time.Minute)
}

// The same guard has to hold on the rescheduling path. A recurring task
// whose next fire falls outside the lookahead window is deleted rather
// than left with a zero NextRunAt, which would spin the scheduler.
func TestMarkFiredDeletesTaskThatCanNeverFireAgain(t *testing.T) {
	t.Parallel()

	store := testStore(t)

	// Create against a schedule that fires, then swap in one that cannot,
	// simulating a task whose remaining fires fall outside the window.
	task, err := store.Create("s1", "* * * * *", "task", true, false)
	require.NoError(t, err)
	store.mu.Lock()
	store.tasks[task.ID].Cron = "0 0 30 2 *"
	store.mu.Unlock()

	store.MarkFired(task.ID)

	require.Empty(t, store.ListAll(), "an unschedulable recurring task must be dropped, not left perpetually due")
}

// A recurring task whose cron became unparseable (a hand-edited durable
// file) must be dropped rather than left in place with a stale
// NextRunAt that keeps coming back due.
func TestMarkFiredDeletesTaskWithUnparseableCron(t *testing.T) {
	t.Parallel()

	store := testStore(t)

	task, err := store.Create("s1", "* * * * *", "task", true, false)
	require.NoError(t, err)
	store.mu.Lock()
	store.tasks[task.ID].Cron = "totally bogus"
	store.mu.Unlock()

	store.MarkFired(task.ID)
	require.Empty(t, store.ListAll())
}

// A fire that succeeds after a failure must clear the recorded error,
// otherwise CronList keeps showing a stale "Last error" forever.
func TestMarkFiredClearsPreviousError(t *testing.T) {
	t.Parallel()

	store := testStore(t)

	task, err := store.Create("s1", "* * * * *", "task", true, false)
	require.NoError(t, err)

	store.MarkError(task.ID, errors.New("transient failure"))
	listed := store.List("s1")
	require.Len(t, listed, 1)
	require.Equal(t, "transient failure", listed[0].LastError)

	store.MarkFired(task.ID)
	listed = store.List("s1")
	require.Len(t, listed, 1)
	require.Empty(t, listed[0].LastError, "a successful fire must clear the previous error")
	require.Equal(t, 1, listed[0].RunCount)
}

// Tasks live in a map, and everything created in the same minute shares
// a NextRunAt, so ordering needs a tiebreak or CronList prints a
// different order on each call.
func TestListOrderIsDeterministic(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	for i := range 10 {
		_, err := store.Create("s1", "* * * * *", fmt.Sprintf("task %d", i), true, false)
		require.NoError(t, err)
	}

	first := store.List("s1")
	require.Len(t, first, 10)
	for range 20 {
		require.Equal(t, first, store.List("s1"), "List must return a stable order")
	}
	require.True(t, slices.IsSortedFunc(first, func(a, b Task) int {
		if c := a.NextRunAt.Compare(b.NextRunAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	}), "tasks must be ordered by next run, then ID")
}

// Remove retires a task regardless of owner or durability, and the
// removal has to reach disk so a restart does not resurrect it.
func TestRemoveDropsDurableTaskAndPersists(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	store := NewStore(path)

	task, err := store.Create("s1", "* * * * *", "durable", true, true)
	require.NoError(t, err)

	store.Remove(task.ID)
	require.Empty(t, store.ListAll())

	reloaded := NewStore(path)
	require.NoError(t, reloaded.Load())
	require.Empty(t, reloaded.ListAll())

	// Removing an unknown ID is a no-op, not a panic.
	store.Remove("nonexistent")
}

// Load is the one path that ingests attacker-or-corruption-shaped data:
// a durable file edited by hand, or written by an older build. Nothing
// in it may produce a task that is perpetually due.
func TestLoadSkipsUnusableTasks(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	stored := []Task{
		{ID: "good0001", SessionID: "s1", Prompt: "ok", Cron: "* * * * *", Recurring: true, Durable: true, NextRunAt: time.Now().Add(time.Hour)},
		{ID: "badcron1", SessionID: "s1", Prompt: "bad cron", Cron: "not a cron", Recurring: true, Durable: true, NextRunAt: time.Now().Add(time.Hour)},
		{ID: "neverfir", SessionID: "s1", Prompt: "impossible", Cron: "0 0 30 2 *", Recurring: true, Durable: true},
		{ID: "notdurab", SessionID: "s1", Prompt: "not durable", Cron: "* * * * *", Recurring: true, Durable: false, NextRunAt: time.Now().Add(time.Hour)},
		{ID: "", SessionID: "s1", Prompt: "no id", Cron: "* * * * *", Recurring: true, Durable: true, NextRunAt: time.Now().Add(time.Hour)},
	}
	data, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	store := NewStore(path)
	require.NoError(t, store.Load())

	loaded := store.ListAll()
	require.Len(t, loaded, 1)
	require.Equal(t, "good0001", loaded[0].ID)

	// Nothing loaded may be due immediately.
	require.Empty(t, store.DueTasks())
}

// A durable file holding more than the per-session cap must not let a
// session exceed it after a restart.
func TestLoadEnforcesSessionLimit(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	stored := make([]Task, 0, MaxTasksPerSession+10)
	for i := range MaxTasksPerSession + 10 {
		stored = append(stored, Task{
			ID:        fmt.Sprintf("task%04d", i),
			SessionID: "s1",
			Prompt:    "p",
			Cron:      "* * * * *",
			Recurring: true,
			Durable:   true,
			NextRunAt: time.Now().Add(time.Hour),
		})
	}
	data, err := json.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	store := NewStore(path)
	require.NoError(t, store.Load())
	require.Len(t, store.ListAll(), MaxTasksPerSession)
}

// The durable file can hold prompts, so it must not be world-readable.
func TestDurableFileIsNotWorldReadable(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		// Windows has no Unix permission bits: os.Chmod there only
		// toggles the read-only attribute, and Stat reports 0666.
		t.Skip("permission bits are not meaningful on Windows")
	}

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	store := NewStore(path)
	_, err := store.Create("s1", "* * * * *", "secret prompt", true, true)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// The store is read by the scheduler goroutine while tools mutate it
// from the agent's turn, so every exported method must be safe to call
// concurrently. Run with -race.
func TestStoreIsRaceFree(t *testing.T) {
	t.Parallel()

	store := NewStore(filepath.Join(t.TempDir(), "scheduled_tasks.json"))
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			session := fmt.Sprintf("s%d", worker%3)
			for range 40 {
				task, err := store.Create(session, "* * * * *", "p", true, worker%2 == 0)
				if err != nil {
					// Hitting the per-session cap is legitimate here.
					continue
				}
				store.List(session)
				store.ListAll()
				store.DueTasks()
				store.MarkFired(task.ID)
				store.MarkError(task.ID, errors.New("boom"))
				if _, err := store.Delete(session, task.ID); err != nil {
					store.Remove(task.ID)
				}
			}
		}(worker)
	}
	wg.Wait()
}
