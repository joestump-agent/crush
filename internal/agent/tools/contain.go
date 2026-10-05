package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// containmentRootKey keys the dispatch workspace root a tool call is
// contained to on its context.
type containmentRootKey struct{}

// WithContainmentRoot returns a context whose file-tool paths are
// contained to root. Tools that accept paths resolve them, following
// symlinks, against the root's real location and refuse anything that
// escapes it. The main agent's context carries no root, so containment
// is a no-op there.
func WithContainmentRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, containmentRootKey{}, root)
}

// ContainPath resolves p against the containment root on ctx. With no
// root on the context it returns p unchanged. With a root it resolves
// p, following symlinks, and rejects anything outside the root,
// including a path that reaches the outside through a symlink inside
// it. A final component that does not exist yet (a file about to be
// written) is resolved against its deepest existing ancestor.
func ContainPath(ctx context.Context, p string) (string, error) {
	root, ok := ctx.Value(containmentRootKey{}).(string)
	if !ok || root == "" {
		return p, nil
	}
	resolved, err := resolveWithin(root, p)
	if err != nil {
		return "", fmt.Errorf("path %s is outside the dispatch workspace %s", p, root)
	}
	return resolved, nil
}

// resolveWithin reports the real location of p and whether it stays
// inside root. Both sides go through EvalSymlinks, so a root that is
// itself a symlink (macOS /var to /private/var) compares equal to the
// same path spelled through the link.
func resolveWithin(root, p string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}

	abs := p
	if filepath.IsAbs(abs) {
		abs = filepath.Clean(abs)
	} else {
		abs = filepath.Join(root, abs)
	}

	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", err
	}

	rel, err := filepath.Rel(realRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the containment root")
	}
	return resolved, nil
}

// resolveExisting resolves every existing component of path through
// EvalSymlinks and re-appends the not-yet-existing tail, so a write to
// a new file in a new directory still lands where the path says.
func resolveExisting(path string) (string, error) {
	probe := path
	tail := ""
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		tail = filepath.Join(filepath.Base(probe), tail)
		probe = parent
	}
}
