package unpack

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func innerRAR(t testing.TB, name string, entries ...rarEntry) rarEntry {
	t.Helper()
	return rarEntry{name: name, data: buildRAR4(entries, rarSpec{})[0]}
}

func TestOneLevelOfNestedArchivesIsUnpackedInPlace(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			inner := innerRAR(t, "inner.rar", rarEntry{name: "movie.mkv", data: []byte("the movie")}, rarEntry{name: "sub", dir: true}, rarEntry{name: "sub/x.nfo", data: []byte("nfo")})
			dir := writeVolumes(t, []string{"outer.rar"}, build([]rarEntry{
				{name: "readme.txt", data: []byte("read me")},
				inner,
			}, rarSpec{}))

			dst, res, err := unpackDir(t, dir)

			require.NoError(t, err)
			require.True(t, res.Nested)
			require.Equal(t, []string{"movie.mkv", "readme.txt", "sub/x.nfo"}, res.Files)
			require.EqualValues(t, len("the movie")+len("read me")+len("nfo"), res.Bytes)
			require.Equal(t, []string{"movie.mkv", "readme.txt", "sub", "sub/x.nfo"}, listing(t, dst), "the inner archive and the scratch dir are gone")
			require.Equal(t, "the movie", readFile(t, filepath.Join(dst, "movie.mkv")))
		})
	}
}

func TestANestedArchiveInASubdirectoryIsUnpackedThere(t *testing.T) {
	inner := innerRAR(t, "disc1/inner.rar", rarEntry{name: "a.bin", data: []byte("A")})
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{{name: "disc1", dir: true}, inner}, rarSpec{}))

	dst, res, err := unpackDir(t, dir)

	require.NoError(t, err)
	require.Equal(t, []string{"disc1/a.bin"}, res.Files)
	require.Equal(t, []string{"disc1", "disc1/a.bin"}, listing(t, dst))
}

func TestANestedMultiVolumeSetIsUnpacked(t *testing.T) {
	content := payload(3000, 9)
	vols := buildRAR5([]rarEntry{{name: "big.bin", data: content}}, rarSpec{perVolume: 1200})
	require.Len(t, vols, 3)
	outer := buildRAR4([]rarEntry{
		{name: "inner.part1.rar", data: vols[0]},
		{name: "inner.part2.rar", data: vols[1]},
		{name: "inner.part3.rar", data: vols[2]},
	}, rarSpec{})
	dir := writeVolumes(t, []string{"outer.rar"}, outer)

	dst, res, err := unpackDir(t, dir)

	require.NoError(t, err)
	require.Equal(t, []string{"big.bin"}, res.Files)
	require.Equal(t, string(content), readFile(t, filepath.Join(dst, "big.bin")))
}

func TestANested7zInsideARARIsUnpacked(t *testing.T) {
	seven, err := os.ReadFile(filepath.Join("testdata", "7z", "t0.7z"))
	require.NoError(t, err)
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{{name: "inner.7z", data: seven}}, rarSpec{}))

	_, res, err := unpackDir(t, dir)

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Files)
}

func TestTheSecondNestingLevelIsLeftAsIs(t *testing.T) {
	deep := innerRAR(t, "deep.rar", rarEntry{name: "deep.txt", data: []byte("deep")})
	middle := innerRAR(t, "middle.rar", rarEntry{name: "middle.txt", data: []byte("middle")}, deep)
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{middle}, rarSpec{}))

	dst, res, err := unpackDir(t, dir)

	require.NoError(t, err)
	require.True(t, res.Nested)
	require.Equal(t, []string{"deep.rar", "middle.txt"}, res.Files)
	require.Equal(t, deep.data, []byte(readFile(t, filepath.Join(dst, "deep.rar"))))
}

func TestANestedArchiveNeverOverwritesWhatTheOuterOneProduced(t *testing.T) {
	inner := innerRAR(t, "inner.rar", rarEntry{name: "same.txt", data: []byte("from inner")})
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{{name: "same.txt", data: []byte("from outer")}, inner}, rarSpec{}))

	dst, res, err := unpackDir(t, dir)

	require.NoError(t, err)
	require.Len(t, res.Files, 2)
	require.Equal(t, "from outer", readFile(t, filepath.Join(dst, "same.txt")))
	require.Equal(t, "from inner", readFile(t, filepath.Join(dst, "same.1.txt")))
}

