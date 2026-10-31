package blackbox_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// Unpacking: RAR (written by the builder copied from the unpack package, as
// "store" archives), 7z (written by the real 7zz), multi-volume sets, nested
// archives, passwords and the ways an archive can be bad.
//
// What the Go daemon does and the tests pin down: only RAR and 7z are archives
// to it, detected by their magic bytes (a zip stays a file); archive volumes,
// .par2 and .nzb files are deleted when something was unpacked; the unpacked
// files are set to 0644 and the directories to 0755 whatever the archive says.

// rarVolumes names the volumes of a RAR set the way the posts do: x.rar for a
// single volume, x.partNN.rar for RAR 5 and for new-style RAR 4, and x.rar
// then x.r00, x.r01, ... for old-style RAR 4.
func rarVolumes(base string, vols [][]byte, newStyle bool) []relSpec {
	var out []relSpec
	for i, v := range vols {
		var name string
		switch {
		case len(vols) == 1:
			name = base + ".rar"
		case newStyle:
			name = fmt.Sprintf("%s.part%02d.rar", base, i+1)
		case i == 0:
			name = base + ".rar"
		default:
			name = fmt.Sprintf("%s.r%02d", base, i-1)
		}
		out = append(out, relSpec{Name: name, Data: v})
	}
	return out
}

// Fixed content of the archives of this file.
var (
	movieData   = randBytes(40, 9000)
	trailerData = randBytes(41, 3300)
	scriptData  = []byte("#!/bin/sh\necho hi\n")
	nfoData     = []byte("posted next to the archive\n")
)

func rarEntries() []rarEntry {
	return []rarEntry{
		{name: "Movie.2024.mkv", data: movieData},
		{name: "extras", dir: true},
		{name: "extras/trailer.mkv", data: trailerData},
		{name: "run.sh", data: scriptData, attr: modeReg | 0o755}, // executable in the archive
	}
}

// archiveFiles are the same files for 7zz.
func archiveFiles() map[string][]byte {
	return map[string][]byte{"Movie.2024.mkv": movieData, "extras/trailer.mkv": trailerData, "run.sh": scriptData}
}

// unpackedTree is what unpacking rarEntries or archiveFiles leaves, with the
// modes forced to 0644 and 0755.
func unpackedTree() tree {
	return tree{
		"Movie.2024.mkv":     ppFile(movieData),
		"extras":             ppDir(),
		"extras/trailer.mkv": ppFile(trailerData),
		"run.sh":             ppFile(scriptData),
	}
}

