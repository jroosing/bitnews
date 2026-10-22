package unpack

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// copyFixtures copies the named files from testdata/<sub> into a fresh dir.
func copyFixtures(t testing.TB, sub string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("testdata", sub, n))
		require.NoError(t, err)
		writeFile(t, dir, n, b)
	}
	return dir
}

func detectOne(t testing.TB, dir string) ArchiveSet {
	t.Helper()
	sets, err := Detect(dir)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.NoError(t, sets[0].Err)
	return sets[0]
}

func readFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func TestUnpackExtractsAReal7zArchive(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	dst := filepath.Join(t.TempDir(), "out")

	res, err := Unpack(context.Background(), detectOne(t, dir), dst)

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Files)
	require.EqualValues(t, 8, res.Bytes)
	require.Equal(t, "foo\n", readFile(t, filepath.Join(dst, "foo")))
	require.Equal(t, "bar\n", readFile(t, filepath.Join(dst, "bar")))
	require.Equal(t, []string{"out"}, children(t, filepath.Dir(dst)))
}

func TestUnpackExtractsAMultiVolume7zSet(t *testing.T) {
	dir := copyFixtures(t, "7z", "multi.7z.001", "multi.7z.002", "multi.7z.003", "multi.7z.004", "multi.7z.005", "multi.7z.006")
	set := detectOne(t, dir)
	require.Len(t, set.Volumes, 6)
	dst := filepath.Join(t.TempDir(), "out")

	res, err := Unpack(context.Background(), set, dst)

	require.NoError(t, err)
	require.Equal(t, []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10"}, res.Files)
	fi, err := os.Stat(filepath.Join(dst, "01"))
	require.NoError(t, err)
	require.EqualValues(t, 3572, fi.Size())
}

func TestUnpackFailsOnAMultiVolume7zSetWithAMissingVolume(t *testing.T) {
	dir := copyFixtures(t, "7z", "multi.7z.001", "multi.7z.002", "multi.7z.004")
	set := detectOne2(t, dir)
	dst := filepath.Join(t.TempDir(), "out")

	_, err := Unpack(context.Background(), set, dst)

	require.ErrorIs(t, err, ErrMissingVolume)
	require.NoDirExists(t, dst)
}

func TestUnpackExtractsAHeaderCompressed7z(t *testing.T) {
	dir := copyFixtures(t, "7z", "t1.7z")

	res, err := Unpack(context.Background(), detectOne(t, dir), filepath.Join(t.TempDir(), "out"))

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Files)
}

