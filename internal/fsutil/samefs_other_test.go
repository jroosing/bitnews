//go:build !unix

package fsutil_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

func TestSameFilesystem_UnsupportedOffUnix(t *testing.T) {
	_, err := fsutil.SameFilesystem(t.TempDir(), t.TempDir())
	assert.ErrorIs(t, err, fsutil.ErrUnsupported)
}
