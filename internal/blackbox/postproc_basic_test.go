package blackbox_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// A release without PAR2 files and archives, the final destination of the
// output (Category folders), the cleanup and the optional tidying.

// withoutEnv removes variables from the environment of the daemon.
func withoutEnv(keys ...string) startOption {
	return func(o *startOptions) { o.drop = append(o.drop, keys...) }
}

func TestPostProcPlainReleaseReachesReadyInTheCompleteDir(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	mkv := bytesOf(1, 7000)
	nfo := []byte("a release note\n")
	rel := r.serve(newRelease(t, []relSpec{{Name: "movie.mkv", Data: mkv}, {Name: "movie.nfo", Data: nfo}}))

	id := r.add(rel, "My.Movie")
	out := r.ready(id)

	assert.Equal(t, filepath.Join(r.roots.complete, "My.Movie"), out, "<complete>/<name> without a Category")
	requireOutput(t, out, id, tree{"movie.mkv": ppFile(mkv), "movie.nfo": ppFile(nfo)})
	assert.NoDirExists(t, r.work(id), "the work dir was moved, not copied")
	assert.Equal(t, []string{"My.Movie"}, dirNames(t, r.roots.complete), "nothing else in the complete dir")

	tr := r.ev.traceUntil(id, "ready")
	assert.Equal(t, append(ppTrace("verifying", "finishing"), "state:ready", "ready"), tr,
		"fetched, post_processing, the stages in order (no unpacking without archives), ready")
	m := r.d.download(id)
	assert.Equal(t, out, m["output_path"])
	assert.NotNil(t, m["ready_at"])
	assert.NotNil(t, m["fetched_at"])
	assert.NotContains(t, m, "failure_reason")
	for _, f := range r.ev.of(id) {
		switch f.name {
		case "ready":
			assert.Equal(t, map[string]any{"id": id, "output_path": out}, fjson(t, f), "the ready event carries the output path")
		case "postprocess_stage":
			assert.Len(t, fjson(t, f), 2, "a stage event is {id, stage}")
		}
	}
	finalState := fjson(t, r.ev.of(id)[r.ev.firstIndex(id, stateIs(t, "ready"))])
	assert.Equal(t, out, finalState["output_path"], "the download_updated event of ready has the output path too")
	assert.NotNil(t, finalState["ready_at"])
}

func dirNames(t testing.TB, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestPostProcTheDefaultUmaskMakesTheOutputGroupWritable(t *testing.T) {
	t.Parallel()
	r := newPPRig(t, ppStart(withoutEnv("UMASK")))
	data := bytesOf(2, 5000)
	rel := r.serve(newRelease(t, []relSpec{{Name: "movie.mkv", Data: data}}))

	id := r.add(rel, "Group.Writable")
	out := r.ready(id)

	// The default UMASK is 002 (ADR-0006): the fetched files are created
	// 0666 and the directories 0777 before it applies, and nothing in
	// post-processing changes them.
	requireTree(t, out, tree{
		"movie.mkv":     ppFileMode(data, 0o664),
		".bitnews-done": ppFile(readFileOf(t, filepath.Join(out, ".bitnews-done"))), // always 0644
	})
	fi, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o775), fi.Mode().Perm())
}

func readFileOf(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return b
}