func TestUnpackExtractsDirectoriesAndEmptyFilesFrom7z(t *testing.T) {
	dir := copyFixtures(t, "7z", "empty.7z")
	dst := filepath.Join(t.TempDir(), "out")

	res, err := Unpack(context.Background(), detectOne(t, dir), dst)

	require.NoError(t, err)
	require.Equal(t, []string{"06", "07", "08", "09", "10"}, res.Files)
	require.Equal(t, []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10"}, listing(t, dst))
	require.DirExists(t, filepath.Join(dst, "03"))
}

func TestUnpackExtractsALZMA2Archive(t *testing.T) {
	dir := copyFixtures(t, "7z", "lzma2.7z")

	res, err := Unpack(context.Background(), detectOne(t, dir), filepath.Join(t.TempDir(), "out"))

	require.NoError(t, err)
	require.Len(t, res.Files, 10)
}

func detectOne2(t testing.TB, dir string) ArchiveSet {
	t.Helper()
	sets, err := Detect(dir)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	return sets[0]
}

func TestUnpack7zPasswords(t *testing.T) {
	cases := map[string]string{
		"encrypted headers":                    "t2.7z",
		"encrypted headers, compressed":        "t3.7z",
		"plain headers, encrypted compressed":  "t4.7z",
		"plain headers, encrypted stored data": "t5.7z",
	}
	for label, file := range cases {
		t.Run(label, func(t *testing.T) {
			dir := copyFixtures(t, "7z", file)
			set := detectOne(t, dir)

			t.Run("right password", func(t *testing.T) {
				dst := filepath.Join(t.TempDir(), "out")
				res, err := Unpack(context.Background(), set, dst, WithPassword("password"))
				require.NoError(t, err)
				require.Equal(t, []string{"bar", "foo"}, res.Files)
				require.Equal(t, "foo\n", readFile(t, filepath.Join(dst, "foo")))
			})
			t.Run("a core.Secret works too", func(t *testing.T) {
				_, err := Unpack(context.Background(), set, filepath.Join(t.TempDir(), "out"), WithPassword(core.Secret("password")))
				require.NoError(t, err)
			})
			t.Run("wrong password", func(t *testing.T) {
				dst := filepath.Join(t.TempDir(), "out")
				_, err := Unpack(context.Background(), set, dst, WithPassword("notpassword"))
				require.ErrorIs(t, err, ErrPassword)
				require.NoDirExists(t, dst)
				require.NoDirExists(t, dst+".tmp")
			})
			t.Run("no password", func(t *testing.T) {
				dst := filepath.Join(t.TempDir(), "out")
				_, err := Unpack(context.Background(), set, dst)
				require.ErrorIs(t, err, ErrPassword)
				require.NoDirExists(t, dst)
				require.NoDirExists(t, dst+".tmp")
			})
		})
	}
}

func TestUnpackAPasswordOnAnUnencrypted7zIsIgnored(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")

	_, err := Unpack(context.Background(), detectOne(t, dir), filepath.Join(t.TempDir(), "out"), WithPassword("whatever"))

	require.NoError(t, err)
}

func TestUnpackTruncatedAndGarbage7zAreCorrupt(t *testing.T) {
	full, err := os.ReadFile(filepath.Join("testdata", "7z", "t0.7z"))
	require.NoError(t, err)
	cases := map[string][]byte{
		"magic only":             magic7z,
		"magic and zeros":        append(append([]byte{}, magic7z...), make([]byte, 100)...),
		"cut in the header":      full[:len(full)-10],
		"cut after signature":    full[:40],
		"flipped header byte":    flip(full, len(full)-3),
		"flipped signature byte": flip(full, 10),
	}
	for label, content := range cases {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "bad.7z", content)
			dst := filepath.Join(t.TempDir(), "out")

			_, err := Unpack(context.Background(), detectOne(t, dir), dst)

			require.ErrorIs(t, err, ErrCorrupt)
			require.NoDirExists(t, dst)
			require.NoDirExists(t, dst+".tmp")
		})
	}
}

func flip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 0xff
	return c
}

func TestUnpackRefusesASetWithAnError(t *testing.T) {
	set := ArchiveSet{Format: Format7z, Name: "x", Volumes: []string{"x.7z.001"}, Err: ErrMissingVolume}

	_, err := Unpack(context.Background(), set, filepath.Join(t.TempDir(), "out"))

	require.ErrorIs(t, err, ErrMissingVolume)
}

func TestUnpackRefusesAVolumeThatBecameASymlink(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	set := detectOne(t, dir)
	real := filepath.Join(t.TempDir(), "real.7z")
	require.NoError(t, os.Rename(set.Volumes[0], real))
	require.NoError(t, os.Symlink(real, set.Volumes[0]))
	dst := filepath.Join(t.TempDir(), "out")

	_, err := Unpack(context.Background(), set, dst)

	require.ErrorIs(t, err, ErrCorrupt)
	require.NoDirExists(t, dst)
}

func TestKindOf7zEntries(t *testing.T) {
	cases := map[fs.FileMode]entryKind{
		0o644:                                   kindFile,
		fs.ModeDir | 0o755:                      kindDir,
		fs.ModeSymlink | 0o777:                  kindSymlink,
		fs.ModeDevice | 0o600:                   kindDevice,
		fs.ModeDevice | fs.ModeCharDevice | 0o6: kindDevice,
		fs.ModeNamedPipe | 0o644:                kindFIFO,
		fs.ModeSocket | 0o644:                   kindOther,
		fs.ModeIrregular:                        kindOther,
	}
	for mode, want := range cases {
		require.Equal(t, want, kind7z(mode), mode.String())
	}
}
