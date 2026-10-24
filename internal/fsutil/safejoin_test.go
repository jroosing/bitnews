package fsutil_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func TestSafeJoin_JoinsRelativePathsUnderRoot(t *testing.T) {
	root := t.TempDir()
	tests := map[string]string{
		"file.txt":           "file.txt",
		"a/b/c.txt":          filepath.Join("a", "b", "c.txt"),
		"./a//b/":            filepath.Join("a", "b"),
		"a/../b":             "b",
		"a/b/../../c":        "c",
		"":                   "",
		".":                  "",
		`windows\style\path`: filepath.Join("windows", "style", "path"),
	}
	for rel, want := range tests {
		t.Run(rel, func(t *testing.T) {
			got, err := fsutil.SafeJoin(root, rel)
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(root, want), got)
		})
	}
}

func TestSafeJoin_RejectsLexicalEscapes(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"..",
		"../x",
		"a/../../x",
		"a/b/../../../x",
		`..\x`,
		`a\..\..\x`,
		"/etc/passwd",
		`\etc\passwd`,
		"//host/share",
		"a\x00b",
		"\x00",
	} {
		t.Run(rel, func(t *testing.T) {
			got, err := fsutil.SafeJoin(root, rel)
			assert.ErrorIs(t, err, fsutil.ErrUnsafePath)
			assert.Empty(t, got)
		})
	}
}

func TestSafeJoin_RejectsEmptyOrNULRoot(t *testing.T) {
	for _, root := range []string{"", "a\x00b"} {
		_, err := fsutil.SafeJoin(root, "x")
		assert.ErrorIs(t, err, fsutil.ErrUnsafePath)
	}
}

func TestSafeJoin_SiblingWithRootAsPrefixIsNotUnderRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "data")
	require.NoError(t, os.Mkdir(root, 0o755))
	require.NoError(t, os.Mkdir(root+"-evil", 0o755))

	_, err := fsutil.SafeJoin(root, "../data-evil/x")
	assert.ErrorIs(t, err, fsutil.ErrUnsafePath)
}

func TestSafeJoin_ResolvesSymlinksInExistingParents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "real", "sub"), 0o755))
	require.NoError(t, os.Mkdir(outside, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	require.NoError(t, os.Symlink("real", filepath.Join(root, "inside")))
	require.NoError(t, os.Symlink(filepath.Join(base, "does-not-exist"), filepath.Join(root, "dangling")))
	require.NoError(t, os.Symlink("../../outside", filepath.Join(root, "real", "rel-escape")))
	require.NoError(t, os.Symlink("../../..", filepath.Join(root, "real", "sub", "up")))

	rejected := []string{
		"escape",
		"escape/file.txt",
		"escape/new/deeper/file.txt",
		"dangling",
		"dangling/file.txt",
		"real/rel-escape/file.txt",
		"real/sub/up/outside/x",
	}
	for _, rel := range rejected {
		t.Run("rejects "+rel, func(t *testing.T) {
			_, err := fsutil.SafeJoin(root, rel)
			assert.ErrorIs(t, err, fsutil.ErrUnsafePath)
		})
	}

	t.Run("allows symlink that stays inside and returns unresolved path", func(t *testing.T) {
		got, err := fsutil.SafeJoin(root, "inside/sub/new.txt")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "inside", "sub", "new.txt"), got)
	})
	t.Run("allows non-existing tail", func(t *testing.T) {
		got, err := fsutil.SafeJoin(root, "real/a/b/c.txt")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "real", "a", "b", "c.txt"), got)
	})
}

func TestSafeJoin_RootThroughSymlinkedAncestorIsAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(realRoot, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(realRoot, link))

	got, err := fsutil.SafeJoin(link, "a/b.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(link, "a", "b.txt"), got)
}

func TestSafeJoin_NonExistingRootHasNothingToResolve(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not", "yet")
	got, err := fsutil.SafeJoin(root, "a/b.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "a", "b.txt"), got)
}
