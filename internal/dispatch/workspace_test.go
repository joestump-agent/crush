package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newGitRegistry returns a Registry backed by a real git repo, plus the
// repo path, for the create -> track -> diff -> cleanup lifecycle tests.
func newGitRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	repo := initRepo(t)
	dir := filepath.Join(t.TempDir(), "dispatch")
	return NewRegistry(dir, NewGitBackend(repo)), repo
}

func TestRegistry_CreateTracksWorkspace(t *testing.T) {
	t.Parallel()
	reg, _ := newGitRegistry(t)

	ws, err := reg.Create(context.Background(), "")
	require.NoError(t, err)
	require.NotEmpty(t, ws.ID)
	require.Equal(t, branchPrefix+ws.ID, ws.Branch)
	require.Equal(t, StatusPending, ws.Status)
	require.DirExists(t, ws.Path)

	got, ok := reg.Get(ws.ID)
	require.True(t, ok)
	require.Equal(t, ws.ID, got.ID)

	require.Len(t, reg.List(), 1)
}

func TestRegistry_ListIsSortedAndIsolated(t *testing.T) {
	t.Parallel()
	reg, _ := newGitRegistry(t)

	a, err := reg.Create(context.Background(), "")
	require.NoError(t, err)
	b, err := reg.Create(context.Background(), "")
	require.NoError(t, err)

	list := reg.List()
	require.Len(t, list, 2)
	require.True(t, list[0].ID < list[1].ID, "list should be sorted by id")

	// Mutating a returned snapshot does not touch tracked state.
	list[0].Status = StatusFailed
	fresh, _ := reg.Get(list[0].ID)
	require.Equal(t, StatusPending, fresh.Status)

	_ = a
	_ = b
}

func TestRegistry_SettersMutateTrackedEntry(t *testing.T) {
	t.Parallel()
	reg, _ := newGitRegistry(t)
	ws, err := reg.Create(context.Background(), "")
	require.NoError(t, err)

	require.True(t, reg.SetStatus(ws.ID, StatusRunning))
	require.True(t, reg.SetSessionID(ws.ID, "sess-1"))
	require.True(t, reg.SetServed(ws.ID, "http://127.0.0.1:9000", "card"))

	got, ok := reg.Get(ws.ID)
	require.True(t, ok)
	require.Equal(t, StatusRunning, got.Status)
	require.Equal(t, "sess-1", got.SessionID)
	require.Equal(t, "http://127.0.0.1:9000", got.Endpoint)
	require.Equal(t, "card", got.Card)

	// Setters on an unknown workspace report not-found rather than panic.
	require.False(t, reg.SetStatus("nope", StatusComplete))
	require.False(t, reg.SetSessionID("nope", "x"))
	require.False(t, reg.SetServed("nope", "x", nil))
}

func TestRegistry_DiffReturnsWork(t *testing.T) {
	t.Parallel()
	reg, _ := newGitRegistry(t)
	ws, err := reg.Create(context.Background(), "")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(ws.Path, "feature.go"), []byte("package feature\n"), 0o644))

	diff, err := reg.Diff(context.Background(), ws.ID)
	require.NoError(t, err)
	require.Contains(t, diff, "feature.go")
}

func TestRegistry_DiffUnknownWorkspaceErrors(t *testing.T) {
	t.Parallel()
	reg, _ := newGitRegistry(t)
	_, err := reg.Diff(context.Background(), "missing")
	require.Error(t, err)
}

func TestRegistry_RemoveTearsDownAndUntracks(t *testing.T) {
	t.Parallel()
	reg, _ := newGitRegistry(t)
	ws, err := reg.Create(context.Background(), "")
	require.NoError(t, err)

	require.NoError(t, reg.Remove(context.Background(), ws.ID))
	require.NoDirExists(t, ws.Path)

	_, ok := reg.Get(ws.ID)
	require.False(t, ok)
	require.Empty(t, reg.List())

	// Removing an already-removed (untracked) workspace is a no-op.
	require.NoError(t, reg.Remove(context.Background(), ws.ID))
}

func TestRegistry_CloseSweepsRemaining(t *testing.T) {
	t.Parallel()
	reg, _ := newGitRegistry(t)

	a, err := reg.Create(context.Background(), "")
	require.NoError(t, err)
	b, err := reg.Create(context.Background(), "")
	require.NoError(t, err)

	require.NoError(t, reg.Close(context.Background()))

	require.NoDirExists(t, a.Path)
	require.NoDirExists(t, b.Path)
	require.Empty(t, reg.List())
}

// failBackend is a Backend whose Create fails, to prove the registry
// surfaces provisioning errors without tracking a phantom workspace.
type failBackend struct{}

func (failBackend) Create(context.Context, string, string, string) (Provisioned, error) {
	return Provisioned{}, errors.New("boom")
}
func (failBackend) Diff(context.Context, string, string) (string, error) { return "", nil }
func (failBackend) Remove(context.Context, string, string) error         { return nil }

func TestRegistry_CreateFailureIsNotTracked(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(filepath.Join(t.TempDir(), "dispatch"), failBackend{})

	_, err := reg.Create(context.Background(), "")
	require.Error(t, err)
	require.Empty(t, reg.List())
}
