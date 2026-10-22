package unpack

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Hostile archives made with Go's archive/zip and with 7zz itself. Every test
// unpacks into <base>/jail/out and then checks that <base> holds nothing but
// the jail: no file appeared outside the destination, whatever the outcome.
// (The jail directory itself exists only once the unpack got as far as staging.)

type zipEnt struct {
	name string
	data string
	mode fs.FileMode // zero: a plain file
}

func writeZip(t testing.TB, path string, ents ...zipEnt) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range ents {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		require.NoError(t, err)
		if e.data != "" {
			_, err = w.Write([]byte(e.data))
			require.NoError(t, err)
		}
	}
	require.NoError(t, zw.Close())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
}

// jail is a destination two levels below a scratch directory, so an escape by
// "../" lands inside the scratch directory where the test can see it.
type jail struct{ base, dst string }

func newJail(t testing.TB) jail {
	t.Helper()
	base := t.TempDir()
	return jail{base: base, dst: filepath.Join(base, "jail", "out")}
}

// requireContained fails when anything exists outside <base>/jail, or when
// anything is left in the jail besides the destination itself.
func (j jail) requireContained(t testing.TB, wantDst bool) {
	t.Helper()
	top := children(t, j.base)
	require.Subset(t, []string{"jail"}, top, "nothing may appear next to the jail")
	if len(top) == 0 {
		require.False(t, wantDst)
		return
	}
	if wantDst {
		require.Equal(t, []string{"out"}, children(t, filepath.Join(j.base, "jail")))
		return
	}
	require.Empty(t, children(t, filepath.Join(j.base, "jail")), "no destination and no staging directory")
}

// evilData is the content for an entry name: directories have none.
func evilData(name string) string {
	if strings.HasSuffix(name, "/") {
		return ""
	}
	return "evil"
}

func TestZipWithHostileNamesIsRefusedAndNothingEscapes(t *testing.T) {
	names := map[string]string{
		"parent":          "../x",
		"deep parent":     "d/../../up.txt",
		"absolute":        "/abs.txt",
		"drive":           "C:/win.txt",
		"backslash up":    `..\x`,
		"backslash up 2":  `a\..\..\x`,
		"dotdot":          "..",
		"dotdot slash":    "../",
		"unc":             `\\server\share\x`,
		"empty component": "a//../../x",
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			j := newJail(t)
			arch := t.TempDir()
			writeZip(t, filepath.Join(arch, "evil.zip"), zipEnt{name: "ok/fine.txt", data: "ok"}, zipEnt{name: name, data: evilData(name)})

			_, err := unpackSet(t, oneSet(filepath.Join(arch, "evil.zip")), j.dst)

			require.ErrorIs(t, err, ErrPathViolation)
			j.requireContained(t, false)
		})
	}
}

func TestZipWithOddButHarmlessNamesStaysInside(t *testing.T) {
	// 7zz may truncate at a NUL or keep a backslash as a letter; whatever it
	// makes of these, the result is inside the destination or refused.
	names := map[string]string{
		"nul byte":       "nul\x00byte.txt",
		"nul then dots":  "a\x00/../../x",
		"backslash":      `a\b.txt`,
		"colon":          "a:b.txt",
		"control chars":  "tab\tname\x01.txt",
		"newline":        "line\nbreak.txt",
		"newline forges": "x\nSize = 0\nAttributes = D",
		"reserved":       "con.txt",
		"unicode bidi":   "evil\u202egpj.exe",
		"leading dash":   "-rf",
		"at sign":        "@list.txt",
		"long":           strings.Repeat("n", 250) + ".txt",
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			j := newJail(t)
			arch := t.TempDir()
			writeZip(t, filepath.Join(arch, "odd.zip"), zipEnt{name: name, data: "content"})

			res, err := unpackSet(t, oneSet(filepath.Join(arch, "odd.zip")), j.dst)

			if err != nil {
				require.True(t, errorsIsAny(err, ErrPathViolation, ErrCorrupt), "unexpected class: %v", err)
				j.requireContained(t, false)
				return
			}
			j.requireContained(t, true)
			for _, f := range res.Files {
				require.NotContains(t, f, "..")
				require.FileExists(t, filepath.Join(j.dst, filepath.FromSlash(f)))
			}
		})
	}
}

func TestZipWithManyEntriesIsRefusedBeforeExtraction(t *testing.T) {
	arch := t.TempDir()
	ents := make([]zipEnt, 3000)
	for i := range ents {
		ents[i] = zipEnt{name: fmt.Sprintf("d%d/f%d", i%50, i)}
	}
	writeZip(t, filepath.Join(arch, "many.zip"), ents...)
	j := newJail(t)

	_, err := unpackSet(t, oneSet(filepath.Join(arch, "many.zip")), j.dst, WithMaxEntries(1000))

	require.ErrorIs(t, err, ErrTooManyEntries)
	require.ErrorIs(t, err, ErrLimit)
	j.requireContained(t, false)
}

