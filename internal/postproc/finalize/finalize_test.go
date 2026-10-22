package finalize_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsenetWithoutArchivesRemovesOnlyPar2AndNzb(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("Show.S01E01")
	writeFiles(t, dir, map[string]string{
		"show.mkv":           "video",
		"show.nfo":           "nfo",
		"sample/sample.mkv":  "sample",
		"show.par2":          "p",
		"show.vol00+01.PAR2": "p",
		"Show.S01E01.nzb":    "n",
		"extras.rar":         "kept: nothing was unpacked",
	})

	out, err := e.finisher().Finish(context.Background(), usenetDownload("Show.S01E01"), dir, "")
	require.NoError(t, err)

	want := filepath.Join(e.settings.Paths.Usenet.Complete, "Show.S01E01")
	require.Equal(t, want, out)
	require.NoDirExists(t, dir)
	got := listing(t, out)
	delete(got, ".bitnews-done")
	require.Equal(t, map[string]string{
		"show.mkv":          "video",
		"show.nfo":          "nfo",
		"sample":            "<dir>",
		"sample/sample.mkv": "sample",
		"extras.rar":        "kept: nothing was unpacked",
	}, got)
}

func TestUsenetWithArchivesDeliversUnpackedContentAndKeepsSampleAndNfo(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("Movie.2024")
	writeFiles(t, dir, map[string]string{
		"movie.part01.rar":       "rar",
		"movie.part02.rar":       "rar",
		"old.rar":                "rar",
		"old.r00":                "rar",
		"old.R01":                "rar",
		"old.s00":                "rar",
		"set.7z.001":             "7z",
		"set.7z.002":             "7z",
		"single.7z":              "7z",
		"movie.par2":             "p",
		"movie.vol01+02.par2":    "p",
		"movie.nzb":              "n",
		"movie.nfo":              "nfo",
		"movie-sample.mkv":       "sample",
		"notes.txt":              "kept",
		"_unpack.tmp/half.bin":   "half",
		"_unpack/Movie.2024.mkv": "movie",
		"_unpack/Subs/en.srt":    "subs",
	})

	out, err := e.finisher().Finish(context.Background(), usenetDownload("Movie.2024"), dir, filepath.Join(dir, "_unpack"))
	require.NoError(t, err)

	got := listing(t, out)
	delete(got, ".bitnews-done")
	require.Equal(t, map[string]string{
		"Movie.2024.mkv":   "movie",
		"Subs":             "<dir>",
		"Subs/en.srt":      "subs",
		"movie.nfo":        "nfo",
		"movie-sample.mkv": "sample",
		"notes.txt":        "kept",
	}, got)
	require.NoDirExists(t, dir)
}