func with(t tree, more tree) tree {
	out := tree{}
	for k, v := range t {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

func TestPostProcArchivesAreUnpackedAndTheirVolumesRemoved(t *testing.T) {
	t.Parallel()
	const hexRAR = "b3a1f60c9e2d47158c0a77e1d2f9b645"
	cases := []struct {
		name  string
		build func(t *testing.T) []relSpec
		// left are files the cleanup does not recognise as archive volumes.
		left tree
	}{
		{name: "RAR 5", build: func(*testing.T) []relSpec { return rarVolumes("rel", buildRAR5(rarEntries(), rarSpec{}), true) }},
		{name: "RAR 4", build: func(*testing.T) []relSpec { return rarVolumes("rel", buildRAR4(rarEntries(), rarSpec{}), false) }},
		{name: "RAR 5 in volumes", build: func(*testing.T) []relSpec {
			vols := buildRAR5(rarEntries(), rarSpec{perVolume: 5000})
			require.Greater(t, len(vols), 2)
			return rarVolumes("rel", vols, true)
		}},
		{name: "RAR 4 old-style volumes", build: func(*testing.T) []relSpec {
			vols := buildRAR4(rarEntries(), rarSpec{perVolume: 5000})
			require.Greater(t, len(vols), 2)
			return rarVolumes("rel", vols, false)
		}},
		{name: "RAR 4 new-style volumes", build: func(*testing.T) []relSpec {
			vols := buildRAR4(rarEntries(), rarSpec{perVolume: 5000, newNaming: true})
			require.Greater(t, len(vols), 2)
			return rarVolumes("rel", vols, true)
		}},
		{name: "RAR with a random name", build: func(*testing.T) []relSpec {
			return []relSpec{{Name: hexRAR, Data: buildRAR5(rarEntries(), rarSpec{})[0]}}
		}, left: tree{hexRAR: ppFile(buildRAR5(rarEntries(), rarSpec{})[0])}}, // not recognised by its name: it is unpacked and kept
		{name: "7z", build: func(t *testing.T) []relSpec { return sevenZipSet(t, "rel.7z", archiveFiles()) }},
		{name: "7z in volumes", build: func(t *testing.T) []relSpec {
			vols := sevenZipSet(t, "rel.7z", archiveFiles(), "-v4k", "-mx0")
			require.Greater(t, len(vols), 2)
			return vols
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newPPRig(t)
			rel := r.serve(newRelease(t, append(c.build(t), relSpec{Name: "rel.nfo", Data: nfoData})))

			id := r.add(rel, "Unpacked.Release")
			out := r.ready(id)

			requireOutput(t, out, id, with(with(unpackedTree(), tree{"rel.nfo": ppFile(nfoData)}), c.left))
			assert.Equal(t, append(ppTrace("verifying", "unpacking", "finishing"), "state:ready", "ready"), r.ev.traceUntil(id, "ready"))
			assert.Equal(t, filepath.Join(r.roots.complete, "Unpacked.Release"), out)
		})
	}
}

func TestPostProcAZipFileIsNotAnArchiveToTheDaemon(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	zip := sevenZipSet(t, "rel.zip", archiveFiles())
	require.Len(t, zip, 1)
	rel := r.serve(newRelease(t, zip))

	id := r.add(rel, "Zipped")
	out := r.ready(id)

	// Detection is by the magic bytes of RAR and 7z only, so a zip is
	// neither unpacked nor removed: it ends up in the output as a file.
	requireOutput(t, out, id, tree{"rel.zip": ppFile(zip[0].Data)})
	assert.Equal(t, append(ppTrace("verifying", "finishing"), "state:ready", "ready"), r.ev.traceUntil(id, "ready"))
}

func TestPostProcFilesThatCollideWithUnpackedOnesKeepTheirNamesAndTheNewcomersGetCounters(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	leftover := []byte("the nfo that was posted on its own\n")
	inArchive := []byte("the nfo inside the archive\n")
	rar := buildRAR5([]rarEntry{
		{name: "rel.nfo", data: inArchive}, {name: "Movie.mkv", data: movieData}, {name: "rel", dir: true}, {name: "rel/inner.txt", data: []byte("x")},
	}, rarSpec{})[0]
	rel := r.serve(newRelease(t, []relSpec{
		{Name: "rel.rar", Data: rar}, {Name: "rel.nfo", Data: leftover}, {Name: "rel", Data: []byte("a file named like the archive's folder")},
	}))

	id := r.add(rel, "Collisions")
	out := r.ready(id)

	// What was fetched stays as it is; the unpacked entry whose name is taken
	// gets the first free counter, before the extension of a file and at the
	// end of a folder's name.
	requireOutput(t, out, id, tree{
		"rel.nfo":         ppFile(leftover),
		"rel.1.nfo":       ppFile(inArchive),
		"Movie.mkv":       ppFile(movieData),
		"rel":             ppFile([]byte("a file named like the archive's folder")),
		"rel.1":           ppDir(),
		"rel.1/inner.txt": ppFile([]byte("x")),
	})
}

func TestPostProcSeveralArchiveSetsAreUnpackedAndMerged(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	a1, a2, b := randBytes(50, 4000), randBytes(51, 4100), randBytes(52, 4200)
	setA := rarVolumes("a", buildRAR5([]rarEntry{
		{name: "dir", dir: true}, {name: "dir/shared.bin", data: a1}, {name: "same.bin", data: a2},
	}, rarSpec{}), true)
	setB := rarVolumes("b", buildRAR5([]rarEntry{
		{name: "dir", dir: true}, {name: "dir/other.bin", data: b}, {name: "same.bin", data: b}, {name: "dir/shared.bin", data: b},
	}, rarSpec{}), true)
	rel := r.serve(newRelease(t, append(setA, setB...)))

	id := r.add(rel, "Two.Sets")
	out := r.ready(id)

	// The sets are unpacked one by one in name order, directories of the
	// same name are merged, and a colliding entry of the later set is renamed.
	requireOutput(t, out, id, tree{
		"dir":              ppDir(),
		"dir/shared.bin":   ppFile(a1),
		"dir/shared.1.bin": ppFile(b),
		"dir/other.bin":    ppFile(b),
		"same.bin":         ppFile(a2),
		"same.1.bin":       ppFile(b),
	})
}

func TestPostProcAnArchiveInsideAnArchiveIsUnpackedOneLevelDeep(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	deepFile := []byte("three levels down\n")
	note := []byte("a note in the middle\n")
	deep := buildRAR5([]rarEntry{{name: "deep.txt", data: deepFile}}, rarSpec{})[0]
	mid := buildRAR5([]rarEntry{{name: "note.txt", data: note}, {name: "deep.rar", data: deep}}, rarSpec{})[0]
	outer := buildRAR5([]rarEntry{{name: "mid.rar", data: mid}, {name: "movie.mkv", data: movieData}}, rarSpec{})[0]
	rel := r.serve(newRelease(t, []relSpec{{Name: "outer.rar", Data: outer}}))

	id := r.add(rel, "Nested")
	out := r.ready(id)

	// The outer set holds an archive, which is unpacked in place and
	// deleted. The archive inside that one is left as it is, and so it is
	// in the output.
	requireOutput(t, out, id, tree{
		"movie.mkv": ppFile(movieData),
		"note.txt":  ppFile(note),
		"deep.rar":  ppFile(deep),
	})
}

func TestPostProcTheArchivePasswordComesFromTheRequestTheNzbOrTheNameSuffix(t *testing.T) {
	t.Parallel()
	const secret = "pässword with spaces"
	enc7z := func(t *testing.T, extra ...string) []relSpec {
		return sevenZipSet(t, "rel.7z", archiveFiles(), append([]string{"-p" + secret}, extra...)...)
	}
	rar5 := func(pw string) []relSpec {
		es := rarEntries()
		for i := range es {
			if !es[i].dir {
				es[i].password = pw
			}
		}
		return rarVolumes("rel", buildRAR5(es, rarSpec{}), true)
	}
	rar4 := func(pw string) []relSpec {
		es := rarEntries()
		for i := range es {
			if !es[i].dir {
				es[i].password = pw
			}
		}
		return rarVolumes("rel", buildRAR4(es, rarSpec{}), false)
	}
	cases := []struct {
		name   string
		build  func(t *testing.T) []relSpec
		opts   []relOption
		file   string // the name of the uploaded NZB, without extension
		fields []string
		name_  string
	}{
		{name: "request field, 7z with encrypted data", build: func(t *testing.T) []relSpec { return enc7z(t) }, file: "Pw.One", fields: []string{"password", secret}},
		{name: "request field, 7z with encrypted headers", build: func(t *testing.T) []relSpec { return enc7z(t, "-mhe=on") }, file: "Pw.Two", fields: []string{"password", secret}},
		{name: "request field, RAR 5", build: func(*testing.T) []relSpec { return rar5(secret) }, file: "Pw.Three", fields: []string{"password", secret}},
		{name: "request field, RAR 4", build: func(*testing.T) []relSpec { return rar4(secret) }, file: "Pw.Four", fields: []string{"password", secret}},
		{name: "NZB meta password", build: func(t *testing.T) []relSpec { return enc7z(t) }, opts: []relOption{relMeta("password", secret)}, file: "Pw.Five"},
		{name: "password suffix of the file name", build: func(t *testing.T) []relSpec { return enc7z(t) }, file: "Pw.Six{{" + secret + "}}"},
		{name: "password suffix of the name field", build: func(t *testing.T) []relSpec { return enc7z(t) }, file: "ignored", fields: []string{"name", "Pw.Seven{{" + secret + "}}"}},
		{
			name: "the request wins over the NZB and the suffix", build: func(t *testing.T) []relSpec { return enc7z(t) },
			opts: []relOption{relMeta("password", "wrong-meta")}, file: "Pw.Eight{{wrong-suffix}}", fields: []string{"password", secret},
		},
		{
			name: "the NZB wins over the suffix", build: func(t *testing.T) []relSpec { return enc7z(t) },
			opts: []relOption{relMeta("password", secret)}, file: "Pw.Nine{{wrong-suffix}}",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newPPRig(t)
			rel := r.serve(newRelease(t, c.build(t), c.opts...))

			id := r.add(rel, c.file, c.fields...)
			out := r.ready(id)

			requireOutput(t, out, id, unpackedTree())
			name := r.d.download(id)["name"].(string)
			assert.NotContains(t, name, "{{", "the password suffix is cut off the name")
			assert.Equal(t, filepath.Join(r.roots.complete, name), out)
			assert.NotContains(t, r.d.apiGet("/api/v1/downloads/"+id).body, secret, "the password is never returned")
		})
	}
}

func TestPostProcAMissingOrWrongArchivePasswordFailsTheDownload(t *testing.T) {
	t.Parallel()
	enc := func(t *testing.T, extra ...string) []relSpec {
		return sevenZipSet(t, "rel.7z", archiveFiles(), append([]string{"-pright"}, extra...)...)
	}
	rarEnc := func(v5 bool) []relSpec {
		es := rarEntries()
		for i := range es {
			if !es[i].dir {
				es[i].password = "right"
			}
		}
		if v5 {
			return rarVolumes("rel", buildRAR5(es, rarSpec{}), true)
		}
		return rarVolumes("rel", buildRAR4(es, rarSpec{}), false)
	}
	const noPassword = "unpacking: unpack: archive password missing or wrong: the archive is encrypted and no password is set"
	wrong := []string{"password", "Wr0ng-pass-word"}
	cases := []struct {
		name   string
		build  func(t *testing.T) []relSpec
		fields []string
		// detail is what 7zz said, as the failure text keeps it.
		detail string
	}{
		{"7z data, no password", func(t *testing.T) []relSpec { return enc(t) }, nil, noPassword},
		{"7z data, wrong password", func(t *testing.T) []relSpec { return enc(t) }, wrong,
			"unpacking: unpack: archive password missing or wrong: ERROR: Data Error in encrypted file. Wrong password? : Movie.2024.mkv"},
		{"7z headers, no password", func(t *testing.T) []relSpec { return enc(t, "-mhe=on") }, nil,
			"unpacking: unpack: archive password missing or wrong: Break signaled"},
		{"7z headers, wrong password", func(t *testing.T) []relSpec { return enc(t, "-mhe=on") }, wrong,
			"unpacking: unpack: archive password missing or wrong: ERROR: <dir>"}, // the archive path is redacted
		{"RAR 5, no password", func(*testing.T) []relSpec { return rarEnc(true) }, nil, noPassword},
		{"RAR 5, wrong password", func(*testing.T) []relSpec { return rarEnc(true) }, wrong,
			"unpacking: unpack: archive password missing or wrong: ERROR: Wrong password : Movie.2024.mkv"},
		{"RAR 4, no password", func(*testing.T) []relSpec { return rarEnc(false) }, nil, noPassword},
		{"RAR 4, wrong password", func(*testing.T) []relSpec { return rarEnc(false) }, wrong,
			"unpacking: unpack: archive password missing or wrong: ERROR: CRC Failed in encrypted file. Wrong password? : Movie.2024.mkv"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newPPRig(t)
			rel := r.serve(newRelease(t, c.build(t)))

			id := r.add(rel, "Locked", c.fields...)
			m := r.failed(id, "archive_password")

			assert.Equal(t, c.detail, m["failure_detail"])
			assert.Equal(t, append(ppTrace("verifying", "unpacking"), "state:failed", "failed:archive_password"), r.ev.traceUntil(id, "failed:archive_password"))
			assert.Empty(t, dirNames(t, r.roots.complete), "nothing was moved")
			assert.NotContains(t, m["failure_detail"], "Wr0ng", "the password never reaches the failure text")
			// What was fetched is still there and the staging dir is gone.
			names := dirNames(t, r.work(id))
			assert.NotContains(t, names, "_unpack")
			assert.NotContains(t, names, "_unpack.tmp")
		})
	}
}

