package core_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

func TestNetworkBindingIsOffByDefault(t *testing.T) {
	n := core.DefaultSettings().Network

	assert.Equal(t, core.NetworkSettings{}, n)
	assert.NoError(t, core.DefaultSettings().Validate())
}

func TestNetworkBindingAcceptsValidInterfaceAndAddress(t *testing.T) {
	for name, b := range map[string]core.ProtocolBinding{
		"interface only": {Interface: "wg0"},
		"address only":   {Address: "10.2.0.2"},
		"both":           {Interface: "tun0", Address: "10.8.0.6"},
		"ipv6":           {Address: "fd00::2"},
		"vlan name":      {Interface: "eth0.100"},
		"max length":     {Interface: "abcdefghijklmno"},
		"alias":          {Interface: "eth0:1"},
		"dash underline": {Interface: "br-lan_1"},
	} {
		s := core.DefaultSettings()
		s.Network = core.NetworkSettings{Usenet: b, Torrent: b}
		assert.NoError(t, s.Validate(), name)
	}
}

func TestNetworkBindingRejectsInvalidValues(t *testing.T) {
	for name, b := range map[string]core.ProtocolBinding{
		"interface too long":      {Interface: "abcdefghijklmnop"},
		"interface with space":    {Interface: "wg 0"},
		"interface with slash":    {Interface: "../wg0"},
		"interface with newline":  {Interface: "wg0\n"},
		"interface non-ascii":     {Interface: "wgé"},
		"interface with nul":      {Interface: "wg0\x00"},
		"interface whitespace":    {Interface: " "},
		"address host name":       {Address: "localhost"},
		"address with port":       {Address: "10.0.0.1:80"},
		"address with zone":       {Address: "fe80::1%eth0"},
		"address unspecified v4":  {Address: "0.0.0.0"},
		"address unspecified v6":  {Address: "::"},
		"address multicast":       {Address: "224.0.0.1"},
		"address multicast v6":    {Address: "ff02::1"},
		"address with space":      {Address: " 10.0.0.1"},
		"address cidr":            {Address: "10.0.0.0/8"},
		"address broadcast style": {Address: "10.0.0"},
	} {
		for _, proto := range []string{"usenet", "torrent"} {
			s := core.DefaultSettings()
			if proto == "usenet" {
				s.Network.Usenet = b
			} else {
				s.Network.Torrent = b
			}
			err := s.Validate()
			require.Error(t, err, "%s %s", proto, name)
			assert.True(t, errors.Is(err, core.ErrInvalidSettings), "%s %s", proto, name)
		}
	}
}

func TestNetworkSettingsJSONShape(t *testing.T) {
	s := core.DefaultSettings()
	s.Network.Usenet = core.ProtocolBinding{Interface: "wg0", Address: "10.2.0.2"}
	s.Network.Torrent = core.ProtocolBinding{Interface: "wg1"}

	raw, err := json.Marshal(s)
	require.NoError(t, err)
	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	assert.Equal(t, map[string]any{
		"usenet":  map[string]any{"interface": "wg0", "address": "10.2.0.2"},
		"torrent": map[string]any{"interface": "wg1", "address": ""},
	}, generic["network"])

	got := core.DefaultSettings()
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, s, got)
}

func TestSettingsWithoutNetworkKeyLoadWithBindingOff(t *testing.T) {
	got := core.DefaultSettings()
	require.NoError(t, json.Unmarshal([]byte(`{"queues":{"usenet":2,"torrent":3}}`), &got))

	assert.Equal(t, core.NetworkSettings{}, got.Network)
	assert.False(t, got.Network.Usenet.Enabled())
	assert.False(t, got.Network.Torrent.Enabled())
}

func TestProtocolBindingEnabled(t *testing.T) {
	assert.False(t, core.ProtocolBinding{}.Enabled())
	assert.True(t, core.ProtocolBinding{Interface: "wg0"}.Enabled())
	assert.True(t, core.ProtocolBinding{Address: "10.0.0.2"}.Enabled())
}

func TestNetworkValidationNamesTheOffendingField(t *testing.T) {
	s := core.DefaultSettings()
	s.Network.Torrent.Interface = "bad name"

	err := s.Validate()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "network.torrent.interface")
}

func FuzzValidInterfaceName(f *testing.F) {
	for _, s := range []string{"", "wg0", "../x", "wg0\x00", "a b", "abcdefghijklmnop", "é"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if core.ValidInterfaceName(name) != nil {
			return
		}
		if len(name) == 0 || len(name) > core.MaxInterfaceNameLen {
			t.Fatalf("accepted %q", name)
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' || c == '-'
			if !ok {
				t.Fatalf("accepted %q", name)
			}
		}
	})
}
