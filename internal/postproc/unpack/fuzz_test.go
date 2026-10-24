package unpack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroosing/bitnews/internal/fsutil"
	"github.com/jroosing/bitnews/internal/postproc/tool/tooltest"
)

// FuzzEntryName feeds arbitrary archive entry names to the guard. Whatever it
// accepts must stay below the staging directory once joined.
func FuzzEntryName(f *testing.F) {
	for _, seed := range []string{
		"a.txt", "dir/a.txt", "../x", "/etc/passwd", `C:\x`, "c:x", `\\server\share\x`,
		"a/../../b", "a\x00b", "", ".", "./", "..", "a/..", "a/./b", `a\..\b`, "con.txt",
		" . ", "a/ /b", "a//b", strings.Repeat("a/", 200), strings.Repeat("é", 300),
		"\xff\xfe", "a\u202eb", "..\u2215x", "%2e%2e/x", "....//x", "a/b/",
	} {
		f.Add(seed)
	}
	stage := f.TempDir()
	f.Fuzz(func(t *testing.T, name string) {
		rel, err := cleanName(name)
		if err != nil {
			return
		}
		if rel == "" {
			return // the archive root itself
		}
		if strings.ContainsAny(rel, "\x00\\") || strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
			t.Fatalf("cleanName(%q) = %q is not a plain relative path", name, rel)
		}
		for c := range strings.SplitSeq(rel, "/") {
			if c == "" || c == "." || c == ".." {
				t.Fatalf("cleanName(%q) = %q has component %q", name, rel, c)
			}
		}
		joined, err := fsutil.SafeJoin(stage, rel)
		if err != nil {
			t.Fatalf("SafeJoin rejected the cleaned name %q (from %q): %v", rel, name, err)
		}
		back, err := filepath.Rel(stage, joined)
		if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) || filepath.IsAbs(back) {
			t.Fatalf("%q (from %q) escapes the staging dir: %q", rel, name, joined)
		}
	})
}

// FuzzParseListing feeds arbitrary bytes to the parser of the 7zz listing,
// the one place where the output of the external program is interpreted.
// Whatever the input, it must not panic, a failure must be ErrCorrupt, and a
// success must hold only entries that are well formed: a name taken from a
// "Path = " line, a size in range, and no more entries than "Path = " lines.
func FuzzParseListing(f *testing.F) {
	for _, seed := range []string{
		"",
		"\nEnter password:\n",
		"Path = a\nSize = 1\nAttributes = A -rw-r--r--\nEncrypted = -\n\n",
		"Path = d\nFolder = +\nSize = 0\nAttributes = D drwxr-xr-x\n\nPath = d/f\nSize = 3\n\n",
		"Path = l\nSize = 3\nAttributes =  lrwxrwxrwx\n\n",
		"Path = a\nSize = 0\nAttributes = D\nFolder = -\nSize = 99999999\n\n",
		"Path = a\nb\nSize = 5\n\n",
		"Path = a\nPath = ../../x\n\n",
		"Path = a\nSize = -1\n\n",
		"Path = a\nSize = 99999999999999999999\n\n",
		"Path = a\n = 5\n\n",
		"Path = h\nHard Link = x\n\n",
		"Path = a\r\nSize = 3\r\n\r\n",
		"Path = a\nSize = 1\n\nERRORS:\nHeaders Error\n",
		"Path = " + strings.Repeat("a/", 5000) + "\nSize = 1\n\n",
		"Path = a\n" + strings.Repeat("Key = v\n", 50) + "\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseListing(data)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("unclassified listing error: %v", err)
			}
			if entries != nil {
				t.Fatal("entries returned together with an error")
			}
			return
		}
		paths := bytes.Count(data, []byte("\nPath = "))
		if bytes.HasPrefix(data, []byte("Path = ")) {
			paths++
		}
		if len(entries) > paths {
			t.Fatalf("%d entries from %d Path lines", len(entries), paths)
		}
		g := newGuard(newConfig([]Option{WithLogger(nil)}))
		for _, e := range entries {
			if e.size < 0 || e.size > 1<<62 {
				t.Fatalf("size %d out of range", e.size)
			}
			if e.kind == kindDir && e.size != 0 {
				t.Fatalf("directory %q with size %d", e.name, e.size)
			}
			if bytes.IndexByte([]byte(e.name), '\n') >= 0 {
				t.Fatalf("name %q holds a line break", e.name)
			}
			_ = g.admit(e) // must not panic, whatever the verdict
		}
		again, err := parseListing(data)
		if err != nil || len(again) != len(entries) {
			t.Fatal("parsing is not deterministic")
		}
	})
}

