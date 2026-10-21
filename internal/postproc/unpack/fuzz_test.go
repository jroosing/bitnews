package unpack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
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

// fuzzUnpackAllocBudget is what one Unpack of a small input may allocate in
// total. The inputs are at most 64 KiB, so anything near it is a declared size
// the archive controls.
const fuzzUnpackAllocBudget = 128 << 20

// FuzzUnpack feeds arbitrary bytes to the RAR and 7z readers. Whatever the
// input, Unpack must not panic, must fail with a classified error, and must
// leave neither a staging dir nor a destination behind when it fails.
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
	for _, seed := range hostile7zSeeds() {
		f.Add(uint8(1), seed)
	}
	work := f.TempDir()
	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		defer func() {
			runtime.ReadMemStats(&after)
			if grew := after.TotalAlloc - before.TotalAlloc; grew > fuzzUnpackAllocBudget {
				t.Errorf("an input of %d bytes made Unpack allocate %d MiB", len(data), grew>>20)
			}
		}()
		file := filepath.Join(work, "in.bin")
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		set := ArchiveSet{Format: FormatRAR, Name: "in", Volumes: []string{file}}
		if which%2 == 1 {
			set.Format = Format7z
		}
		dst := filepath.Join(work, "out")
		_ = os.RemoveAll(dst)
		defer func() { _ = os.RemoveAll(dst); _ = os.RemoveAll(dst + ".tmp") }()

		_, err := Unpack(context.Background(), set, dst,
			WithLogger(nil), WithPassword("pw"), WithMaxBytes(1<<24), WithMaxDictionary(1<<22),
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
		}
		if _, serr := os.Lstat(dst + ".tmp"); serr == nil {
			t.Fatal("staging dir left behind")
		}
	})
}
