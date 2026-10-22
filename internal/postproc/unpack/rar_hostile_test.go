package unpack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRealRARArchivesWithHostileNamesFailSafely(t *testing.T) {
	names := []string{"../escape.txt", "a/../../escape.txt", "/etc/passwd", `C:\Windows\x`, "..", `..\..\x`, "sub/../../x"}
	for gen, build := range builders() {
		for _, name := range names {
			t.Run(gen+"/"+name, func(t *testing.T) {
				dir := writeVolumes(t, []string{"evil.rar"}, build([]rarEntry{
					{name: "fine.txt", data: []byte("ok")},
					{name: name, data: []byte("evil")},
				}, rarSpec{}))
				base := t.TempDir()
				dst := filepath.Join(base, "jail", "out")
				sets, err := Detect(dir)
				require.NoError(t, err)

				_, err = unpackSet(t, sets[0], dst)

				require.ErrorIs(t, err, ErrPathViolation)
				require.Equal(t, []string{"jail"}, children(t, base), "nothing outside jail")
				require.Empty(t, children(t, filepath.Join(base, "jail")), "no dst, no staging dir")
			})
		}
	}
}

func TestRealRARArchivesWithLinksAndSpecialEntriesAreRefused(t *testing.T) {
	// RAR4 has no link records; its symlink is a mode.
	specials := map[string]struct {
		rar5Only bool
		e        rarEntry
	}{
		"symlink mode": {e: rarEntry{name: "link-to-etc", data: []byte("/etc"), attr: modeSymlink | 0o777}},
		"unix symlink": {rar5Only: true, e: rarEntry{name: "link-to-etc", data: []byte("/etc"), attr: modeSymlink | 0o777, link: linkUnixSymlink}},
		"win symlink":  {rar5Only: true, e: rarEntry{name: "win-link", data: []byte(`C:\Windows`), link: linkWinSymlink}},
		"junction":     {rar5Only: true, e: rarEntry{name: "junction", data: []byte(`C:\x`), link: linkJunction}},
		"hardlink":     {rar5Only: true, e: rarEntry{name: "hard", data: []byte("ok.txt"), link: linkHardlink}},
		"file copy":    {rar5Only: true, e: rarEntry{name: "copy", data: []byte("ok.txt"), link: linkFileCopy}},
		"fifo":         {e: rarEntry{name: "pipe", attr: modeFIFO | 0o644}},
		"char device":  {e: rarEntry{name: "chardev", attr: modeChar | 0o644}},
		"socket":       {e: rarEntry{name: "sock", attr: modeSocket | 0o644}},
	}
	for gen, build := range builders() {
		for label, c := range specials {
			if c.rar5Only && gen == "RAR4" {
				continue
			}
			t.Run(gen+"/"+label, func(t *testing.T) {
				dir := writeVolumes(t, []string{"links.rar"}, build([]rarEntry{
					{name: "ok.txt", data: []byte("fine")}, c.e, {name: "after.txt", data: []byte("x")},
				}, rarSpec{}))
				base := t.TempDir()

				_, err := unpackSet(t, detectOne(t, dir), filepath.Join(base, "out"))

				require.Error(t, err)
				if label == "file copy" {
					// 7zz lists a RAR5 file copy as a file and cannot extract it.
					require.ErrorIs(t, err, ErrUnsupported)
				} else {
					require.ErrorIs(t, err, ErrPathViolation)
				}
				require.Empty(t, children(t, base))
			})
		}
	}
}

func TestRealRARArchivesDropSetuidAndOtherModeBits(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			entries := []rarEntry{
				{name: "suid", data: []byte("x"), attr: modeReg | 0o4755},
				{name: "sgid", data: []byte("x"), attr: modeReg | 0o2755},
				{name: "sticky", data: []byte("x"), attr: modeReg | 0o1777},
				{name: "exec", data: []byte("x"), attr: modeReg | 0o777},
				{name: "readonly", data: []byte("x"), attr: modeReg},
				{name: "d", dir: true, attr: modeDir | 0o4000},
				{name: "d/f", data: []byte("x")},
			}
			dir := writeVolumes(t, []string{"modes.rar"}, build(entries, rarSpec{}))

			dst, _, err := unpackDir(t, dir)

			require.NoError(t, err)
			for _, p := range listing(t, dst) {
				fi, err := os.Lstat(filepath.Join(dst, p))
				require.NoError(t, err)
				require.Zero(t, fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky), p)
				if fi.IsDir() {
					require.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), p)
				} else {
					require.Equal(t, os.FileMode(0o644), fi.Mode().Perm(), p)
				}
			}
		})
	}
}

