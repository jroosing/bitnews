package tidy_test

import (
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/postproc/tidy"
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
