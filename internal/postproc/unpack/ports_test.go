package unpack

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

const portTestdata = "testdata"

// archiveDir copies the named archives (paths relative to the unpack
// testdata, e.g. "7z/t0.7z") into a new temporary directory. A "name=path"
// entry renames the copy.
func portArchiveDir(t testing.TB, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		dstName, src := filepath.Base(f), f
		if before, after, ok := strings.Cut(f, "="); ok {
			dstName, src = before, after
		}
		b, err := os.ReadFile(filepath.Join(portTestdata, src))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, dstName), b, 0o644))
	}
	return dir
}

// detect runs the adapter's Detect and requires it to succeed.
func portDetect(t testing.TB, u Unpacker, dir string) []core.ArchiveSet {
	t.Helper()
	sets, err := u.Detect(context.Background(), dir)
	require.NoError(t, err)
	return sets
}

// tree lists the paths below root (directories with a trailing slash).
func portTree(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	}))
	slices.Sort(out)
	return out
}

func TestUnpacker_DetectAndUnpackA7z(t *testing.T) {
	dir := portArchiveDir(t, "7z/t0.7z")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	require.Len(t, sets, 1)
	assert.Equal(t, "t0.7z", sets[0].Name, "a set is named by its first volume")

	dst := filepath.Join(t.TempDir(), "out")
	res, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{DownloadID: "d", Sets: sets})
	require.NoError(t, err)
	assert.Equal(t, core.UnpackResult{Files: 2, Bytes: 8}, res)
	assert.Equal(t, []string{"bar", "foo"}, portTree(t, dst))
	b, err := os.ReadFile(filepath.Join(dst, "foo"))
	require.NoError(t, err)
	assert.Equal(t, "foo\n", string(b))
	assert.FileExists(t, filepath.Join(dir, "t0.7z"), "archives are never modified")
}

func TestUnpacker_StoredRAR(t *testing.T) {
	dir := portArchiveDir(t, "rar/container.rar")
	u := Unpacker{}
	dst := filepath.Join(t.TempDir(), "out")
	res, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: portDetect(t, u, dir)})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Files)
	assert.Equal(t, []string{"container"}, portTree(t, dst))
}

func TestUnpacker_MultiVolume7z(t *testing.T) {
	dir := portArchiveDir(t, "7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.003", "7z/multi.7z.004", "7z/multi.7z.005", "7z/multi.7z.006")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	require.Len(t, sets, 1)
	dst := filepath.Join(t.TempDir(), "out")
	res, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	require.NoError(t, err)
	assert.Equal(t, 10, res.Files)
	assert.Len(t, portTree(t, dst), 10)
}

func TestUnpacker_NothingToDetect(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("not an archive"), 0o644))
	sets, err := Unpacker{}.Detect(context.Background(), dir)
	require.NoError(t, err)
	assert.Empty(t, sets)
}

func TestUnpacker_MissingVolumeIsCorruptAtDetect(t *testing.T) {
	dir := portArchiveDir(t, "7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.004")
	sets, err := Unpacker{}.Detect(context.Background(), dir)
	require.Error(t, err)
	assert.Nil(t, sets)
	assert.ErrorIs(t, err, core.ErrArchiveCorrupt)
	assert.ErrorIs(t, err, ErrMissingVolume)
}

func TestUnpacker_ASetThatIsNoLongerCompleteIsCorruptAtUnpack(t *testing.T) {
	dir := portArchiveDir(t, "7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.003", "7z/multi.7z.004", "7z/multi.7z.005", "7z/multi.7z.006")
	sets := portDetect(t, Unpacker{}, dir)
	require.NoError(t, os.Remove(filepath.Join(dir, "multi.7z.002")))

	dst := filepath.Join(t.TempDir(), "out")
	_, err := Unpacker{}.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	assert.ErrorIs(t, err, core.ErrArchiveCorrupt)
	assert.ErrorIs(t, err, ErrMissingVolume)
	assert.NoDirExists(t, dst)
}