func TestZipWithManyEntriesWithinTheCapExtracts(t *testing.T) {
	arch := t.TempDir()
	ents := make([]zipEnt, 3000)
	for i := range ents {
		ents[i] = zipEnt{name: fmt.Sprintf("d%d/f%d", i%50, i), data: "x"}
	}
	writeZip(t, filepath.Join(arch, "many.zip"), ents...)
	j := newJail(t)

	res, err := unpackSet(t, oneSet(filepath.Join(arch, "many.zip")), j.dst)

	require.NoError(t, err)
	require.Len(t, res.Files, 3000)
	j.requireContained(t, true)
}

func TestZipWithSymlinkEntriesIsRefused(t *testing.T) {
	cases := map[string][]zipEnt{
		"link to an absolute path": {{name: "ok.txt", data: "x"}, {name: "link", data: "/etc/passwd", mode: 0o777 | os.ModeSymlink}},
		"link to the parent":       {{name: "up", data: "..", mode: 0o777 | os.ModeSymlink}},
		"link then a file through": {{name: "esc", data: "../..", mode: 0o777 | os.ModeSymlink}, {name: "esc/pwned.txt", data: "evil"}},
		"dangling relative link":   {{name: "l", data: "no-such-target", mode: 0o777 | os.ModeSymlink}},
		"device":                   {{name: "dev", mode: os.ModeDevice | os.ModeCharDevice | 0o666}},
		"fifo":                     {{name: "pipe", mode: os.ModeNamedPipe | 0o666}},
		"socket":                   {{name: "sock", mode: os.ModeSocket | 0o666}},
	}
	for label, ents := range cases {
		t.Run(label, func(t *testing.T) {
			arch := t.TempDir()
			writeZip(t, filepath.Join(arch, "links.zip"), ents...)
			j := newJail(t)

			_, err := unpackSet(t, oneSet(filepath.Join(arch, "links.zip")), j.dst)

			require.ErrorIs(t, err, ErrPathViolation)
			j.requireContained(t, false)
		})
	}
}

func TestSevenZipArchivesWithSymlinksAreRefused(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "a.txt", []byte("hello"))
	require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(src, "abs-link")))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(src, "rel-link")))
	require.NoError(t, os.Symlink("../../..", filepath.Join(src, "up-link")))
	require.NoError(t, os.Symlink("nowhere", filepath.Join(src, "dangling")))
	for _, name := range []string{"abs-link", "rel-link", "up-link", "dangling"} {
		t.Run(name, func(t *testing.T) {
			arch := t.TempDir()
			// -snl stores the link itself instead of what it points to.
			run7zz(t, src, "a", "-snl", "-bd", filepath.Join(arch, "links.7z"), "a.txt", name)
			j := newJail(t)

			_, err := unpackSet(t, oneSet(filepath.Join(arch, "links.7z")), j.dst)

			require.ErrorIs(t, err, ErrPathViolation)
			j.requireContained(t, false)
		})
	}
}

func TestSevenZipArchiveWithAHardlinkEntryExtractsNoLink(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "a.txt", []byte("hello"))
	require.NoError(t, os.Link(filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")))
	arch := t.TempDir()
	run7zz(t, src, "a", "-snh", "-bd", filepath.Join(arch, "hard.7z"), "a.txt", "b.txt")
	j := newJail(t)

	_, err := unpackSet(t, oneSet(filepath.Join(arch, "hard.7z")), j.dst)

	if err == nil {
		// 7zz restored plain files; no file in the tree may be a hardlink.
		for _, p := range listing(t, j.dst) {
			require.EqualValues(t, 1, linkCount(mustLstat(t, filepath.Join(j.dst, p))), p)
		}
		j.requireContained(t, true)
		return
	}
	require.ErrorIs(t, err, ErrPathViolation)
	j.requireContained(t, false)
}

func mustLstat(t testing.TB, p string) fs.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	require.NoError(t, err)
	return fi
}