func TestPostProcBadArchivesFailWithTheirReason(t *testing.T) {
	t.Parallel()
	good7z := func(t *testing.T) []relSpec { return sevenZipSet(t, "rel.7z", archiveFiles(), "-mx0") }
	cases := []struct {
		name   string
		reason string
		build  func(t *testing.T) []relSpec
		detail string
	}{
		{"a flipped byte in a stored 7z", "archive_corrupt", func(t *testing.T) []relSpec {
			s := good7z(t)
			s[0].Data = flipByte(s[0].Data, 100)
			return s
		}, "unpacking: unpack: archive corrupt: ERROR: CRC Failed : Movie.2024.mkv"},
		{"a truncated 7z", "archive_corrupt", func(t *testing.T) []relSpec {
			s := good7z(t)
			s[0].Data = s[0].Data[:len(s[0].Data)/2]
			return s
		}, "unpacking: unpack: archive corrupt: ERROR: <dir>"},
		{"a RAR whose data does not match its CRC", "archive_corrupt", func(t *testing.T) []relSpec {
			v := rarVolumes("rel", buildRAR5(rarEntries(), rarSpec{}), true)
			v[0].Data = flipByte(v[0].Data, len(v[0].Data)/2)
			return v
		}, "unpacking: unpack: archive corrupt: ERROR: CRC Failed : Movie.2024.mkv"},
		{"a RAR that is only its signature", "archive_corrupt", func(*testing.T) []relSpec {
			return []relSpec{{Name: "rel.rar", Data: []byte("Rar!\x1a\x07\x01\x00")}}
		}, "unpacking: unpack: archive corrupt: ERROR: <dir>"},
		{"a RAR set without its second volume", "archive_corrupt", func(*testing.T) []relSpec {
			v := rarVolumes("rel", buildRAR5(rarEntries(), rarSpec{perVolume: 4000}), true)
			require.Greater(t, len(v), 3)
			return slices.Delete(v, 1, 2)
		}, `unpacking: archive corrupt: archive "rel": unpack: missing archive volume: archive corrupt: rel is missing volume 2`},
		{"an old-style RAR set without a middle volume", "archive_corrupt", func(*testing.T) []relSpec {
			v := rarVolumes("rel", buildRAR4(rarEntries(), rarSpec{perVolume: 4000}), false)
			require.Greater(t, len(v), 3)
			return slices.Delete(v, 2, 3)
		}, `unpacking: archive corrupt: archive "rel": unpack: missing archive volume: archive corrupt: rel is missing volume 2`},
		{"a RAR 5 file copy record that 7zz cannot extract", "archive_unsupported", func(*testing.T) []relSpec {
			return rarVolumes("rel", buildRAR5([]rarEntry{
				{name: "ok.txt", data: []byte("ok")}, {name: "copy", data: []byte("ok.txt"), link: linkFileCopy},
			}, rarSpec{}), true)
		}, "unpacking: unpack: unsupported archive feature: archive unsupported: ERROR: Unsupported Method : copy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newPPRig(t)
			rel := r.serve(newRelease(t, c.build(t)))

			id := r.add(rel, "Bad.Archive")
			m := r.failed(id, c.reason)

			assert.Equal(t, c.detail, m["failure_detail"])
			// A set with a volume missing is refused by the detection, which
			// runs before the unpacking stage is announced.
			stages := []string{"verifying", "unpacking"}
			if strings.Contains(c.detail, "missing archive volume") {
				stages = []string{"verifying"}
			}
			assert.Equal(t, append(ppTrace(stages...), "state:failed", "failed:"+c.reason), r.ev.traceUntil(id, "failed:"+c.reason))
			assert.Empty(t, dirNames(t, r.roots.complete), "nothing was moved")
		})
	}
}