func TestPostProcCleanupRemovesPar2AndNzbFilesAndKeepsNfoAndSamples(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	mkv := bytesOf(3, 9000)
	sample := bytesOf(4, 2500)
	nfo := []byte("nfo text\n")
	set := makePar2(t, "movie", []relSpec{{Name: "movie.mkv", Data: mkv}}, 3000, 6, 2)
	specs := append([]relSpec{
		{Name: "movie.mkv", Data: mkv}, {Name: "movie-sample.mkv", Data: sample}, {Name: "movie.nfo", Data: nfo},
	}, set.all()...)
	rel := r.serve(newRelease(t, specs))

	id := r.add(rel, "Clean.Release")
	out := r.ready(id)

	requireOutput(t, out, id, tree{
		"movie.mkv": ppFile(mkv), "movie-sample.mkv": ppFile(sample), "movie.nfo": ppFile(nfo),
	})
	vols := idsOfNames(rel, volumeNames(set)...)
	require.NotEmpty(t, vols)
	assert.False(t, requestedAny(r.srv, vols), "a clean download fetches no recovery volume")
	for _, a := range idsOfNames(rel, "movie.mkv", "movie-sample.mkv", "movie.nfo", set.Index.Name) {
		assert.Equal(t, 1, r.srv.Requests(a), "every other article once: %s", a)
	}
	assert.Equal(t, append(ppTrace("verifying", "finishing"), "state:ready", "ready"), r.ev.traceUntil(id, "ready"))
}

func volumeNames(s par2Set) []string {
	var out []string
	for _, v := range s.Volumes {
		out = append(out, v.Name)
	}
	return out
}

func TestPostProcTheOutputFolderFollowsTheCategoryOfTheProtocol(t *testing.T) {
	t.Parallel()
	type tc struct {
		name string
		// setup stores the Categories and returns the fields of the add
		// request and the NZB options.
		setup func(t *testing.T, r *ppRig, lib string) (fields []string, opts []relOption)
		// want is the output dir relative to the complete root ("" lib
		// stands for the library dir the test made).
		want func(r *ppRig, lib string) string
	}
	cases := []tc{
		{
			name:  "no category",
			setup: func(*testing.T, *ppRig, string) ([]string, []relOption) { return nil, nil },
			want:  func(r *ppRig, _ string) string { return filepath.Join(r.roots.complete, "Rel") },
		},
		{
			name: "a category without folders is a subfolder of the complete dir",
			setup: func(t *testing.T, r *ppRig, _ string) ([]string, []relOption) {
				return []string{"category_id", string(addCategoryDirs(t, r.cfg, "series", "", ""))}, nil
			},
			want: func(r *ppRig, _ string) string { return filepath.Join(r.roots.complete, "series", "Rel") },
		},
		{
			name: "the category named in the NZB is found without regard to case",
			setup: func(t *testing.T, r *ppRig, _ string) ([]string, []relOption) {
				addCategoryDirs(t, r.cfg, "movies", "", "")
				return nil, []relOption{relMeta("category", "mOVIES")}
			},
			want: func(r *ppRig, _ string) string { return filepath.Join(r.roots.complete, "movies", "Rel") },
		},
		{
			name: "a Usenet folder replaces the complete dir and the category name",
			setup: func(t *testing.T, r *ppRig, lib string) ([]string, []relOption) {
				return []string{"category_id", string(addCategoryDirs(t, r.cfg, "series", filepath.Join(lib, "usenet-series"), ""))}, nil
			},
			want: func(_ *ppRig, lib string) string { return filepath.Join(lib, "usenet-series", "Rel") },
		},
		{
			name: "a torrent folder is not used for a Usenet download",
			setup: func(t *testing.T, r *ppRig, lib string) ([]string, []relOption) {
				return []string{"category_id", string(addCategoryDirs(t, r.cfg, "series", "", filepath.Join(lib, "torrent-series")))}, nil
			},
			want: func(r *ppRig, _ string) string { return filepath.Join(r.roots.complete, "series", "Rel") },
		},
		{
			name: "with both folders set the Usenet one is used",
			setup: func(t *testing.T, r *ppRig, lib string) ([]string, []relOption) {
				id := addCategoryDirs(t, r.cfg, "series", filepath.Join(lib, "usenet-series"), filepath.Join(lib, "torrent-series"))
				return []string{"category_id", string(id)}, nil
			},
			want: func(_ *ppRig, lib string) string { return filepath.Join(lib, "usenet-series", "Rel") },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lib := t.TempDir()
			var fields []string
			var opts []relOption
			r := newPPRig(t, ppBefore(func(r *ppRig) { fields, opts = c.setup(t, r, lib) }))
			data := bytesOf(5, 4000)
			rel := r.serve(newRelease(t, []relSpec{{Name: "a.mkv", Data: data}}, opts...))

			id := r.add(rel, "Rel", fields...)
			out := r.ready(id)

			want := c.want(r, lib)
			assert.Equal(t, want, out)
			requireOutput(t, out, id, tree{"a.mkv": ppFile(data)})
			parent, err := os.Stat(filepath.Dir(out))
			require.NoError(t, err)
			assert.Equal(t, fs.FileMode(0o755), parent.Mode().Perm(), "the folders above the output are created 0777 under the umask")
			if len(fields) > 0 || len(opts) > 0 {
				assert.NotNil(t, r.d.download(id)["category_id"], "the Download has its Category")
			}
		})
	}
}

