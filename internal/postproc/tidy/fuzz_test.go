package tidy_test

import (
	"testing"

	"github.com/jroosing/bitnews/internal/postproc/tidy"
)

// FuzzClean checks that Clean never panics and never returns an empty name
// for a non-empty one, whatever bytes the name holds.
func FuzzClean(f *testing.F) {
	for _, c := range cleanCases {
		f.Add(c.in)
	}
	f.Add("\xff\xfe-NZBGeek")
	f.Fuzz(func(t *testing.T, name string) {
		got := tidy.Clean(name)
		if name != "" && got == "" {
			t.Fatalf("Clean(%q) is empty", name)
		}
		if len(got) > len(name) {
			t.Fatalf("Clean(%q) = %q is longer", name, got)
		}
	})
}

// FuzzVideoExt checks that VideoExt never panics on any bytes and only
// returns a known extension.
func FuzzVideoExt(f *testing.F) {
	f.Add([]byte{0x1a, 0x45, 0xdf, 0xa3})
	f.Add([]byte("\x00\x00\x00\x20ftypqt  "))
	f.Add([]byte("RIFF\x00\x00\x00\x00AVI "))
	f.Add([]byte{0x47})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, head []byte) {
		switch ext := tidy.VideoExt(head); ext {
		case "", ".mkv", ".mp4", ".mov", ".avi", ".wmv", ".mpg", ".ts", ".m2ts":
		default:
			t.Fatalf("VideoExt = %q", ext)
		}
	})
}
