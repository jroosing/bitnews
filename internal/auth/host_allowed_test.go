package auth_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/auth"
)

func TestHostAllowedIgnoresPortCaseAndTrailingDot(t *testing.T) {
	list := []string{"Bitnews.LAN", "192.168.1.10:9494", "[::1]"}

	assert.True(t, auth.HostAllowed("bitnews.lan:9494", list))
	assert.True(t, auth.HostAllowed("BITNEWS.lan.", list))
	assert.True(t, auth.HostAllowed("192.168.1.10", list))
	assert.True(t, auth.HostAllowed("[::1]:80", list))
	assert.False(t, auth.HostAllowed("evil.example", list))
	assert.False(t, auth.HostAllowed("", list))
	assert.False(t, auth.HostAllowed("bitnews.lan", nil))
}