func TestADirectoryWithoutPermissionsDoesNotBlockCleanupOrTheResult(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			dir := writeVolumes(t, []string{"locked.rar"}, build([]rarEntry{
				{name: "d", dir: true, attr: modeDir},
				{name: "d/f", data: []byte("inside")},
			}, rarSpec{}))

			dst, res, err := unpackDir(t, dir)

			require.NoError(t, err)
			require.Equal(t, []string{"d/f"}, res.Files)
			require.Equal(t, "inside", readFile(t, filepath.Join(dst, "d", "f")))

			// And when the unpack fails later, the staging directory still goes.
			dir = writeVolumes(t, []string{"locked.rar"}, build([]rarEntry{
				{name: "d", dir: true, attr: modeDir},
				{name: "d/f", data: []byte("inside")},
			}, rarSpec{}))
			dst, _, err = unpackDir(t, dir, WithMaxBytes(1))
			require.ErrorIs(t, err, ErrTooLarge)
			require.NoDirExists(t, dst+".tmp")
		})
	}
}

func TestRealRARArchivesRejectCaseDuplicates(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			dir := writeVolumes(t, []string{"dup.rar"}, build([]rarEntry{
				{name: "Movie.mkv", data: []byte("1")},
				{name: "movie.MKV", data: []byte("2")},
			}, rarSpec{}))

			dst, _, err := unpackDir(t, dir)

			require.ErrorIs(t, err, ErrPathViolation)
			require.NoDirExists(t, dst)
			require.NoDirExists(t, dst+".tmp")
		})
	}
}

func TestSolidRARArchivesExtract(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			dir := writeVolumes(t, []string{"solid.rar"}, build([]rarEntry{
				{name: "a", data: []byte("first")}, {name: "b", data: []byte("second")},
			}, rarSpec{solid: true}))

			dst, res, err := unpackDir(t, dir)

			require.NoError(t, err)
			require.Equal(t, []string{"a", "b"}, res.Files)
			require.Equal(t, "second", readFile(t, filepath.Join(dst, "b")))
		})
	}
}

func TestNamesThatSanitizingChangesAreRenamedInPlace(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			dir := writeVolumes(t, []string{"names.rar"}, build([]rarEntry{
				{name: "dir/a:b?.txt", data: []byte("1")},
				{name: "dir/con.txt", data: []byte("2")},
				{name: "trailing. ", data: []byte("3")},
			}, rarSpec{}))

			dst, res, err := unpackDir(t, dir)

			require.NoError(t, err)
			require.Equal(t, []string{"dir/_con.txt", "dir/a_b_.txt", "trailing"}, res.Files)
			require.Equal(t, []string{"dir", "dir/_con.txt", "dir/a_b_.txt", "trailing"}, listing(t, dst))
		})
	}
}

func TestCorruptRARArchivesFailCleanly(t *testing.T) {
	for gen, build := range builders() {
		whole := build([]rarEntry{{name: "f.bin", data: payload(3000, 5)}, {name: "g.bin", data: payload(100, 6)}}, rarSpec{})[0]
		cases := map[string][]byte{
			"magic only":              whole[:len(magicRAR4)],
			"cut right after magic":   whole[:12],
			"cut in the first header": whole[:30],
			"cut inside file data":    whole[:len(whole)/2],
			"flipped header byte":     flip(whole, 20),
			"flipped data byte":       flip(whole, 200),
		}
		if gen == "RAR5" {
			cases["magic only"] = whole[:len(magicRAR5)]
		}
		for label, content := range cases {
			t.Run(gen+"/"+label, func(t *testing.T) {
				dir := t.TempDir()
				writeFile(t, dir, "bad.rar", content)

				dst, _, err := unpackDir(t, dir)

				require.ErrorIs(t, err, ErrCorrupt)
				require.NoDirExists(t, dst)
				require.NoDirExists(t, dst+".tmp")
			})
		}
	}
}
