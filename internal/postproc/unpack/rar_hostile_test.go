package unpack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// builders runs a test against both RAR generations.
func builders() map[string]func([]rarEntry, rarSpec) [][]byte {
	return map[string]func([]rarEntry, rarSpec) [][]byte{"RAR4": buildRAR4, "RAR5": buildRAR5}
}

func TestRealRARArchivesWithHostileNamesFailSafely(t *testing.T) {
	names := []string{"../escape.txt", "a/../../escape.txt", "/etc/passwd", `C:\Windows\x`, "..", `..\..\x`, "sub/../../x"}
	for gen, build := range builders() {
		for _, name := range names {
			t.Run(gen+"/"+name, func(t *testing.T) {
				vols := build([]rarEntry{
					{name: "fine.txt", data: []byte("ok")},
					{name: name, data: []byte("evil")},
				}, rarSpec{})
				dir := writeVolumes(t, []string{"evil.rar"}, vols)
				out := t.TempDir()
				dst := filepath.Join(out, "out")
				sets, err := Detect(dir)
				require.NoError(t, err)

				_, err = Unpack(t.Context(), sets[0], dst, WithLogger(nil))

				require.ErrorIs(t, err, ErrPathViolation)
				require.Empty(t, listing(t, out), "no dst, no staging dir, nothing else")
			})
		}
	}
}

func TestRealRARArchivesSkipLinksAndSpecialEntries(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			entries := []rarEntry{
				{name: "ok.txt", data: []byte("fine")},
				{name: "link-to-etc", data: []byte("/etc"), attr: modeSymlink | 0o777, link: linkUnixSymlink},
				{name: "win-link", data: []byte(`C:\Windows`), link: linkWinSymlink},
				{name: "junction", data: []byte(`C:\x`), link: linkJunction},
				{name: "hard", data: []byte("ok.txt"), link: linkHardlink},
				{name: "copy", data: []byte("ok.txt"), link: linkFileCopy},
				{name: "pipe", attr: modeFIFO | 0o644},
				{name: "chardev", attr: modeChar | 0o644},
				{name: "sock", attr: modeSocket | 0o644},
				{name: "after.txt", data: []byte("still extracted")},
			}
			if gen == "RAR4" {
				// RAR4 has no link records; a symlink is a mode.
				entries = []rarEntry{entries[0], {name: "link-to-etc", data: []byte("/etc"), attr: modeSymlink | 0o777}, entries[6], entries[7], entries[8], entries[9]}
			}
			dir := writeVolumes(t, []string{"links.rar"}, build(entries, rarSpec{}))

			dst, res, err := unpackDir(t, dir)

			require.NoError(t, err)
			require.Equal(t, []string{"after.txt", "ok.txt"}, res.Files)
			require.Equal(t, []string{"after.txt", "ok.txt"}, listing(t, dst))
			require.Equal(t, len(entries)-2, res.Skipped)
		})
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
					require.Equal(t, os.FileMode(0o700), fi.Mode().Perm()&0o700, p+": owner rwx")
				} else {
					require.Zero(t, fi.Mode().Perm()&0o111, p+": not executable")
					require.Equal(t, os.FileMode(0o600), fi.Mode().Perm()&0o600, p+": owner rw")
				}
			}
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

func TestSolidRARArchivesStayInStepWhenAnEntryIsSkipped(t *testing.T) {
	for gen, build := range builders() {
		t.Run(gen, func(t *testing.T) {
			link := rarEntry{name: "l", data: []byte("/etc/passwd"), attr: modeSymlink | 0o777, link: linkUnixSymlink}
			if gen == "RAR4" {
				link.link = linkNone
			}
			dir := writeVolumes(t, []string{"solid.rar"}, build([]rarEntry{
				{name: "a", data: []byte("first")}, link, {name: "b", data: []byte("second")},
			}, rarSpec{solid: true}))

			dst, res, err := unpackDir(t, dir)

			require.NoError(t, err)
			require.Equal(t, []string{"a", "b"}, res.Files)
			require.Equal(t, "second", readFile(t, filepath.Join(dst, "b")))
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