func TestUnpacker_Passwords(t *testing.T) {
	dir := portArchiveDir(t, "7z/t2.7z")
	run := func(t *testing.T, u Unpacker) (string, error) {
		dst := filepath.Join(t.TempDir(), "out")
		_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{DownloadID: "dl1", Sets: portDetect(t, u, dir)})
		return dst, err
	}
	t.Run("right password", func(t *testing.T) {
		var asked core.ID
		dst, err := run(t, Unpacker{Passwords: func(id core.ID) (core.Secret, error) { asked = id; return "password", nil }})
		require.NoError(t, err)
		assert.Equal(t, core.ID("dl1"), asked)
		assert.Equal(t, []string{"bar", "foo"}, portTree(t, dst))
	})
	t.Run("wrong password", func(t *testing.T) {
		dst, err := run(t, Unpacker{Passwords: func(core.ID) (core.Secret, error) { return "nope", nil }})
		assert.ErrorIs(t, err, core.ErrArchivePassword)
		assert.ErrorIs(t, err, ErrPassword)
		assert.NoDirExists(t, dst)
	})
	t.Run("no supplier", func(t *testing.T) {
		_, err := run(t, Unpacker{})
		assert.ErrorIs(t, err, core.ErrArchivePassword)
	})
	t.Run("empty secret means none", func(t *testing.T) {
		_, err := run(t, Unpacker{Passwords: func(core.ID) (core.Secret, error) { return "", nil }})
		assert.ErrorIs(t, err, core.ErrArchivePassword)
	})
	t.Run("supplier error is returned as is", func(t *testing.T) {
		boom := errors.New("vault sealed")
		_, err := run(t, Unpacker{Passwords: func(core.ID) (core.Secret, error) { return "", boom }})
		assert.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, core.ErrArchivePassword)
	})
}

func TestUnpacker_ResourceLimits(t *testing.T) {
	dir := portArchiveDir(t, "7z/t0.7z")
	run := func(u Unpacker) error {
		dst := filepath.Join(t.TempDir(), "out")
		_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: portDetect(t, u, dir)})
		assert.NoDirExists(t, dst)
		return err
	}
	t.Run("too many entries is corrupt", func(t *testing.T) {
		err := run(Unpacker{Options: []Option{WithMaxEntries(1)}})
		assert.ErrorIs(t, err, core.ErrArchiveCorrupt)
		assert.ErrorIs(t, err, ErrTooManyEntries)
	})
	t.Run("too large is corrupt", func(t *testing.T) {
		err := run(Unpacker{Options: []Option{WithMaxBytes(3)}})
		assert.ErrorIs(t, err, core.ErrArchiveCorrupt)
		assert.ErrorIs(t, err, ErrTooLarge)
	})
	t.Run("not enough free space is disk full", func(t *testing.T) {
		err := run(Unpacker{FreeSpace: func(string) (uint64, error) { return 4, nil }})
		assert.ErrorIs(t, err, core.ErrDiskFull)
		assert.NotErrorIs(t, err, core.ErrArchiveCorrupt)
	})
	t.Run("MinFree is kept free", func(t *testing.T) {
		err := run(Unpacker{MinFree: 1000, FreeSpace: func(string) (uint64, error) { return 1004, nil }})
		assert.ErrorIs(t, err, core.ErrDiskFull)
	})
	t.Run("MinFreeFunc is read at each unpack", func(t *testing.T) {
		reserve := uint64(1000)
		u := Unpacker{MinFreeFunc: func() uint64 { return reserve }, FreeSpace: func(string) (uint64, error) { return 1004, nil }}
		assert.ErrorIs(t, run(u), core.ErrDiskFull)

		reserve = 0
		dst := filepath.Join(t.TempDir(), "out")
		_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: portDetect(t, u, dir)})
		assert.NoError(t, err, "a smaller reserve applies without rebuilding the Unpacker")
	})
	t.Run("enough free space", func(t *testing.T) {
		assert.NoError(t, func() error {
			dst := filepath.Join(t.TempDir(), "out")
			u := Unpacker{MinFree: 100, FreeSpace: func(string) (uint64, error) { return 1 << 20, nil }}
			_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: portDetect(t, u, dir)})
			return err
		}())
	})
}

