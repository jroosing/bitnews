package unpack

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectFindsASingleRAR5ArchiveByMagic(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "movie.rar", fakeVolume(magicRAR5))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.Equal(t, FormatRAR, sets[0].Format)
	require.Equal(t, "movie", sets[0].Name)
	require.Equal(t, []string{p}, sets[0].Volumes)
	require.NoError(t, sets[0].Err)
}

func TestDetectIgnoresFilesWithoutArchiveMagicWhateverTheExtension(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "fake.rar", []byte("this is not a rar"))
	writeFile(t, dir, "fake.7z", nil)
	writeFile(t, dir, "notes.txt", []byte("hello"))
	writeFile(t, dir, "short.rar", []byte("Rar!"))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Empty(t, sets)
}

func TestDetectFindsAnArchiveWithAnObfuscatedName(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a1b2c3d4", fakeVolume(magicRAR4))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.Equal(t, FormatRAR, sets[0].Format)
	require.Equal(t, []string{p}, sets[0].Volumes)
}

func TestDetectGroupsNewStyleRARVolumesInOrder(t *testing.T) {
	dir := t.TempDir()
	var want []string
	for i := 1; i <= 12; i++ {
		want = append(want, filepath.Join(dir, fmt.Sprintf("show.part%02d.rar", i)))
	}
	// Created in reverse, with a mix of RAR4 and RAR5 headers.
	for i, w := range slices.Backward(want) {
		magic := magicRAR5
		if i%2 == 0 {
			magic = magicRAR4
		}
		require.NoError(t, os.WriteFile(w, fakeVolume(magic), 0o600))
	}

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.NoError(t, sets[0].Err)
	require.Equal(t, "show", sets[0].Name)
	require.Equal(t, want, sets[0].Volumes)
}

func TestDetectGroupsOldStyleRARVolumes(t *testing.T) {
	dir := t.TempDir()
	names := []string{"old.rar", "old.r00", "old.r01", "old.r02"}
	var want []string
	for _, n := range names {
		want = append(want, writeFile(t, dir, n, fakeVolume(magicRAR4)))
	}

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.NoError(t, sets[0].Err)
	require.Equal(t, "old", sets[0].Name)
	require.Equal(t, want, sets[0].Volumes)
}

func TestDetectOldStyleVolumesRollOverFromR99ToS00(t *testing.T) {
	dir := t.TempDir()
	want := []string{writeFile(t, dir, "big.rar", fakeVolume(magicRAR4))}
	for i := range 100 {
		want = append(want, writeFile(t, dir, fmt.Sprintf("big.r%02d", i), fakeVolume(magicRAR4)))
	}
	want = append(want, writeFile(t, dir, "big.s00", fakeVolume(magicRAR4)))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.NoError(t, sets[0].Err)
	require.Equal(t, want, sets[0].Volumes)
}

func TestDetectFindsSingleAndMultiVolume7z(t *testing.T) {
	dir := t.TempDir()
	single := writeFile(t, dir, "one.7z", fakeVolume(magic7z))
	v1 := writeFile(t, dir, "multi.7z.001", fakeVolume(magic7z))
	v2 := writeFile(t, dir, "multi.7z.002", []byte("continuation bytes without any magic"))
	v3 := writeFile(t, dir, "multi.7z.003", []byte("more"))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 2)
	require.Equal(t, ArchiveSet{Format: Format7z, Name: "multi", Volumes: []string{v1, v2, v3}}, sets[0])
	require.Equal(t, ArchiveSet{Format: Format7z, Name: "one", Volumes: []string{single}}, sets[1])
}

func TestDetectIgnoresOrphan7zVolumesWithoutAMagicFirstVolume(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lost.7z.002", []byte("continuation"))
	writeFile(t, dir, "fake.7z.001", []byte("not a 7z first volume"))
	writeFile(t, dir, "fake.7z.002", []byte("continuation"))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Empty(t, sets)
}