func TestPostProcA7zSetWithoutItsFirstVolumeIsNotAnArchiveToTheDaemon(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	vols := sevenZipSet(t, "rel.7z", archiveFiles(), "-v4k", "-mx0")
	require.Greater(t, len(vols), 2)
	rest := vols[1:]
	rel := r.serve(newRelease(t, rest))

	id := r.add(rel, "Headless")
	out := r.ready(id)

	// The volumes after the first have no magic of their own and the first
	// is missing, so nothing is an archive: no unpacking stage, no failure,
	// and the cleanup (which only deletes volumes when something was
	// unpacked) leaves the pieces in the output.
	want := tree{}
	for _, v := range rest {
		want[v.Name] = ppFile(v.Data)
	}
	requireOutput(t, out, id, want)
	assert.Equal(t, append(ppTrace("verifying", "finishing"), "state:ready", "ready"), r.ev.traceUntil(id, "ready"))
}

func TestPostProcHostileArchiveEntriesFailWithoutWritingOutsideTheWorkDir(t *testing.T) {
	t.Parallel()
	const unsafe = "unpacking: unpack: unsafe path in archive: path violation: "
	type entry = rarEntry
	cases := []struct {
		name    string
		entries func(outside string) []entry
		// detail4 and detail5 are the failure texts for RAR 4 and RAR 5. The
		// name in them is as 7zz lists it, run through the path redaction
		// of failure texts (an absolute path becomes <dir>).
		detail4, detail5 string
	}{
		{"a parent directory", func(string) []entry { return []entry{{name: "../evil.txt", data: []byte("evil")}} },
			unsafe + `parent directory reference "../evil.txt"`, unsafe + `parent directory reference "../evil.txt"`},
		{"a parent directory in the middle", func(string) []entry { return []entry{{name: "a/../../evil.txt", data: []byte("evil")}} },
			unsafe + `parent directory reference "a/../../evil.txt"`, unsafe + `parent directory reference "a/../../evil.txt"`},
		{"a parent directory with backslashes", func(string) []entry { return []entry{{name: `..\..\evil.txt`, data: []byte("evil")}} },
			unsafe + `parent directory reference "../../evil.txt"`, unsafe + `parent directory reference "..<dir>"`},
		{"only a parent directory", func(string) []entry { return []entry{{name: "..", data: []byte("evil")}} },
			unsafe + `parent directory reference ".."`, unsafe + `parent directory reference ".."`},
		{"an absolute path", func(outside string) []entry { return []entry{{name: outside + "/evil.txt", data: []byte("evil")}} },
			unsafe + `absolute path "<dir>"`, unsafe + `absolute path "<dir>"`},
		{"a drive letter", func(string) []entry { return []entry{{name: `C:\Windows\evil.txt`, data: []byte("evil")}} },
			unsafe + `drive letter "C:/Windows/evil.txt"`, unsafe + `drive letter "<dir>"`},
		{"a symlink", func(string) []entry {
			return []entry{{name: "evil-link", data: []byte("/etc"), attr: modeSymlink | 0o777}}
		}, unsafe + `the archive contains a symlink "evil-link"`, unsafe + `the archive contains a symlink "evil-link"`},
		{"a fifo", func(string) []entry { return []entry{{name: "evil-pipe", attr: modeFIFO | 0o644}} },
			unsafe + `the archive contains a fifo "evil-pipe"`, unsafe + `the archive contains a fifo "evil-pipe"`},
		{"a device", func(string) []entry { return []entry{{name: "evil-dev", attr: modeChar | 0o644}} },
			unsafe + `the archive contains a device "evil-dev"`, unsafe + `the archive contains a device "evil-dev"`},
		{"two names that differ only in case", func(string) []entry {
			return []entry{{name: "Evil.txt", data: []byte("1")}, {name: "evil.TXT", data: []byte("2")}}
		}, unsafe + `duplicate entry "evil.TXT"`, unsafe + `duplicate entry "evil.TXT"`},
	}
	for _, build := range []struct {
		name string
		do   func([]rarEntry) []relSpec
		want func(c int) string
	}{
		{"RAR 4", func(es []rarEntry) []relSpec { return rarVolumes("arc", buildRAR4(es, rarSpec{}), false) }, func(c int) string { return cases[c].detail4 }},
		{"RAR 5", func(es []rarEntry) []relSpec { return rarVolumes("arc", buildRAR5(es, rarSpec{}), true) }, func(c int) string { return cases[c].detail5 }},
	} {
		for i, c := range cases {
			t.Run(build.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				r := newPPRig(t)
				es := append([]rarEntry{{name: "fine.txt", data: []byte("fine")}}, c.entries(r.roots.outside)...)
				rel := r.serve(newRelease(t, build.do(es)))

				id := r.add(rel, "Hostile.Archive")
				m := r.failed(id, "path_violation")

				assert.Equal(t, build.want(i), m["failure_detail"])
				assert.Empty(t, dirNames(t, r.roots.complete), "nothing was moved")
				assert.Empty(t, dirNames(t, r.roots.outside), "nothing was written to the directory the archive aimed at")
				assert.Empty(t, findNamed(t, filepath.Dir(r.roots.complete), "evil"), "no file of the archive anywhere in the tree")
				names := dirNames(t, r.work(id))
				assert.NotContains(t, names, "_unpack")
				assert.NotContains(t, names, "_unpack.tmp")
				assert.NotContains(t, names, "fine.txt", "the staging dir with the entries before the bad one is gone")
				assert.Equal(t, append(ppTrace("verifying", "unpacking"), "state:failed", "failed:path_violation"), r.ev.traceUntil(id, "failed:path_violation"))
			})
		}
	}
}

