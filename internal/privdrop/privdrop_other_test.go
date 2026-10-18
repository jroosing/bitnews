//go:build !linux && !windows

package privdrop_test

import (
	"gitlab.com/kyura4k/gitnews/internal/privdrop"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOSCannotChangeIdsOutsideLinux(t *testing.T) {
	sys := privdrop.OS()

	assert.ErrorContains(t, sys.Setuid(1), "only supported on Linux")
	assert.ErrorContains(t, sys.Setgid(1), "only supported on Linux")
	assert.ErrorContains(t, sys.Setgroups([]int{1}), "only supported on Linux")
}