func TestACompressionBombIsRefusedFromTheListing(t *testing.T) {
	arch := t.TempDir()
	zeros := filepath.Join(t.TempDir(), "zeros.bin")
	require.NoError(t, os.WriteFile(zeros, make([]byte, 16<<20), 0o600))
	run7zz(t, filepath.Dir(zeros), "a", "-bd", "-mx5", filepath.Join(arch, "bomb.7z"), "zeros.bin")
	fi, err := os.Stat(filepath.Join(arch, "bomb.7z"))
	require.NoError(t, err)
	require.Less(t, fi.Size(), int64(16<<20)/100, "the fixture must be a real bomb: a ratio over 100")

	t.Run("over the thresholds", func(t *testing.T) {
		j := newJail(t)

		_, err := unpackSet(t, oneSet(filepath.Join(arch, "bomb.7z")), j.dst, withBombLimits(1<<20, 100))

		require.ErrorIs(t, err, ErrBomb)
		require.ErrorIs(t, err, ErrLimit)
		j.requireContained(t, false)
	})
	t.Run("under the output threshold", func(t *testing.T) {
		j := newJail(t)

		_, err := unpackSet(t, oneSet(filepath.Join(arch, "bomb.7z")), j.dst, withBombLimits(1<<30, 100))

		require.NoError(t, err)
	})
	t.Run("ratio below the limit", func(t *testing.T) {
		j := newJail(t)

		_, err := unpackSet(t, oneSet(filepath.Join(arch, "bomb.7z")), j.dst, withBombLimits(1<<20, 1<<20))

		require.NoError(t, err)
	})
}

// withBombLimits lowers the bomb thresholds so a test need not write a gigabyte.
func withBombLimits(minBytes, ratio int64) Option {
	return func(c *config) { c.bombMin, c.bombRatio = minBytes, ratio }
}

func TestAZipWhoseHeaderUnderstatesTheSizeIsCorruptAndWritesNoMore(t *testing.T) {
	var raw bytes.Buffer
	fw, err := flate.NewWriter(&raw, 9)
	require.NoError(t, err)
	zeros := make([]byte, 1<<20)
	sum := crc32.NewIEEE()
	for range 50 {
		_, _ = fw.Write(zeros)
		_, _ = sum.Write(zeros)
	}
	require.NoError(t, fw.Close())
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "lie.bin", Method: zip.Deflate, CRC32: sum.Sum32(), CompressedSize64: uint64(raw.Len()), UncompressedSize64: 100})
	require.NoError(t, err)
	_, err = w.Write(raw.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	arch := t.TempDir()
	path := writeFile(t, arch, "lie.zip", buf.Bytes())
	j := newJail(t)

	_, err = unpackSet(t, oneSet(path), j.dst)

	require.ErrorIs(t, err, ErrCorrupt)
	j.requireContained(t, false)
}

func TestEncryptedZipAndSevenZipPasswords(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "s.txt", []byte("the secret"))
	cases := map[string][]string{
		"zip ZipCrypto":     {"enc.zip", "-tzip", "-mem=ZipCrypto"},
		"zip AES-256":       {"enc.zip", "-tzip", "-mem=AES256"},
		"7z plain headers":  {"enc.7z", "-mhe=off"},
		"7z hidden headers": {"enc.7z", "-mhe=on"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			arch := t.TempDir()
			run7zzIn(t, src, "right\nright\n", append([]string{"a", "-p", "-bd", filepath.Join(arch, c[0])}, append(c[1:], "s.txt")...)...)
			set := oneSet(filepath.Join(arch, c[0]))

			t.Run("right", func(t *testing.T) {
				j := newJail(t)
				res, err := unpackSet(t, set, j.dst, WithPassword("right"))
				require.NoError(t, err)
				require.Equal(t, []string{"s.txt"}, res.Files)
				require.Equal(t, "the secret", readFile(t, filepath.Join(j.dst, "s.txt")))
			})
			t.Run("wrong", func(t *testing.T) {
				j := newJail(t)
				_, err := unpackSet(t, set, j.dst, WithPassword("wrong"))
				require.ErrorIs(t, err, ErrPassword)
				j.requireContained(t, false)
			})
			t.Run("none", func(t *testing.T) {
				j := newJail(t)
				_, err := unpackSet(t, set, j.dst)
				require.ErrorIs(t, err, ErrPassword)
				j.requireContained(t, false)
			})
		})
	}
}

func TestThePasswordIsNeverInAnErrorMessage(t *testing.T) {
	arch := t.TempDir()
	src := t.TempDir()
	writeFile(t, src, "s.txt", []byte("x"))
	run7zzIn(t, src, "hunter2-the-real-one\nhunter2-the-real-one\n", "a", "-p", "-bd", filepath.Join(arch, "e.7z"), "s.txt")

	_, err := unpackSet(t, oneSet(filepath.Join(arch, "e.7z")), newJail(t).dst, WithPassword("hunter2-guess"))

	require.ErrorIs(t, err, ErrPassword)
	require.NotContains(t, err.Error(), "hunter2")
}

