package tidy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/postproc/tidy"
)

func TestCleanGivesWhatCleanPyGives(t *testing.T) {
	for _, c := range cleanCases {
		want := c.want
		if want == "" {
			want = c.in // the script would print an empty name; a Download needs one
		}
		assert.Equal(t, want, tidy.Clean(c.in), c.in)
	}
}

func TestCleanKeepsTheOriginalWhenNothingIsLeft(t *testing.T) {
	// Clean.py would print an empty name here; a Download needs one.
	assert.Equal(t, "-NZBGeek", tidy.Clean("-NZBGeek"))
	assert.Equal(t, "", tidy.Clean(""))
}
