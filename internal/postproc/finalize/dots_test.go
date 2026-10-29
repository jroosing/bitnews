package finalize_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/fsutil"
	"github.com/jroosing/bitnews/internal/postproc/finalize"
)

// logged returns a Finisher option that sends the Finisher's log to the
// returned buffer.
func logged() (*bytes.Buffer, func() slog.Handler) {
	var buf bytes.Buffer
	return &buf, func() slog.Handler { return slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}) }
}

func TestUsenetRenamesUnderscoresToDotsWhenTheOptionIsOn(t *testing.T) {
	e := newEnv(t)
	e.settings.PostProcess.UnderscoresToDots = true
	dir := e.usenetDir("Show")
	writeFiles(t, dir, map[string]string{
		"my_show_s01e01.mkv":        "video",
		"my_show.nfo":               "nfo",
		"sub_dir/keep_inner.srt":    "subs",
		"plain.txt":                 "plain",
		"set_one.part01.rar":        "rar",
		"_unpack/unpacked_file.mp4": "unpacked",
	})
	buf, h := logged()

	out, err := e.finisher(finalize.WithLogger(slog.New(h()))).Finish(context.Background(), usenetDownload("Show"), dir, filepath.Join(dir, "_unpack"))
	require.NoError(t, err)

	got := listing(t, out)
	delete(got, ".bitnews-done")
	assert.Equal(t, map[string]string{
		"my.show.s01e01.mkv":     "video",
		"my.show.nfo":            "nfo",
		"sub.dir":                "<dir>",
		"sub.dir/keep_inner.srt": "subs", // not recursive
		"plain.txt":              "plain",
		"unpacked.file.mp4":      "unpacked",
	}, got)
	assert.Empty(t, e.warns(), "a rename is not a warning")
	assert.Contains(t, buf.String(), "my_show.nfo")
	assert.Contains(t, buf.String(), "my.show.nfo")
	assert.NotContains(t, buf.String(), "level=WARN")
}

func TestUsenetKeepsUnderscoresWhenTheOptionIsOff(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("Show")
	writeFiles(t, dir, map[string]string{"my_show.mkv": "video", "a_dir/x.txt": "x"})

	out, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, "")
	require.NoError(t, err)

	got := listing(t, out)
	delete(got, ".bitnews-done")
	assert.Equal(t, map[string]string{"my_show.mkv": "video", "a_dir": "<dir>", "a_dir/x.txt": "x"}, got)
}

func TestTorrentFilesKeepTheirUnderscoresEvenWhenTheOptionIsOn(t *testing.T) {
	e := newEnv(t)
	e.settings.PostProcess.UnderscoresToDots = true
	dir := e.torrentDir("Show")
	writeFiles(t, dir, map[string]string{"my_show.mkv": "video"})

	out, err := e.finisher().Finish(context.Background(), torrentDownload("Show"), dir, "")
	require.NoError(t, err)

	got := listing(t, out)
	delete(got, ".bitnews-done")
	assert.Equal(t, map[string]string{"my_show.mkv": "video"}, got)
}

func TestUnderscoresToDotsSkipsAnEntryWhoseDottedNameIsTaken(t *testing.T) {
	e := newEnv(t)
	e.settings.PostProcess.UnderscoresToDots = true
	dir := e.usenetDir("Show")
	writeFiles(t, dir, map[string]string{"a_b.mkv": "underscored", "a.b.mkv": "dotted", "c_d.mkv": "other"})

	out, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, "")
	require.NoError(t, err)

	got := listing(t, out)
	delete(got, ".bitnews-done")
	assert.Equal(t, map[string]string{"a_b.mkv": "underscored", "a.b.mkv": "dotted", "c.d.mkv": "other"}, got)
}

func TestUnderscoresToDotsDoesNothingTheSecondTimeAfterACrashBeforeTheMove(t *testing.T) {
	e := newEnv(t)
	e.settings.PostProcess.UnderscoresToDots = true
	dir := e.usenetDir("Show")
	writeFiles(t, dir, map[string]string{"my_show.mkv": "video"})
	crash := true
	e.mover = func(src, dst string) (fsutil.MoveResult, error) {
		if crash {
			crash = false
			return fsutil.MoveResult{}, errors.New("crash")
		}
		return fsutil.MoveDir(src, dst)
	}
	buf, h := logged()
	f := e.finisher(finalize.WithLogger(slog.New(h())))
	d := usenetDownload("Show")

	_, err := f.Finish(context.Background(), d, dir, "")
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(buf.String(), "my.show.mkv"), buf.String())
	buf.Reset()
	out, err := f.Finish(context.Background(), d, dir, "")

	require.NoError(t, err)
	assert.Empty(t, buf.String(), "nothing left to rename on the retry")
	got := listing(t, out)
	delete(got, ".bitnews-done")
	assert.Equal(t, map[string]string{"my.show.mkv": "video"}, got)
}

// A fully obfuscated post whose movie kept its random name (no usable PAR2
// set) is named after the Download, so media managers find a video file.
func TestUsenetNamesAnObfuscatedVideoAfterTheDownload(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("Film")
	writeFiles(t, dir, map[string]string{
		"13184cde52aaba45f8491bbf0db79b93": "\x1a\x45\xdf\xa3 matroska video",
		"release.nfo":                      "nfo",
	})
	buf, h := logged()

	out, err := e.finisher(finalize.WithLogger(slog.New(h()))).Finish(context.Background(), usenetDownload("Film.2025.1080p"), dir, "")

	require.NoError(t, err)
	got := listing(t, out)
	delete(got, finalize.MarkerName)
	assert.Equal(t, map[string]string{"Film.2025.1080p.mkv": "\x1a\x45\xdf\xa3 matroska video", "release.nfo": "nfo"}, got)
	assert.Contains(t, buf.String(), "named the obfuscated video after the download")
}

func TestTorrentFilesAreNeverRenamedEvenWhenObfuscated(t *testing.T) {
	e := newEnv(t)
	dir := e.torrentDir("Film")
	writeFiles(t, dir, map[string]string{"13184cde": "\x1a\x45\xdf\xa3 seeding"})

	out, err := e.finisher().Finish(context.Background(), torrentDownload("Film"), dir, "")

	require.NoError(t, err)
	assert.Contains(t, listing(t, out), "13184cde", "a seeded file keeps its name")
}
