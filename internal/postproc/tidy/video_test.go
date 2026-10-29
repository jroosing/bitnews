package tidy_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/postproc/tidy"
)

var (
	mkvHead = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x01, 0x00, 0x00, 0x00}
	mp4Head = []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00")
	aviHead = []byte("RIFF\x00\x10\x00\x00AVI LIST")
)

func write(t *testing.T, path string, head []byte, size int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	b := make([]byte, size)
	copy(b, head)
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

// An obfuscated post without a usable PAR2 set leaves the movie under a
// random name with no extension; media managers only import files with a
// video extension. The largest file is named after the Download, with the
// extension its content says.
func TestNameVideoNamesTheLargestFileAfterTheDownload(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "13184cde52aaba45f8491bbf0db79b93"), mkvHead, 9000)
	write(t, filepath.Join(dir, "21cb82676987e551"), mkvHead, 300) // a sample: left alone
	touch(t, filepath.Join(dir, "release.nfo"))

	r, ok, err := tidy.NameVideo(dir, "The.Housemaid.2025.1080p.BluRay.DDP7.1.x264-ZoroSenpai")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, tidy.Rename{From: "13184cde52aaba45f8491bbf0db79b93", To: "The.Housemaid.2025.1080p.BluRay.DDP7.1.x264-ZoroSenpai.mkv"}, r)
	assert.ElementsMatch(t, []string{"The.Housemaid.2025.1080p.BluRay.DDP7.1.x264-ZoroSenpai.mkv", "21cb82676987e551", "release.nfo"}, names(t, dir))
}

func TestNameVideoKnowsTheCommonContainers(t *testing.T) {
	for ext, head := range map[string][]byte{
		".mkv":  mkvHead,
		".mp4":  mp4Head,
		".avi":  aviHead,
		".wmv":  {0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11},
		".mpg":  {0x00, 0x00, 0x01, 0xba},
		".ts":   tsHead(0),
		".m2ts": tsHead(4),
	} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "abc"), head, 1000)

			r, ok, err := tidy.NameVideo(dir, "Film")

			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, "Film"+ext, r.To)
		})
	}
}

// tsHead is the start of an MPEG transport stream: a sync byte every 188
// bytes, after a 4-byte timecode in an M2TS file.
func tsHead(prefix int) []byte {
	b := make([]byte, 600)
	for i := prefix; i < len(b); i += 188 + prefix {
		b[i] = 0x47
	}
	return b
}

func TestNameVideoLeavesNamedOrUnknownFilesAlone(t *testing.T) {
	for name, files := range map[string]map[string][]byte{
		"already a video":   {"movie.mkv": mkvHead},
		"not a video":       {"0f0f0f0f": []byte("PK\x03\x04 zip")},
		"largest not video": {"aaaa": []byte("just text"), "bbbb": mkvHead},
		"empty":             {},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			size := 2000
			for n, head := range files {
				write(t, filepath.Join(dir, n), head, size)
				size -= 500 // the first is the largest
			}
			before := names(t, dir)

			_, ok, err := tidy.NameVideo(dir, "Film")

			require.NoError(t, err)
			assert.False(t, ok)
			assert.ElementsMatch(t, before, names(t, dir))
		})
	}
}

func TestNameVideoFindsTheMovieInASubfolderAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sub", "x9"), mkvHead, 5000)
	touch(t, filepath.Join(dir, "sub", "Film.mkv"))

	r, ok, err := tidy.NameVideo(dir, "Film")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, tidy.Rename{From: "sub/x9", To: "sub/Film.1.mkv"}, r)
	assert.ElementsMatch(t, []string{"Film.1.mkv", "Film.mkv"}, names(t, filepath.Join(dir, "sub")))
}

func TestNameVideoMakesAHostileDownloadNameSafe(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "x"), mkvHead, 100)

	r, ok, err := tidy.NameVideo(dir, "../../etc/Film")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ".._.._etc_Film.mkv", r.To)
	assert.FileExists(t, filepath.Join(dir, r.To))
}