func TestPostProcTwoDownloadsWithTheSameNameGetUniqueFolders(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a, b := bytesOf(6, 4000), bytesOf(7, 4100)
	relA := r.serve(newRelease(t, []relSpec{{Name: "a.mkv", Data: a}}))
	relB := r.serve(newRelease(t, []relSpec{{Name: "b.mkv", Data: b}}))

	idA := r.add(relA, "Same.Name")
	outA := r.ready(idA)
	idB := r.add(relB, "Same.Name")
	outB := r.ready(idB)

	assert.Equal(t, filepath.Join(r.roots.complete, "Same.Name"), outA)
	assert.Equal(t, filepath.Join(r.roots.complete, "Same.Name.1"), outB, "an existing folder is never merged into: the newcomer gets a counter")
	requireOutput(t, outA, idA, tree{"a.mkv": ppFile(a)})
	requireOutput(t, outB, idB, tree{"b.mkv": ppFile(b)})
}

func TestPostProcCleanNamesStripsReleaseBotSuffixesWhenSwitchedOn(t *testing.T) {
	t.Parallel()
	on := func(s *core.Settings) { s.PostProcess.CleanNames = true }
	for _, c := range []struct {
		name string
		tune []ppOption
		in   string
		want string
	}{
		{"on strips a bot suffix", []ppOption{ppTune(on)}, "Show.S01E01.1080p.WEB-DL-GROUP-4P", "Show.S01E01.1080p.WEB-DL-GROUP"},
		{"on strips a trailing group tag", []ppOption{ppTune(on)}, "Movie.2020.1080p.BluRay.x264-GRP-AB12", "Movie.2020.1080p.BluRay.x264-GRP"},
		{"on leaves a clean name", []ppOption{ppTune(on)}, "Name.With.Dots-GROUP", "Name.With.Dots-GROUP"},
		{"off leaves the suffix", nil, "Show.S01E01.1080p.WEB-DL-GROUP-4P", "Show.S01E01.1080p.WEB-DL-GROUP-4P"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newPPRig(t, c.tune...)
			data := bytesOf(8, 4000)
			rel := r.serve(newRelease(t, []relSpec{{Name: "a.mkv", Data: data}}))

			id := r.add(rel, c.in)
			out := r.ready(id)

			assert.Equal(t, c.want, r.d.download(id)["name"], "the cleaned name is the Download's name")
			assert.Equal(t, filepath.Join(r.roots.complete, c.want), out, "and the name of its folder")
			requireOutput(t, out, id, tree{"a.mkv": ppFile(data)})
		})
	}
}

