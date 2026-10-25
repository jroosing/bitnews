package fsutil_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func TestWithinAndInside(t *testing.T) {
	cases := []struct {
		root, p        string
		within, inside bool
	}{
		{"/a", "/a", true, false},
		{"/a", "/a/", true, false},
		{"/a/", "/a", true, false},
		{"/a", "/a/b", true, true},
		{"/a/", "/a/b/", true, true},
		{"/a", "/a/b/c", true, true},
		{"/a", "/ab", false, false},
		{"/a", "/ab/c", false, false},
		{"/a/b", "/a", false, false},
		{"/a", "/b", false, false},
		{"/a", "/a/..", false, false},
		{"/a", "/a/../b", false, false},
		{"/a", "/a/b/..", true, false},
		{"/a", "/a/./b", true, true},
		{"/a", "/a/..b", true, true},
		{"/a", "/a//b", true, true},
		{"/", "/", true, false},
		{"/", "/a", true, true},
		{"/", "/a/b", true, true},
		{"/a", "a/b", false, false},
		{"a", "/a/b", false, false},
		{"a", "a/b", true, true},
		{"a", "a", true, false},
		{"a", "../a", false, false},
	}
	for _, c := range cases {
		assert.Equal(t, c.within, fsutil.Within(c.root, c.p), "Within(%q, %q)", c.root, c.p)
		assert.Equal(t, c.inside, fsutil.Inside(c.root, c.p), "Inside(%q, %q)", c.root, c.p)
	}
}
