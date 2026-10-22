package unpack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// builders runs a test against both RAR generations. The archives come from
// rar_builder_test.go (stored files only); 7zz is the reader under test.
func builders() map[string]func([]rarEntry, rarSpec) [][]byte {
	return map[string]func([]rarEntry, rarSpec) [][]byte{"RAR4": buildRAR4, "RAR5": buildRAR5}
}

func TestUnpackExtractsARealRAR4Archive(t *testing.T) {
	dir := copyFixtures(t, "rar", "container.rar")
	o := newOut(t)

	res, err := unpackSet(t, detectOne(t, dir), o.dst)

	require.NoError(t, err)
	require.Equal(t, []string{"container"}, res.Files)
	require.Equal(t, "#!/bin/sh\nexit 0\n", readFile(t, filepath.Join(o.dst, "container")))
	fi, err := os.Stat(filepath.Join(o.dst, "container"))
	require.NoError(t, err)
	require.Zero(t, fi.Mode().Perm()&0o111, "the archive's executable bit is dropped")
}

func TestBuiltRARSingleVolumeExtracts(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			vols := build([]rarEntry{
				{name: "docs", dir: true},
				{name: "docs/readme.txt", data: []byte("hello " + gen)},
				{name: "empty", data: nil},
			}, rarSpec{})
			require.Len(t, vols, 1)
			dir := t.TempDir()
			writeFile(t, dir, "built.rar", vols[0])
			o := newOut(t)

			res, err := unpackSet(t, detectOne(t, dir), o.dst)

			require.NoError(t, err)
			require.Equal(t, []string{"docs/readme.txt", "empty"}, res.Files)
			require.Equal(t, "hello "+gen, readFile(t, filepath.Join(o.dst, "docs", "readme.txt")))
		})
	}
}

// writeVolumes writes volumes under the names and returns the directory.
func writeVolumes(t testing.TB, names []string, vols [][]byte) string {
	t.Helper()
	require.Len(t, vols, len(names))
	dir := t.TempDir()
	for i, n := range names {
		writeFile(t, dir, n, vols[i])
	}
	return dir
}

func unpackDir(t testing.TB, dir string, opts ...Option) (string, Result, error) {
	t.Helper()
	sets, err := Detect(dir)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	dst := filepath.Join(t.TempDir(), "out")
	res, err := unpackSet(t, sets[0], dst, opts...)
	return dst, res, err
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func TestUnpackMultiVolumeRARSets(t *testing.T) {
	big := payload(5000, 1)
	small := []byte("tail file")
	entries := []rarEntry{
		{name: "big.bin", data: big},
		{name: "small.txt", data: small},
	}
	cases := map[string]struct {
		build func() [][]byte
		names []string
	}{
		"RAR4 new-style": {
			build: func() [][]byte { return buildRAR4(entries, rarSpec{perVolume: 2000, newNaming: true}) },
			names: []string{"rel.part1.rar", "rel.part2.rar", "rel.part3.rar"},
		},
		"RAR4 old-style": {
			build: func() [][]byte { return buildRAR4(entries, rarSpec{perVolume: 2000}) },
			names: []string{"rel.rar", "rel.r00", "rel.r01"},
		},
		"RAR5": {
			build: func() [][]byte { return buildRAR5(entries, rarSpec{perVolume: 2000}) },
			names: []string{"rel.part01.rar", "rel.part02.rar", "rel.part03.rar"},
		},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			dir := writeVolumes(t, c.names, c.build())

			dst, res, err := unpackDir(t, dir)

			require.NoError(t, err)
			require.Equal(t, []string{"big.bin", "small.txt"}, res.Files)
			require.Equal(t, string(big), readFile(t, filepath.Join(dst, "big.bin")))
			require.Equal(t, string(small), readFile(t, filepath.Join(dst, "small.txt")))
		})
	}
}

func TestUnpackMultiVolumeRARWithAMissingLastVolumeIsCorrupt(t *testing.T) {
	vols := buildRAR5([]rarEntry{{name: "big.bin", data: payload(5000, 2)}}, rarSpec{perVolume: 2000})
	require.Len(t, vols, 3)
	dir := writeVolumes(t, []string{"rel.part1.rar", "rel.part2.rar"}, vols[:2])

	dst, _, err := unpackDir(t, dir)

	require.ErrorIs(t, err, ErrCorrupt)
	require.NoDirExists(t, dst)
	require.NoDirExists(t, dst+".tmp")
}

func TestUnpackMultiVolumeRARWithAMissingMiddleVolumeIsRefused(t *testing.T) {
	vols := buildRAR5([]rarEntry{{name: "big.bin", data: payload(5000, 2)}}, rarSpec{perVolume: 2000})
	dir := writeVolumes(t, []string{"rel.part1.rar", "rel.part3.rar"}, [][]byte{vols[0], vols[2]})
	sets, err := Detect(dir)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	dst := filepath.Join(t.TempDir(), "out")

	_, err = unpackSet(t, sets[0], dst)

	require.ErrorIs(t, err, ErrMissingVolume)
	require.NoDirExists(t, dst)
}

func TestUnpackEncryptedRAR(t *testing.T) {
	secret := []byte("the secret payload, long enough to span more than one AES block")
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			dir := writeVolumes(t, []string{"enc.rar"}, build([]rarEntry{{name: "s.txt", data: secret, password: "hunter2"}}, rarSpec{}))

			t.Run("right password", func(t *testing.T) {
				dst, res, err := unpackDir(t, dir, WithPassword("hunter2"))
				require.NoError(t, err)
				require.Equal(t, []string{"s.txt"}, res.Files)
				require.Equal(t, string(secret), readFile(t, filepath.Join(dst, "s.txt")))
			})
			t.Run("core.Secret", func(t *testing.T) {
				_, _, err := unpackDir(t, dir, WithPassword(core.Secret("hunter2")))
				require.NoError(t, err)
			})
			t.Run("wrong password", func(t *testing.T) {
				dst, _, err := unpackDir(t, dir, WithPassword("hunter3"))
				require.ErrorIs(t, err, ErrPassword)
				require.NoDirExists(t, dst)
				require.NoDirExists(t, dst+".tmp")
			})
			t.Run("no password", func(t *testing.T) {
				dst, _, err := unpackDir(t, dir)
				require.ErrorIs(t, err, ErrPassword)
				require.NoDirExists(t, dst)
				require.NoDirExists(t, dst+".tmp")
			})
		})
	}
}