func TestUnpacker_GarbageArchiveIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	full, err := os.ReadFile(filepath.Join(portTestdata, "7z", "t0.7z"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "t.7z"), full[:len(full)-40], 0o644))
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	require.Len(t, sets, 1)
	dst := filepath.Join(t.TempDir(), "out")
	_, err = u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	assert.ErrorIs(t, err, core.ErrArchiveCorrupt)
	assert.NoDirExists(t, dst)
}

func TestUnpacker_ASetThatIsGoneIsCorruptAndNoSetsIsAnError(t *testing.T) {
	dir := portArchiveDir(t, "7z/t0.7z")
	dst := filepath.Join(t.TempDir(), "out")
	_, err := Unpacker{}.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: []core.ArchiveSet{{Name: "other.7z"}}})
	assert.ErrorIs(t, err, core.ErrArchiveCorrupt)
	_, err = Unpacker{}.Unpack(context.Background(), dir, dst, core.UnpackOptions{})
	require.Error(t, err)
	assert.NoDirExists(t, dst)
}

func TestUnpacker_CancelledContext(t *testing.T) {
	dir := portArchiveDir(t, "7z/t0.7z")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := u.Detect(ctx, dir)
	assert.ErrorIs(t, err, context.Canceled)

	dst := filepath.Join(t.TempDir(), "out")
	_, err = u.Unpack(ctx, dir, dst, core.UnpackOptions{Sets: sets})
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, core.ErrArchiveCorrupt)
	assert.NoDirExists(t, dst)
}

func TestUnpacker_SingleSetReplacesAnExistingDestination(t *testing.T) {
	dir := portArchiveDir(t, "7z/t0.7z")
	u := Unpacker{}
	dst := filepath.Join(t.TempDir(), "out")
	require.NoError(t, os.MkdirAll(dst, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dst, "stale"), []byte("x"), 0o644))

	_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: portDetect(t, u, dir)})
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "foo"}, portTree(t, dst))
}

func TestUnpacker_SeveralSetsMergeIntoOneDirectory(t *testing.T) {
	// t0 (bar, foo) and the stored RAR (container) do not collide.
	dir := portArchiveDir(t, "7z/t0.7z", "rar/container.rar")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	require.Len(t, sets, 2)
	base := t.TempDir()
	dst := filepath.Join(base, "out")

	res, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	require.NoError(t, err)
	assert.Equal(t, 3, res.Files)
	assert.EqualValues(t, 8+len("#!/bin/sh\nexit 0\n"), res.Bytes)
	assert.Equal(t, []string{"bar", "container", "foo"}, portTree(t, dst))
	assert.Equal(t, []string{"out"}, portChildren(t, base), "work directories are gone")
}

func TestUnpacker_SeveralSetsKeepCollidingNames(t *testing.T) {
	// t0 and t1 both hold bar and foo; the later set's files are renamed.
	dir := portArchiveDir(t, "a.7z=7z/t0.7z", "b.7z=7z/t1.7z")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	require.Len(t, sets, 2)
	assert.Equal(t, []string{"a.7z", "b.7z"}, []string{sets[0].Name, sets[1].Name})
	base := t.TempDir()
	dst := filepath.Join(base, "out")

	res, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	require.NoError(t, err)
	assert.Equal(t, 4, res.Files)
	assert.Equal(t, []string{"bar", "bar.1", "foo", "foo.1"}, portTree(t, dst))
	assert.Equal(t, []string{"out"}, portChildren(t, base))
}

func TestUnpacker_SeveralSetsKeepFileAndDirectoryCollisions(t *testing.T) {
	// empty.7z holds directories 01..10 (and files 06..10 at the top);
	// multi.7z holds the files 01..10, so 01..05 collide as file versus
	// directory and 06..10 as file versus file.
	dir := portArchiveDir(t, "a.7z=7z/empty.7z",
		"7z/multi.7z.001", "7z/multi.7z.002", "7z/multi.7z.003", "7z/multi.7z.004", "7z/multi.7z.005", "7z/multi.7z.006")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	require.Len(t, sets, 2)
	dst := filepath.Join(t.TempDir(), "out")

	_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	require.NoError(t, err)
	got := portTree(t, dst)
	for _, n := range []string{"01", "02", "03", "04", "05"} {
		assert.Contains(t, got, n+"/", "directory %s kept", n)
		assert.Contains(t, got, n+".1", "file %s renamed", n)
	}
	for _, n := range []string{"06", "07", "08", "09", "10"} {
		assert.Contains(t, got, n)
		assert.Contains(t, got, n+".1")
	}
	assert.Len(t, got, 20)
}

