package fsutil_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func FuzzSafeName(f *testing.F) {
	for _, seed := range []string{
		"", ".", "..", "...", "a/b", `a\b`, "../../etc/passwd", "a\x00b", "movie.mkv",
		"CON", "nul.txt", "COM1.log", "a\xffb", "x\u202eexe.txt", "\u200b", " . ",
		strings.Repeat("a", 300) + ".mkv", strings.Repeat("日", 200) + ".mkv",
		"a:b*c?d\"e<f>g|h", "trailing. . ", ".hidden", "a." + strings.Repeat("b", 300),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := fsutil.SafeName(in)
		if err != nil {
			if got != "" {
				t.Fatalf("SafeName(%q) returned %q with error %v", in, got, err)
			}
			return
		}
		switch {
		case got == "" || got == "." || got == "..":
			t.Fatalf("SafeName(%q) = %q", in, got)
		case len(got) > 255:
			t.Fatalf("SafeName(%q) is %d bytes", in, len(got))
		case !utf8.ValidString(got):
			t.Fatalf("SafeName(%q) = %q is not valid UTF-8", in, got)
		case strings.ContainsAny(got, "/\\\x00:*?\"<>|"):
			t.Fatalf("SafeName(%q) = %q contains a forbidden character", in, got)
		case strings.HasSuffix(got, " ") || strings.HasSuffix(got, ".") || strings.HasPrefix(got, " "):
			t.Fatalf("SafeName(%q) = %q has leading/trailing space or trailing dot", in, got)
		case filepath.Base(got) != got || filepath.Clean(got) != got:
			t.Fatalf("SafeName(%q) = %q is not a single path component", in, got)
		}
		for _, r := range got {
			if unicode.In(r, unicode.C, unicode.Zl, unicode.Zp) {
				t.Fatalf("SafeName(%q) = %q contains control rune %U", in, got, r)
			}
		}
	})
}

func FuzzSafeJoin(f *testing.F) {
	for _, seed := range []string{
		"", ".", "..", "../x", "a/../../x", "/etc/passwd", `..\x`, "a/b/c", "a\x00b",
		"link", "link/x", "inside/new/file", "dangling/x", "dir/../link/x", "//host/share",
		"a/./b//c/", `a\..\..\b`,
	} {
		f.Add(seed)
	}

	base := f.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "dir"), filepath.Join(root, "inside"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			f.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		for name, target := range map[string]string{
			"link":     outside,
			"dangling": filepath.Join(base, "missing"),
		} {
			if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
				f.Fatal(err)
			}
		}
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, rel string) {
		got, err := fsutil.SafeJoin(root, rel)
		if err != nil {
			if got != "" {
				t.Fatalf("SafeJoin(%q) returned %q with error %v", rel, got, err)
			}
			return
		}
		// Lexical containment: root is a path prefix and no ".." survives.
		r, err := filepath.Rel(root, got)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			t.Fatalf("SafeJoin(%q) = %q escapes root (rel %q, err %v)", rel, got, r, err)
		}
		for part := range strings.SplitSeq(filepath.ToSlash(r), "/") {
			if part == ".." {
				t.Fatalf("SafeJoin(%q) = %q contains ..", rel, got)
			}
		}
		if strings.IndexByte(got, 0) >= 0 {
			t.Fatalf("SafeJoin(%q) = %q contains NUL", rel, got)
		}
		// Real containment: whatever exists of the result resolves inside root.
		anc := got
		for {
			if _, err := os.Lstat(anc); err == nil {
				break
			}
			parent := filepath.Dir(anc)
			if parent == anc {
				return
			}
			anc = parent
		}
		real, err := filepath.EvalSymlinks(anc)
		if err != nil {
			t.Fatalf("SafeJoin(%q) = %q accepted unresolvable %q: %v", rel, got, anc, err)
		}
		rr, err := filepath.Rel(realRoot, real)
		if err != nil || rr == ".." || strings.HasPrefix(rr, ".."+string(filepath.Separator)) {
			t.Fatalf("SafeJoin(%q) = %q resolves to %q outside %q", rel, got, real, realRoot)
		}
	})
}

func FuzzWithin(f *testing.F) {
	for _, s := range [][2]string{
		{"/a", "/a"}, {"/a", "/ab"}, {"/a", "/a/b"}, {"/a/", "/a/../b"}, {"/", "/x"},
		{"a", "a/b"}, {"/a", "a"}, {"/a", "/a/..b"}, {"", ""}, {"/a\x00", "/a\x00/b"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, root, p string) {
		w, in := fsutil.Within(root, p), fsutil.Inside(root, p)
		if !w {
			if in {
				t.Fatalf("Inside(%q, %q) without Within", root, p)
			}
			return
		}
		cr, cp := filepath.Clean(root), filepath.Clean(p)
		if in != (cr != cp) {
			t.Fatalf("Within(%q, %q): Inside=%v, but clean paths equal=%v", root, p, in, cr == cp)
		}
		// Within means p is root or root plus a separator-bounded tail.
		if cr != cp && cr != string(filepath.Separator) && cr != "." &&
			!strings.HasPrefix(cp, cr+string(filepath.Separator)) {
			t.Fatalf("Within(%q, %q) but %q is not below %q", root, p, cp, cr)
		}
		if filepath.IsAbs(root) != filepath.IsAbs(p) {
			t.Fatalf("Within(%q, %q) mixes absolute and relative", root, p)
		}
	})
}
