package par2_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

// hostileDir is a Download folder called dl inside a root that also holds a
// bystander file, plus an obfuscated copy of the data that a hostile PAR2
// file names with an attacker-chosen path. If par2 acted on that name it
// would rename the copy there.
func hostileDir(t *testing.T, name string, parFile func(*testSet) []byte) (root, dl string) {
	t.Helper()
	root = t.TempDir()
	dl = filepath.Join(root, "dl")
	require.NoError(t, os.Mkdir(dl, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bystander.txt"), []byte("mine"), 0o644))
	data := payload(3, 2500)
	require.NoError(t, os.WriteFile(filepath.Join(dl, "obfuscated"), data, 0o644))
	set := newSet(blockSize, fileSpec{Name: name, Data: data})
	require.NoError(t, os.WriteFile(filepath.Join(dl, "set.par2"), parFile(set), 0o644))
	return root, dl
}

func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		out[rel] = string(b)
		return nil
	}))
	return out
}

func TestAHostileSetIsRefusedBeforePar2Starts(t *testing.T) {
	hostile := []string{
		"../escaped.bin", "sub/../../escaped.bin", "/tmp/escaped.bin", `..\escaped.bin`, `C:\escaped.bin`, "C:escaped.bin",
		"a\x00../../escaped.bin", "", "..", "dir//x", "x\nRepair is possible.",
	}
	for _, name := range hostile {
		t.Run(name, func(t *testing.T) {
			root, dl := hostileDir(t, name, func(s *testSet) []byte { return s.index() })
			before := tree(t, root)
			// A program that does not exist: if the check let the set through,
			// the error would be a start failure, not ErrUnsafeSet.
			tl := par2.Tool{Path: "/nonexistent/par2"}

			_, err := tl.Verify(context.Background(), dl)
			assert.ErrorIs(t, err, par2.ErrUnsafeSet, "verify")
			_, err = tl.Repair(context.Background(), dl)
			assert.ErrorIs(t, err, par2.ErrUnsafeSet, "repair")

			assert.Equal(t, before, tree(t, root), "nothing moved, nothing created")
		})
	}
}

func TestAHostileSetIsRefusedWithTheRealPar2AndNothingLandsOutside(t *testing.T) {
	root, dl := hostileDir(t, "../escaped.bin", func(s *testSet) []byte { return s.index() })
	before := tree(t, root)

	_, err := toolFor(t).Verify(context.Background(), dl)
	require.ErrorIs(t, err, par2.ErrUnsafeSet)
	_, err = toolFor(t).Repair(context.Background(), dl)
	require.ErrorIs(t, err, par2.ErrUnsafeSet)

	assert.Equal(t, before, tree(t, root))
}

func TestAHostileNameHiddenInARecoveryVolumeIsFound(t *testing.T) {
	// The index is clean; the evil description sits inside the payload of a
	// recovery packet of a second file. par2 finds packets by magic wherever
	// it resynchronises, so the check has to as well.
	root := t.TempDir()
	dl := filepath.Join(root, "dl")
	require.NoError(t, os.Mkdir(dl, 0o755))
	data := payload(3, 2500)
	good := newSet(blockSize, fileSpec{Name: "ok.bin", Data: data})
	evil := newSet(blockSize, fileSpec{Name: "../escaped.bin", Data: data})
	require.NoError(t, os.WriteFile(filepath.Join(dl, "ok.bin"), data, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dl, "set.par2"), good.index(), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dl, "set.vol0+1.par2"), good.volume(evil.fileDescPacket(0)), 0o644))

	_, err := par2.Tool{Path: "/nonexistent/par2"}.Verify(context.Background(), dl)

	assert.ErrorIs(t, err, par2.ErrUnsafeSet)
}

func TestASafeHandWrittenSetWorksWithPar2(t *testing.T) {
	// Control for the tests above: the same builder, a harmless name. The
	// obfuscated file is renamed by par2 to the name in the set.
	root, dl := hostileDir(t, "renamed.bin", func(s *testSet) []byte { return s.index() })

	rep, err := toolFor(t).Verify(context.Background(), dl)

	require.NoError(t, err)
	assert.True(t, rep.Clean)
	assert.Equal(t, 1, rep.Renamed)
	assert.FileExists(t, filepath.Join(dl, "renamed.bin"))
	assert.NoFileExists(t, filepath.Join(dl, "obfuscated"))
	assert.FileExists(t, filepath.Join(root, "bystander.txt"))
}

func TestASymlinkedTargetIsRefused(t *testing.T) {
	root, dl := hostileDir(t, "target.bin", func(s *testSet) []byte { return s.index() })
	outside := filepath.Join(root, "bystander.txt")
	require.NoError(t, os.Symlink(outside, filepath.Join(dl, "target.bin")))

	_, err := par2.Tool{Path: "/nonexistent/par2"}.Verify(context.Background(), dl)

	assert.ErrorIs(t, err, par2.ErrUnsafeSet)
}