func TestHostileNamesInANestedArchiveFailTheWholeUnpack(t *testing.T) {
	inner := innerRAR(t, "inner.rar", rarEntry{name: "fine.txt", data: []byte("ok")}, rarEntry{name: "../../escape.txt", data: []byte("evil")})
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{{name: "outer.txt", data: []byte("x")}, inner}, rarSpec{}))
	out := t.TempDir()
	sets, err := Detect(dir)
	require.NoError(t, err)

	_, err = Unpack(t.Context(), sets[0], filepath.Join(out, "out"), WithLogger(nil))

	require.ErrorIs(t, err, ErrPathViolation)
	require.Empty(t, listing(t, out))
}

func TestACorruptNestedArchiveFailsTheUnpack(t *testing.T) {
	bad := append(append([]byte{}, magicRAR5...), bytes.Repeat([]byte{0xAB}, 40)...)
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{{name: "inner.rar", data: bad}}, rarSpec{}))

	dst, _, err := unpackDir(t, dir)

	require.ErrorIs(t, err, ErrCorrupt)
	require.NoDirExists(t, dst)
	require.NoDirExists(t, dst+".tmp")
}

func TestANestedSetWithAMissingVolumeFailsTheUnpack(t *testing.T) {
	vols := buildRAR5([]rarEntry{{name: "big.bin", data: payload(3000, 4)}}, rarSpec{perVolume: 1200})
	outer := buildRAR4([]rarEntry{
		{name: "inner.part1.rar", data: vols[0]},
		{name: "inner.part3.rar", data: vols[2]},
	}, rarSpec{})
	dir := writeVolumes(t, []string{"outer.rar"}, outer)

	dst, _, err := unpackDir(t, dir)

	require.ErrorIs(t, err, ErrMissingVolume)
	require.NoDirExists(t, dst)
}

func TestNestedArchivesUseTheSamePassword(t *testing.T) {
	inner := innerRAR(t, "inner.rar", rarEntry{name: "s.txt", data: []byte("secret"), password: "pw"})
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR4([]rarEntry{inner}, rarSpec{}))

	dst, res, err := unpackDir(t, dir, WithPassword("pw"))
	require.NoError(t, err)
	require.Equal(t, []string{"s.txt"}, res.Files)
	require.Equal(t, "secret", readFile(t, filepath.Join(dst, "s.txt")))

	dst, _, err = unpackDir(t, dir, WithPassword("other"))
	require.ErrorIs(t, err, ErrPassword)
	require.NoDirExists(t, dst)
}

func TestTheByteCapCoversOuterAndNestedArchivesTogether(t *testing.T) {
	inner := innerRAR(t, "inner.rar", rarEntry{name: "a.bin", data: payload(600, 1)})
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{{name: "b.bin", data: payload(600, 2)}, inner}, rarSpec{}))

	t.Run("under the cap", func(t *testing.T) {
		_, _, err := unpackDir(t, dir, WithMaxBytes(4000))
		require.NoError(t, err)
	})
	t.Run("over the cap only with the inner archive", func(t *testing.T) {
		// The outer archive holds 600 + len(inner) bytes; the inner adds 600.
		_, _, err := unpackDir(t, dir, WithMaxBytes(int64(600+len(inner.data)+300)))
		require.ErrorIs(t, err, ErrTooLarge)
	})
}

func TestFilesWithoutArchiveMagicInTheOutputAreNotTouched(t *testing.T) {
	dir := writeVolumes(t, []string{"outer.rar"}, buildRAR5([]rarEntry{
		{name: "fake.rar", data: []byte("not an archive")},
		{name: "movie.mkv", data: []byte("video")},
	}, rarSpec{}))

	dst, res, err := unpackDir(t, dir)

	require.NoError(t, err)
	require.False(t, res.Nested)
	require.Equal(t, []string{"fake.rar", "movie.mkv"}, res.Files)
	require.Equal(t, "not an archive", readFile(t, filepath.Join(dst, "fake.rar")))
}