func TestPostProcA7zSymlinkFailsTheDownload(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "real.txt"), []byte("real"), 0o644))
	require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(src, "evil-link")))
	dst := t.TempDir()
	runTool(t, src, "7zz", "a", "-bd", "-y", "-snl", filepath.Join(dst, "rel.7z"), "real.txt", "evil-link")
	rel := r.serve(newRelease(t, readDirSpecs(t, dst, func(string) bool { return true })))

	id := r.add(rel, "Linked")
	m := r.failed(id, "path_violation")

	assert.Equal(t, `unpacking: unpack: unsafe path in archive: path violation: the archive contains a symlink "evil-link"`, m["failure_detail"])
	assert.Empty(t, dirNames(t, r.roots.complete))
}

func TestPostProcAnArchiveThatDeclaresAHugeOutputIsRefusedAsABomb(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	// 1.1 GiB of zeros pack into a few kilobytes, but they are never
	// written: the listing declares the size and the ratio of more than
	// 100:1 over 1 GiB is refused before 7zz extracts anything.
	src := t.TempDir()
	f, err := os.Create(filepath.Join(src, "zeros.bin"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1100<<20))
	require.NoError(t, f.Close())
	dst := t.TempDir()
	runTool(t, src, "7zz", "a", "-bd", "-y", "-mx1", filepath.Join(dst, "rel.7z"), "zeros.bin")
	specs := readDirSpecs(t, dst, func(string) bool { return true })
	require.Less(t, len(specs[0].Data), 1<<20)
	rel := r.serve(newRelease(t, specs))

	id := r.add(rel, "Bomb")
	m := r.failed(id, "archive_corrupt")

	assert.Regexp(t, `^unpacking: unpack: resource limit exceeded: compression ratio too high: archive corrupt: archive of \d+ bytes declares 1153433600$`, m["failure_detail"])
	assert.Empty(t, dirNames(t, r.roots.complete))
	assert.NotContains(t, dirNames(t, r.work(id)), "zeros.bin")
}