func TestPostProcUnderscoresToDotsRenamesTheTopLevelOfTheResult(t *testing.T) {
	t.Parallel()
	r := newPPRig(t, ppTune(func(s *core.Settings) { s.PostProcess.UnderscoresToDots = true }))
	a, taken1, taken2, plain := bytesOf(9, 3500), bytesOf(10, 3100), bytesOf(11, 3200), bytesOf(12, 3300)
	rel := r.serve(newRelease(t, []relSpec{
		{Name: "my_movie_2024.mkv", Data: a},
		{Name: "x_y.nfo", Data: taken1}, {Name: "x.y.nfo", Data: taken2},
		{Name: "plain.txt", Data: plain},
	}))

	id := r.add(rel, "Dots_Release")
	out := r.ready(id)

	assert.Equal(t, filepath.Join(r.roots.complete, "Dots_Release"), out, "the folder name is not touched: only entries inside the result are")
	requireOutput(t, out, id, tree{
		"my.movie.2024.mkv": ppFile(a),
		"x_y.nfo":           ppFile(taken1), // the dotted name is taken: nothing is overwritten
		"x.y.nfo":           ppFile(taken2),
		"plain.txt":         ppFile(plain),
	})
}

func TestPostProcTheLargestFileWithoutAVideoExtensionIsNamedAfterTheDownload(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	video := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, bytesOf(13, 8000)...)
	small := bytesOf(14, 900)
	rel := r.serve(newRelease(t, []relSpec{
		{Name: "6f1d2c9ab04e8d77c3a15b09e2f46d38", Data: video}, // the obfuscated movie
		{Name: "a0b1c2d3e4f5", Data: small},                     // another file: left alone
		{Name: "Movie.2024-GRP.mkv", Data: bytesOf(15, 100)},    // the name is taken
	}))

	id := r.add(rel, "Movie.2024-GRP")
	out := r.ready(id)

	requireOutput(t, out, id, tree{
		"Movie.2024-GRP.1.mkv": ppFile(video), // a taken name gets a counter
		"Movie.2024-GRP.mkv":   ppFile(bytesOf(15, 100)),
		"a0b1c2d3e4f5":         ppFile(small),
	})
}

func TestPostProcTheVideoContainerDecidesTheExtension(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	filler := bytesOf(16, 6000)
	with := func(head []byte) []byte { return append(append([]byte(nil), head...), filler[len(head):]...) }
	ts := func(off, size int) []byte {
		b := append([]byte(nil), filler...)
		for i := range 4 {
			b[off+i*size] = 0x47
		}
		return b
	}
	cases := []struct {
		ext  string
		data []byte
	}{
		{".mkv", with([]byte{0x1a, 0x45, 0xdf, 0xa3})},
		{".mp4", with([]byte("\x00\x00\x00\x20ftypisom"))},
		{".mov", with([]byte("\x00\x00\x00\x14ftypqt  "))},
		{".avi", with([]byte("RIFF\x10\x20\x30\x40AVI "))},
		{".wmv", with([]byte{0x30, 0x26, 0xb2, 0x75, 0x8e, 0x66, 0xcf, 0x11})},
		{".mpg", with([]byte{0x00, 0x00, 0x01, 0xba})},
		{".ts", ts(0, 188)},
		{".m2ts", ts(4, 192)},
		{"", filler}, // not a video: stays as it is
	}
	for i, c := range cases {
		hash := fmt.Sprintf("%032x", i+1)
		rel := r.serve(newRelease(t, []relSpec{{Name: hash, Data: c.data}}))
		name := fmt.Sprintf("Release%d", i)

		id := r.add(rel, name)
		out := r.ready(id)

		want := name + c.ext
		if c.ext == "" {
			want = hash
		}
		requireOutput(t, out, id, tree{want: ppFile(c.data)})
	}
}

func TestPostProcAFileThatAlreadyHasAVideoExtensionIsNotRenamed(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	video := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, bytesOf(17, 5000)...)
	rel := r.serve(newRelease(t, []relSpec{{Name: "keep-this-name.AVI", Data: video}}))

	id := r.add(rel, "Another.Name")
	out := r.ready(id)

	requireOutput(t, out, id, tree{"keep-this-name.AVI": ppFile(video)})
}
