package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainPathWithoutRoot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "anywhere.txt")
	got, err := ContainPath(t.Context(), p)
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestContainPath(t *testing.T) {
	root := t.TempDir()
	ctx := WithContainmentRoot(t.Context(), root)

	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("x"), 0o644))

	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	t.Run("relative inside", func(t *testing.T) {
		got, err := ContainPath(ctx, filepath.Join("sub", "file.txt"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(realRoot, "sub", "file.txt"), got)
	})

	t.Run("absolute inside", func(t *testing.T) {
		got, err := ContainPath(ctx, filepath.Join(root, "sub", "file.txt"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(realRoot, "sub", "file.txt"), got)
	})

	t.Run("the root itself", func(t *testing.T) {
		got, err := ContainPath(ctx, root)
		require.NoError(t, err)
		require.Equal(t, realRoot, got)
	})

	t.Run("new file in new directory", func(t *testing.T) {
		got, err := ContainPath(ctx, filepath.Join("newdir", "newfile.txt"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(realRoot, "newdir", "newfile.txt"), got)
	})

	t.Run("relative escape", func(t *testing.T) {
		_, err := ContainPath(ctx, filepath.Join("..", "outside.txt"))
		require.Error(t, err)
	})

	t.Run("absolute escape", func(t *testing.T) {
		_, err := ContainPath(ctx, filepath.Join(root, "..", "outside.txt"))
		require.Error(t, err)
	})

	t.Run("escape that cleans back inside", func(t *testing.T) {
		got, err := ContainPath(ctx, filepath.Join("sub", "..", "file.txt"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(realRoot, "file.txt"), got)
	})
}

func TestContainPathSymlinks(t *testing.T) {
	t.Run("symlink inside pointing outside", func(t *testing.T) {
		root := t.TempDir()
		ctx := WithContainmentRoot(t.Context(), root)

		outsideFile := filepath.Join(t.TempDir(), "outside.txt")
		require.NoError(t, os.WriteFile(outsideFile, []byte("x"), 0o644))

		link := filepath.Join(root, "escape")
		if err := os.Symlink(outsideFile, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := ContainPath(ctx, link)
		require.Error(t, err)
	})

	t.Run("root reached through a symlink", func(t *testing.T) {
		realDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(realDir, "file.txt"), []byte("x"), 0o644))

		rootLink := filepath.Join(t.TempDir(), "rootlink")
		if err := os.Symlink(realDir, rootLink); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		realDirResolved, err := filepath.EvalSymlinks(realDir)
		require.NoError(t, err)

		got, err := ContainPath(WithContainmentRoot(t.Context(), rootLink), filepath.Join(rootLink, "file.txt"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(realDirResolved, "file.txt"), got)
	})
}