func TestDetectReportsAMissingMiddleVolumeOnTheSet(t *testing.T) {
	cases := map[string][]string{
		"new-style rar": {"a.part01.rar", "a.part02.rar", "a.part04.rar"},
		"old-style rar": {"a.rar", "a.r00", "a.r02"},
		"7z":            {"a.7z.001", "a.7z.002", "a.7z.004"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for i, f := range files {
				content := fakeVolume(magicRAR5)
				if strings.Contains(f, ".7z.") {
					content = fakeVolume(magic7z)
					if i > 0 {
						content = []byte("continuation")
					}
				}
				writeFile(t, dir, f, content)
			}

			sets, err := Detect(dir)

			require.NoError(t, err)
			require.Len(t, sets, 1)
			require.ErrorIs(t, sets[0].Err, ErrMissingVolume)
			require.Len(t, sets[0].Volumes, 3, "the volumes found are still listed, not guessed")
		})
	}
}

func TestDetectReportsAMissingFirstVolume(t *testing.T) {
	cases := map[string][]string{
		"new-style rar": {"a.part02.rar", "a.part03.rar"},
		"old-style rar": {"a.r00", "a.r01"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range files {
				writeFile(t, dir, f, fakeVolume(magicRAR4))
			}

			sets, err := Detect(dir)

			require.NoError(t, err)
			require.Len(t, sets, 1)
			require.ErrorIs(t, sets[0].Err, ErrMissingVolume)
		})
	}
}

func TestDetectReportsTwoFilesForTheSameVolumeNumber(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.part1.rar", fakeVolume(magicRAR5))
	writeFile(t, dir, "a.part01.rar", fakeVolume(magicRAR5))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.ErrorIs(t, sets[0].Err, ErrMissingVolume)
}

func TestDetectMatchesExtensionsCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "SHOW.PART1.RAR", fakeVolume(magicRAR5))
	b := writeFile(t, dir, "SHOW.PART2.RAR", fakeVolume(magicRAR5))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.NoError(t, sets[0].Err)
	require.Equal(t, []string{a, b}, sets[0].Volumes)
}

func TestDetectIgnoresSymlinksDirectoriesAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := writeFile(t, outside, "real.rar", fakeVolume(magicRAR5))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "link.rar")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "dir.rar"), 0o755))
	writeFile(t, filepath.Join(dir, "dir.rar"), "inner.rar", fakeVolume(magicRAR5))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Empty(t, sets, "only regular files count, and Detect does not recurse")
}

func TestDetectTreatsASymlinkedVolumeAsMissing(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeFile(t, dir, "a.part1.rar", fakeVolume(magicRAR5))
	target := writeFile(t, outside, "real", fakeVolume(magicRAR5))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "a.part2.rar")))
	writeFile(t, dir, "a.part3.rar", fakeVolume(magicRAR5))

	sets, err := Detect(dir)

	require.NoError(t, err)
	require.Len(t, sets, 1)
	require.ErrorIs(t, sets[0].Err, ErrMissingVolume)
}

func TestDetectHandlesThousandsOfFilesDeterministically(t *testing.T) {
	dir := t.TempDir()
	for i := range 3000 {
		writeFile(t, dir, fmt.Sprintf("junk-%04d.bin", i), []byte("x"))
	}
	for i := range 50 {
		writeFile(t, dir, fmt.Sprintf("set%02d.rar", i), fakeVolume(magicRAR5))
	}

	first, err := Detect(dir)
	require.NoError(t, err)
	second, err := Detect(dir)
	require.NoError(t, err)

	require.Len(t, first, 50)
	require.Equal(t, first, second)
	for i := 1; i < len(first); i++ {
		require.Less(t, first[i-1].Name, first[i].Name)
	}
}

func TestDetectKeepsATruncatedFileWithAValidMagic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cut.rar", magicRAR5[:6])   // shorter than any full magic
	q := writeFile(t, dir, "cut2.rar", magicRAR5) // magic and nothing else

	sets, err := Detect(dir)

	require.NoError(t, err)
	// Detect only classifies; Unpack reports the corruption.
	require.Len(t, sets, 1)
	require.Equal(t, []string{q}, sets[0].Volumes)
}

func TestDetectFailsOnAMissingDirectory(t *testing.T) {
	_, err := Detect(filepath.Join(t.TempDir(), "nope"))
	require.Error(t, err)
}