// FuzzUnpack feeds arbitrary bytes to the real 7zz through Unpack. Whatever
// the input, Unpack must fail with a classified error or succeed with a tree
// that lies inside the destination, and must leave neither a staging directory
// nor, after a failure, a destination behind.
func FuzzUnpack(f *testing.F) {
	f.Add(uint8(0), magicRAR4)
	f.Add(uint8(0), magicRAR5)
	f.Add(uint8(1), magic7z)
	f.Add(uint8(0), buildRAR4([]rarEntry{{name: "a/b.txt", data: []byte("hello")}, {name: "../x", data: []byte("y")}}, rarSpec{})[0])
	f.Add(uint8(0), buildRAR5([]rarEntry{{name: "a/b.txt", data: []byte("hello")}, {name: "l", data: []byte("/etc"), link: linkUnixSymlink}}, rarSpec{})[0])
	f.Add(uint8(0), buildRAR5([]rarEntry{{name: "e", data: []byte("secret"), password: "pw"}}, rarSpec{})[0])
	for _, n := range []string{"t0.7z", "t1.7z", "t2.7z", "t5.7z", "empty.7z"} {
		if b, err := os.ReadFile(filepath.Join("testdata", "7z", n)); err == nil {
			f.Add(uint8(1), b)
		}
	}
	if b, err := os.ReadFile(filepath.Join("testdata", "rar", "container.rar")); err == nil {
		f.Add(uint8(0), b)
	}
	f.Add(uint8(1), []byte("PK\x03\x04"))
	work := f.TempDir()
	z := tooltest.Path(f, "7zz")
	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		file := filepath.Join(work, "in.bin")
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		set := ArchiveSet{Format: FormatRAR, Name: "in", Volumes: []string{file}}
		if which%2 == 1 {
			set.Format = Format7z
		}
		dst := filepath.Join(work, "jail", "out")
		_ = os.RemoveAll(filepath.Dir(dst))
		defer func() { _ = os.RemoveAll(filepath.Dir(dst)) }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		res, err := Unpack(ctx, set, dst, WithSevenZip(z), WithLogger(nil), WithPassword("pw"), WithMaxBytes(1<<24),
			WithFreeSpace(func(string) (uint64, error) { return 1 << 40, nil }))

		if err != nil {
			classified := false
			for _, known := range []error{ErrCorrupt, ErrPassword, ErrUnsupported, ErrPathViolation, ErrLimit, ErrMissingVolume} {
				if errors.Is(err, known) {
					classified = true
				}
			}
			if !classified {
				t.Fatalf("unclassified error: %v", err)
			}
			if _, serr := os.Lstat(dst); serr == nil {
				t.Fatalf("destination left behind after %v", err)
			}
		} else {
			for _, name := range res.Files {
				if _, serr := os.Lstat(filepath.Join(dst, filepath.FromSlash(name))); serr != nil {
					t.Fatalf("listed file %q missing: %v", name, serr)
				}
			}
		}
		if _, serr := os.Lstat(dst + ".tmp"); serr == nil {
			t.Fatal("staging dir left behind")
		}
		for _, got := range children(t, work) {
			if got != "in.bin" && got != "jail" {
				t.Fatalf("work directory holds %q", got)
			}
		}
		if got := children(t, filepath.Dir(dst)); len(got) > 1 {
			t.Fatalf("jail holds %v", got)
		}
	})
}
