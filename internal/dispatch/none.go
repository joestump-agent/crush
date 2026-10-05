package dispatch

import "context"

// NoneProvider is the [WorkspaceProvider] for agents that need no
// isolated workspace: every method is a no-op and Diff reports no
// changes. A provisioned entry gets an empty [Placement], so the
// registry entry carries no path, branch, or base. Nothing selects it
// yet; #433's per-agent provider selection will.
type NoneProvider struct{}

var _ WorkspaceProvider = NoneProvider{}

// Provision implements [WorkspaceProvider] with a no-op: it creates
// nothing and returns an empty placement.
func (NoneProvider) Provision(ctx context.Context, id string, opts ProvisionOptions) (Placement, error) {
	return Placement{}, nil
}

// Diff implements [WorkspaceProvider] with a no-op: a workspace-less
// agent has no diff to report.
func (NoneProvider) Diff(ctx context.Context, entry Entry) (string, error) {
	return "", nil
}

// Release implements [WorkspaceProvider] with a no-op: nothing was
// created, so there is nothing to tear down.
func (NoneProvider) Release(ctx context.Context, entry Entry) error {
	return nil
}
