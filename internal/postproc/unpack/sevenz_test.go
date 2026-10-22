package unpack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestUnpackExtractsAReal7zArchive(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	o := newOut(t)

	res, err := unpackSet(t, detectOne(t, dir), o.dst)

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Files)
	require.EqualValues(t, 8, res.Bytes)
	require.Equal(t, "foo\n", readFile(t, filepath.Join(o.dst, "foo")))
	require.Equal(t, "bar\n", readFile(t, filepath.Join(o.dst, "bar")))
	require.Equal(t, []string{"out"}, children(t, o.parent))
}

func TestUnpackExtractsAMultiVolume7zSet(t *testing.T) {
	dir := copyFixtures(t, "7z", "multi.7z.001", "multi.7z.002", "multi.7z.003", "multi.7z.004", "multi.7z.005", "multi.7z.006")
	set := detectOne(t, dir)
	require.Len(t, set.Volumes, 6)
	o := newOut(t)

	res, err := unpackSet(t, set, o.dst)

	require.NoError(t, err)
	require.Len(t, res.Files, 10)
}

func TestUnpackFailsOnAMultiVolume7zSetWithAMissingVolume(t *testing.T) {
	dir := copyFixtures(t, "7z", "multi.7z.001", "multi.7z.002", "multi.7z.004")
	sets, err := Detect(dir)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	o := newOut(t)

	_, err = unpackSet(t, sets[0], o.dst)

	require.ErrorIs(t, err, ErrMissingVolume)
	require.Empty(t, children(t, o.parent))
}

func TestUnpackAMultiVolume7zSetWhoseTailWasLostIsCorrupt(t *testing.T) {
	// Detect cannot know that .003 is missing when it is the last one.
	dir := copyFixtures(t, "7z", "multi.7z.001", "multi.7z.002", "multi.7z.003")
	o := newOut(t)

	_, err := unpackSet(t, detectOne(t, dir), o.dst)

	require.ErrorIs(t, err, ErrCorrupt)
	require.Empty(t, children(t, o.parent))
}

func TestUnpackExtractsAHeaderCompressed7z(t *testing.T) {
	dir := copyFixtures(t, "7z", "t1.7z")
	o := newOut(t)

	res, err := unpackSet(t, detectOne(t, dir), o.dst)

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Files)
}

func TestUnpackExtractsDirectoriesAndEmptyFilesFrom7z(t *testing.T) {
	dir := copyFixtures(t, "7z", "empty.7z")
	o := newOut(t)

	res, err := unpackSet(t, detectOne(t, dir), o.dst)

	require.NoError(t, err)
	require.NotEmpty(t, res.Files)
	require.Contains(t, listing(t, o.dst), "01")
}

func TestUnpackExtractsALZMA2Archive(t *testing.T) {
	dir := copyFixtures(t, "7z", "lzma2.7z")
	o := newOut(t)

	res, err := unpackSet(t, detectOne(t, dir), o.dst)

	require.NoError(t, err)
	require.NotEmpty(t, res.Files)
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
				o := newOut(t)
				res, err := unpackSet(t, set, o.dst, WithPassword("password"))
				require.NoError(t, err)
				require.Equal(t, []string{"bar", "foo"}, res.Files)
				require.Equal(t, "foo\n", readFile(t, filepath.Join(o.dst, "foo")))
			})
			t.Run("a core.Secret works too", func(t *testing.T) {
				_, err := unpackSet(t, set, newOut(t).dst, WithPassword(core.Secret("password")))
				require.NoError(t, err)
			})
			t.Run("wrong password", func(t *testing.T) {
				o := newOut(t)
				_, err := unpackSet(t, set, o.dst, WithPassword("notpassword"))
				require.ErrorIs(t, err, ErrPassword)
				require.Empty(t, children(t, o.parent))
			})
			t.Run("no password", func(t *testing.T) {
				o := newOut(t)
				_, err := unpackSet(t, set, o.dst)
				require.ErrorIs(t, err, ErrPassword)
				require.Empty(t, children(t, o.parent))
			})
		})
	}
}

func TestUnpackAPasswordOnAnUnencrypted7zIsIgnored(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")

	_, err := unpackSet(t, detectOne(t, dir), newOut(t).dst, WithPassword("whatever"))

	require.NoError(t, err)
}

func TestUnpackPasswordsWithAwkwardCharacters(t *testing.T) {
	for _, pw := range []string{"pässwörd 日本語", "with spaces  and  trailing ", `-pquote"s'&;$(x)`, "-ppw", "@list"} {
		t.Run(pw, func(t *testing.T) {
			src := t.TempDir()
			writeFile(t, src, "secret.txt", []byte("s3cret"))
			arch := t.TempDir()
			// 7zz reads the password to create the archive from stdin too.
			createEncrypted7z(t, arch, src, pw)
			o := newOut(t)

			res, err := unpackSet(t, detectOne(t, arch), o.dst, WithPassword(pw))

			require.NoError(t, err)
			require.Equal(t, []string{"secret.txt"}, res.Files)
			require.Equal(t, "s3cret", readFile(t, filepath.Join(o.dst, "secret.txt")))
		})
	}
}

func TestAPasswordWithALineBreakIsRefused(t *testing.T) {
	dir := copyFixtures(t, "7z", "t3.7z")

	_, err := unpackSet(t, detectOne(t, dir), newOut(t).dst, WithPassword("pass\nword"))

	require.ErrorIs(t, err, ErrPassword)
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
			o := newOut(t)

			_, err := unpackSet(t, oneSet(filepath.Join(dir, "bad.7z")), o.dst)

			require.ErrorIs(t, err, ErrCorrupt)
			require.Empty(t, children(t, o.parent))
		})
	}
}

func TestUnpackRefusesASetWithAnError(t *testing.T) {
	set := ArchiveSet{Format: Format7z, Name: "x", Volumes: []string{"x.7z.001"}, Err: ErrMissingVolume}

	_, err := unpackSet(t, set, newOut(t).dst)

	require.ErrorIs(t, err, ErrMissingVolume)
}

func TestUnpackRefusesASetWithoutVolumes(t *testing.T) {
	_, err := unpackSet(t, ArchiveSet{Format: Format7z, Name: "x"}, newOut(t).dst)

	require.ErrorIs(t, err, ErrCorrupt)
}

func TestUnpackRefusesAVolumeThatBecameASymlink(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	set := detectOne(t, dir)
	real := filepath.Join(t.TempDir(), "real.7z")
	require.NoError(t, os.Rename(set.Volumes[0], real))
	require.NoError(t, os.Symlink(real, set.Volumes[0]))
	o := newOut(t)

	_, err := unpackSet(t, set, o.dst)

	require.ErrorIs(t, err, ErrCorrupt)
	require.Empty(t, children(t, o.parent))
}

func TestUnpackWorksForAnArchiveNamedLikeASwitch(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("testdata", "7z", "t0.7z"))
	require.NoError(t, err)
	path := writeFile(t, dir, "-so.7z", b)
	o := newOut(t)

	res, err := unpackSet(t, oneSet(path), o.dst)

	require.NoError(t, err)
	require.Equal(t, []string{"bar", "foo"}, res.Files)
}

func TestUnpackFailsWithoutThe7zzProgram(t *testing.T) {
	dir := copyFixtures(t, "7z", "t0.7z")
	o := newOut(t)

	_, err := Unpack(t.Context(), detectOne(t, dir), o.dst, WithSevenZip(filepath.Join(t.TempDir(), "no-such-7zz")), WithLogger(nil))

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCorrupt)
	require.Empty(t, children(t, o.parent))
}