func TestUnpacker_SeveralSetsReplaceStaleLeftovers(t *testing.T) {
	dir := portArchiveDir(t, "a.7z=7z/t0.7z", "b.7z=7z/t1.7z")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	base := t.TempDir()
	dst := filepath.Join(base, "out")
	// A previous, interrupted run left a destination and a work directory.
	require.NoError(t, os.MkdirAll(filepath.Join(dst, "old"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dst+".sets", "0", "junk"), 0o755))

	_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	require.NoError(t, err)
	assert.Equal(t, []string{"bar", "bar.1", "foo", "foo.1"}, portTree(t, dst))
	assert.Equal(t, []string{"out"}, portChildren(t, base))
}

func TestUnpacker_SeveralSetsFailureLeavesNothing(t *testing.T) {
	// The second set needs a password nobody supplies.
	dir := portArchiveDir(t, "a.7z=7z/t0.7z", "b.7z=7z/t2.7z")
	u := Unpacker{}
	sets := portDetect(t, u, dir)
	base := t.TempDir()
	dst := filepath.Join(base, "out")

	_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: sets})
	assert.ErrorIs(t, err, core.ErrArchivePassword)
	assert.Empty(t, portChildren(t, base))
}

func TestMergeInto_MergesDirectoriesRecursively(t *testing.T) {
	root := t.TempDir()
	write := func(p, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	a, b, merged := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "m")
	require.NoError(t, os.Mkdir(merged, 0o755))
	write(filepath.Join(a, "Movie", "movie.mkv"), "A-movie")
	write(filepath.Join(a, "Movie", "Subs", "en.srt"), "A-en")
	write(filepath.Join(b, "Movie", "movie.mkv"), "B-movie")
	write(filepath.Join(b, "Movie", "Subs", "nl.srt"), "B-nl")
	write(filepath.Join(b, "Movie", "readme.txt"), "B-readme")

	require.NoError(t, mergeInto(a, merged))
	require.NoError(t, mergeInto(b, merged))

	assert.Equal(t, []string{
		"Movie/", "Movie/Subs/", "Movie/Subs/en.srt", "Movie/Subs/nl.srt",
		"Movie/movie.1.mkv", "Movie/movie.mkv", "Movie/readme.txt",
	}, portTree(t, merged))
	got, err := os.ReadFile(filepath.Join(merged, "Movie", "movie.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "A-movie", string(got), "the first set keeps its name")
	got, err = os.ReadFile(filepath.Join(merged, "Movie", "movie.1.mkv"))
	require.NoError(t, err)
	assert.Equal(t, "B-movie", string(got))
}

func TestMapError(t *testing.T) {
	boom := errors.New("io exploded")
	tests := []struct {
		name  string
		in    error
		is    []error
		isNot []error
	}{
		{"password", ErrPassword, []error{core.ErrArchivePassword, ErrPassword}, []error{core.ErrArchiveCorrupt}},
		{"unsupported", fmt.Errorf("rar5: %w", ErrUnsupported), []error{core.ErrArchiveUnsupported}, []error{core.ErrArchiveCorrupt}},
		{"corrupt", ErrCorrupt, []error{core.ErrArchiveCorrupt}, []error{core.ErrArchiveUnsupported}},
		{"missing volume", ErrMissingVolume, []error{core.ErrArchiveCorrupt, ErrMissingVolume}, nil},
		{"path violation", ErrPathViolation, []error{core.ErrPathViolation}, []error{core.ErrArchiveCorrupt}},
		{"disk full", ErrDiskFull, []error{core.ErrDiskFull}, []error{core.ErrArchiveCorrupt}},
		{"ENOSPC", fmt.Errorf("write: %w", syscall.ENOSPC), []error{core.ErrDiskFull}, []error{core.ErrArchiveCorrupt}},
		{"bomb", ErrBomb, []error{core.ErrArchiveCorrupt, ErrBomb}, []error{core.ErrDiskFull}},
		{"too large", ErrTooLarge, []error{core.ErrArchiveCorrupt}, []error{core.ErrDiskFull}},
		{"too many entries", ErrTooManyEntries, []error{core.ErrArchiveCorrupt}, []error{core.ErrDiskFull}},
		{"other limit", ErrLimit, []error{core.ErrArchiveCorrupt}, nil},
		{"canceled", context.Canceled, []error{context.Canceled}, []error{core.ErrArchiveCorrupt}},
		{"deadline wrapped", fmt.Errorf("x: %w", context.DeadlineExceeded), []error{context.DeadlineExceeded}, []error{core.ErrArchiveCorrupt}},
		{"exists is internal", ErrExists, []error{ErrExists}, []error{core.ErrArchiveCorrupt, core.ErrPathViolation}},
		{"other", boom, []error{boom}, []error{core.ErrArchiveCorrupt, core.ErrArchivePassword, core.ErrArchiveUnsupported, core.ErrPathViolation, core.ErrDiskFull}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapError(context.Background(), tt.in)
			require.Error(t, got)
			for _, e := range tt.is {
				assert.ErrorIs(t, got, e)
			}
			for _, e := range tt.isNot {
				assert.NotErrorIs(t, got, e)
			}
		})
	}
	assert.NoError(t, MapError(context.Background(), nil))
}

func TestUnpacker_AssertsPort(t *testing.T) {
	var (
		_ core.Unpacker = Unpacker{}
		_ core.Unpacker = &Unpacker{}
	)
}

func portChildren(t testing.TB, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestUnpacker_SevenZipFieldSelectsTheProgram(t *testing.T) {
	dir := portArchiveDir(t, "7z/t0.7z")
	u := Unpacker{SevenZip: filepath.Join(t.TempDir(), "no-such-7zz")}
	dst := filepath.Join(t.TempDir(), "out")

	_, err := u.Unpack(context.Background(), dir, dst, core.UnpackOptions{Sets: portDetect(t, Unpacker{}, dir)})

	require.Error(t, err)
	assert.NotErrorIs(t, err, core.ErrArchiveCorrupt, "a missing program is not the archive's fault")
	assert.NotErrorIs(t, err, core.ErrArchiveUnsupported)
	assert.NoDirExists(t, dst)
}

func TestUnpacker_ASymlinkInTheArchiveIsAPathViolation(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "link", Method: zip.Store}
	h.SetMode(0o777 | os.ModeSymlink)
	w, err := zw.CreateHeader(h)
	require.NoError(t, err)
	_, err = w.Write([]byte("/etc/passwd"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	dir := t.TempDir()
	vol := filepath.Join(dir, "links.zip")
	require.NoError(t, os.WriteFile(vol, buf.Bytes(), 0o644))
	// Detect does not offer a zip, so the set is handed to Unpack directly
	// and its error goes through MapError as the Unpacker's would.
	set := ArchiveSet{Format: Format7z, Name: "links", Volumes: []string{vol}}
	base := t.TempDir()

	_, err = Unpack(context.Background(), set, filepath.Join(base, "out"), WithReplace())

	assert.ErrorIs(t, MapError(context.Background(), err), core.ErrPathViolation)
	assert.Empty(t, portChildren(t, base))
}

func TestUnpacker_PasswordGoesToSevenZipAndNotIntoTheError(t *testing.T) {
	dir := portArchiveDir(t, "7z/t2.7z")
	u := Unpacker{Passwords: func(core.ID) (core.Secret, error) { return "s3cret-wrong", nil }}

	_, err := u.Unpack(context.Background(), dir, filepath.Join(t.TempDir(), "out"), core.UnpackOptions{Sets: portDetect(t, u, dir)})

	require.ErrorIs(t, err, core.ErrArchivePassword)
	assert.NotContains(t, err.Error(), "s3cret")
}
