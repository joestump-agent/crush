package dispatch

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// NoneProvider's no-ops: Provision yields an empty placement, Diff
// reports no changes, and Release succeeds, so an agent served by it
// gets a registry entry with no path, branch, or base.
func TestNoneProviderNoOps(t *testing.T) {
	var p WorkspaceProvider = NoneProvider{}
	ctx := context.Background()

	placement, err := p.Provision(ctx, "d-1", ProvisionOptions{})
	require.NoError(t, err)
	require.Equal(t, Placement{}, placement)

	diff, err := p.Diff(ctx, Entry{ID: "d-1"})
	require.NoError(t, err)
	require.Empty(t, diff)

	require.NoError(t, p.Release(ctx, Entry{ID: "d-1"}))
}
