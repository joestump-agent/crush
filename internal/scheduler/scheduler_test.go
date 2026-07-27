package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestDropSessionKeepsDurable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	store := NewStore(path)

	_, err := store.Create("s1", "* * * * *", "session task", true, false)
	require.NoError(t, err)
	durableTask, err := store.Create("s1", "* * * * *", "durable task", true, true)
	require.NoError(t, err)

	store.DropSession("s1")

	remaining := store.ListAll()
	require.Len(t, remaining, 1)
	require.Equal(t, durableTask.ID, remaining[0].ID)
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