func TestPostProcAnArchiveThatDoesNotFitOnTheDiskFailsWithDiskFull(t *testing.T) {
	t.Parallel()
	var minFree int64
	r := newPPRig(t, ppTune(func(s *core.Settings) {
		// Keep all but 32 MiB of the free space of the disk back.
		free := freeBytes(t, os.TempDir())
		minFree = int64(free) - 32<<20
		s.Queues.MinFree = minFree
	}))
	src := t.TempDir()
	f, err := os.Create(filepath.Join(src, "zeros.bin"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(256<<20)) // under the bomb limit of 1 GiB
	require.NoError(t, f.Close())
	dst := t.TempDir()
	runTool(t, src, "7zz", "a", "-bd", "-y", "-mx1", filepath.Join(dst, "rel.7z"), "zeros.bin")
	rel := r.serve(newRelease(t, readDirSpecs(t, dst, func(string) bool { return true })))
	require.Positive(t, minFree)

	id := r.add(rel, "Too.Big")
	m := r.failed(id, "disk_full")

	assert.Regexp(t, `^unpacking: unpack: resource limit exceeded: not enough free disk space: disk full: need 268435456 bytes, \d+ free, \d+ reserved$`, m["failure_detail"])
	assert.Empty(t, dirNames(t, r.roots.complete))
	assert.NotContains(t, dirNames(t, r.work(id)), "zeros.bin")
}

func TestPostProcTheUnpackedTreeOfAnArchiveWithNamesToCleanIsUnderscoresToDotsOnlyAtTheTopLevel(t *testing.T) {
	t.Parallel()
	r := newPPRig(t, ppTune(func(s *core.Settings) { s.PostProcess.UnderscoresToDots = true }))
	rel := r.serve(newRelease(t, sevenZipSet(t, "rel.7z", map[string][]byte{
		"top_file.txt":      []byte("1"),
		"Sub_Dir/in_er.txt": []byte("2"),
	})))

	id := r.add(rel, "Underscores")
	out := r.ready(id)

	requireOutput(t, out, id, tree{
		"top.file.txt":      ppFile([]byte("1")),
		"Sub.Dir":           ppDir(),
		"Sub.Dir/in_er.txt": ppFile([]byte("2")), // the option does not recurse
	})
}

func TestPostProcTheFailureOfAnArchiveDoesNotKeepTheOtherFilesFromStayingInTheWorkDir(t *testing.T) {
	t.Parallel()
	r := newPPRig(t)
	rel := r.serve(newRelease(t, []relSpec{
		{Name: "rel.rar", Data: buildRAR5([]rarEntry{{name: "../evil", data: []byte("x")}}, rarSpec{})[0]},
		{Name: "rel.nfo", Data: nfoData},
	}))

	id := r.add(rel, "Stays")
	r.failed(id, "path_violation")

	names := dirNames(t, r.work(id))
	slices.Sort(names)
	assert.Equal(t, []string{"Stays.nzb", "rel.nfo", "rel.rar"}, names, "the fetched files and the NZB stay for the user to look at")
	assert.True(t, strings.HasSuffix(names[0], ".nzb"))
}

// freeBytes is the free space of the disk of dir.
func freeBytes(t testing.TB, dir string) uint64 {
	t.Helper()
	return statfsFree(t, dir)
}