func TestTruncatedSevenZipAndZipArchivesAreCorrupt(t *testing.T) {
	data := make([]byte, 600<<10)
	for i := range data {
		data[i] = byte(i*31 + i>>8)
	}
	src := t.TempDir()
	writeFile(t, src, "big.bin", data)
	arch := t.TempDir()
	run7zz(t, src, "a", "-bd", "-mx0", filepath.Join(arch, "big.7z"), "big.bin")
	run7zz(t, src, "a", "-bd", "-mx0", filepath.Join(arch, "big.zip"), "big.bin")
	for _, name := range []string{"big.7z", "big.zip"} {
		whole, err := os.ReadFile(filepath.Join(arch, name))
		require.NoError(t, err)
		for label, n := range map[string]int{"half": len(whole) / 2, "all but the tail": len(whole) - 20, "a few bytes": 40} {
			t.Run(name+"/"+label, func(t *testing.T) {
				cut := writeFile(t, t.TempDir(), name, whole[:n])
				j := newJail(t)

				_, err := unpackSet(t, oneSet(cut), j.dst)

				require.ErrorIs(t, err, ErrCorrupt)
				j.requireContained(t, false)
			})
		}
	}
}

func TestAFlippedByteInTheDataIsCorrupt(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "big.bin", bytes.Repeat([]byte("0123456789abcdef"), 40<<10))
	arch := t.TempDir()
	run7zz(t, src, "a", "-bd", "-mx0", filepath.Join(arch, "big.7z"), "big.bin")
	whole, err := os.ReadFile(filepath.Join(arch, "big.7z"))
	require.NoError(t, err)
	path := writeFile(t, t.TempDir(), "flip.7z", flip(whole, 300<<10))
	j := newJail(t)

	_, err = unpackSet(t, oneSet(path), j.dst)

	require.ErrorIs(t, err, ErrCorrupt)
	j.requireContained(t, false)
}

func TestSevenZipVolumesCreatedBy7zzWithAMissingVolume(t *testing.T) {
	data := make([]byte, 300<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	src := t.TempDir()
	writeFile(t, src, "big.bin", data)
	arch := t.TempDir()
	run7zz(t, src, "a", "-bd", "-mx0", "-v100k", filepath.Join(arch, "vol.7z"), "big.bin")
	vols, err := filepath.Glob(filepath.Join(arch, "vol.7z.*"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(vols), 3)

	t.Run("complete", func(t *testing.T) {
		j := newJail(t)
		res, err := unpackSet(t, detectOne(t, arch), j.dst)
		require.NoError(t, err)
		require.Equal(t, []string{"big.bin"}, res.Files)
		require.Equal(t, string(data), readFile(t, filepath.Join(j.dst, "big.bin")))
	})
	t.Run("missing middle volume is found by Detect", func(t *testing.T) {
		dir := t.TempDir()
		copyFile(t, vols[0], dir)
		copyFile(t, vols[2], dir)
		sets, err := Detect(dir)
		require.NoError(t, err)
		require.Len(t, sets, 1)
		j := newJail(t)

		_, err = unpackSet(t, sets[0], j.dst)

		require.ErrorIs(t, err, ErrMissingVolume)
		j.requireContained(t, false)
	})
	t.Run("missing last volume is found by 7zz", func(t *testing.T) {
		dir := t.TempDir()
		for _, v := range vols[:len(vols)-1] {
			copyFile(t, v, dir)
		}
		j := newJail(t)

		_, err := unpackSet(t, detectOne(t, dir), j.dst)

		require.ErrorIs(t, err, ErrCorrupt)
		j.requireContained(t, false)
	})
}

func copyFile(t testing.TB, src, dir string) {
	t.Helper()
	b, err := os.ReadFile(src)
	require.NoError(t, err)
	writeFile(t, dir, filepath.Base(src), b)
}

func TestGarbageThatLooksLikeAnArchiveIsCorrupt(t *testing.T) {
	for label, content := range map[string][]byte{
		"zeros":          make([]byte, 4096),
		"text":           []byte("this is not an archive at all\n"),
		"7z magic":       append(append([]byte{}, magic7z...), bytes.Repeat([]byte{0xA5}, 64)...),
		"rar5 magic":     append(append([]byte{}, magicRAR5...), bytes.Repeat([]byte{0xA5}, 64)...),
		"empty":          nil,
		"zip magic only": []byte("PK\x03\x04"),
	} {
		t.Run(label, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "junk.rar", content)
			j := newJail(t)

			_, err := unpackSet(t, oneSet(path), j.dst)

			require.ErrorIs(t, err, ErrCorrupt)
			j.requireContained(t, false)
		})
	}
}
