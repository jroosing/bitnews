package netbind_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jroosing/bitnews/internal/netbind"
)

func TestEnabledOnlyWhenInterfaceOrAddressSet(t *testing.T) {
	assert.False(t, netbind.Binding{}.Enabled())
	assert.True(t, netbind.Binding{Interface: "wg0"}.Enabled())
	assert.True(t, netbind.Binding{Address: "10.0.0.2"}.Enabled())
}

func TestListenAddrUsesBoundAddressOrAllInterfaces(t *testing.T) {
	assert.Equal(t, "10.0.0.2:6881", netbind.Binding{Address: "10.0.0.2"}.ListenAddr(6881))
	assert.Equal(t, "[fd00::2]:0", netbind.Binding{Address: "fd00::2"}.ListenAddr(0))
	assert.Equal(t, ":6881", netbind.Binding{Interface: "wg0"}.ListenAddr(6881))
	assert.Equal(t, ":6881", netbind.Binding{}.ListenAddr(6881))
}

func TestValidateRejectsHostileValues(t *testing.T) {
	assert.NoError(t, netbind.Binding{}.Validate())
	assert.NoError(t, netbind.Binding{Interface: "wg0", Address: "10.0.0.2"}.Validate())
	for _, b := range []netbind.Binding{
		{Interface: "../../etc"},
		{Interface: "wg0\x00evil"},
		{Interface: "a b"},
		{Interface: "averyveryverylongname"},
		{Address: "example.com"},
		{Address: "0.0.0.0"},
		{Address: "10.0.0.2%eth0"},
	} {
		assert.Error(t, b.Validate(), "%+v", b)
	}
}
