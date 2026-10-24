package core_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

func TestDefaultSettingsHaveNoSpeedLimitAndSaneTorrentAndDiskDefaults(t *testing.T) {
	s := core.DefaultSettings()

	require.NoError(t, s.Validate())
	assert.Equal(t, core.SpeedSettings{}, s.Speed, "no limit unless the user sets one")
	assert.Equal(t, core.DefaultTorrentPort, s.Torrent.EffectivePort())
	assert.True(t, s.Torrent.DHTEnabled())
	assert.True(t, s.Torrent.PEXEnabled())
	assert.Equal(t, core.MSEPrefer, s.Torrent.EffectiveMSE())
	assert.Equal(t, 1, s.Queues.EffectivePostprocConcurrency())
	assert.Equal(t, core.DefaultMinFree, s.Queues.EffectiveMinFree())
}

func TestZeroValuesOfTheNewSettingsAreTheDefaults(t *testing.T) {
	// A PUT from a client that does not know these keys decodes them to zero.
	var s core.Settings

	assert.Equal(t, core.DefaultTorrentPort, s.Torrent.EffectivePort())
	assert.True(t, s.Torrent.DHTEnabled())
	assert.True(t, s.Torrent.PEXEnabled())
	assert.False(t, s.Torrent.LSD)
	assert.Equal(t, core.MSEPrefer, s.Torrent.EffectiveMSE())
	assert.Equal(t, 1, s.Queues.EffectivePostprocConcurrency())
	assert.Equal(t, core.DefaultMinFree, s.Queues.EffectiveMinFree())
}

func TestEffectiveSpeedIsTheTighterOfTheOverallAndTheProtocolLimit(t *testing.T) {
	s := core.SpeedSettings{
		Global:  core.SpeedLimit{DownKBps: 1000, UpKBps: 0},
		Usenet:  core.SpeedLimit{DownKBps: 5000},
		Torrent: core.SpeedLimit{DownKBps: 200, UpKBps: 50},
	}

	down, up := s.Effective(core.SourceNZB)
	assert.Equal(t, int64(1000*1024), down, "the overall limit is tighter")
	assert.Zero(t, up)

	down, up = s.Effective(core.SourceTorrent)
	assert.Equal(t, int64(200*1024), down, "the protocol limit is tighter")
	assert.Equal(t, int64(50*1024), up, "only the protocol sets an upload limit")

	down, up = core.SpeedSettings{}.Effective(core.SourceTorrent)
	assert.Zero(t, down)
	assert.Zero(t, up, "zero means unlimited")
}

func TestUsenetNeverUploads(t *testing.T) {
	s := core.SpeedSettings{Global: core.SpeedLimit{UpKBps: 10}}

	_, up := s.Effective(core.SourceNZB)

	assert.Zero(t, up)
}

func TestNewSettingsValidate(t *testing.T) {
	for name, mod := range map[string]func(*core.Settings){
		"negative overall download": func(s *core.Settings) { s.Speed.Global.DownKBps = -1 },
		"negative torrent upload":   func(s *core.Settings) { s.Speed.Torrent.UpKBps = -5 },
		"absurd usenet download":    func(s *core.Settings) { s.Speed.Usenet.DownKBps = core.MaxSpeedKBps + 1 },
		"usenet upload limit":       func(s *core.Settings) { s.Speed.Usenet.UpKBps = 10 },
		"negative port":             func(s *core.Settings) { s.Torrent.ListenPort = -1 },
		"port above 65535":          func(s *core.Settings) { s.Torrent.ListenPort = 65536 },
		"unknown MSE policy":        func(s *core.Settings) { s.Torrent.MSE = "maybe" },
		"negative post-processing":  func(s *core.Settings) { s.Queues.PostprocConcurrency = -1 },
		"too many post-processing":  func(s *core.Settings) { s.Queues.PostprocConcurrency = core.MaxPostprocConcurrency + 1 },
		"negative reserve":          func(s *core.Settings) { s.Queues.MinFree = -1 },
		"absurd reserve":            func(s *core.Settings) { s.Queues.MinFree = core.MaxMinFree + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			s := core.DefaultSettings()
			mod(&s)
			assert.ErrorIs(t, s.Validate(), core.ErrInvalidSettings)
		})
	}
}

func TestNewSettingsAcceptTheirLimits(t *testing.T) {
	s := core.DefaultSettings()
	s.Speed = core.SpeedSettings{
		Global:  core.SpeedLimit{DownKBps: core.MaxSpeedKBps, UpKBps: 1},
		Torrent: core.SpeedLimit{DownKBps: 1, UpKBps: core.MaxSpeedKBps},
	}
	s.Torrent = core.TorrentSettings{ListenPort: 65535, DisableDHT: true, DisablePEX: true, LSD: true, MSE: core.MSERequire}
	s.Queues.PostprocConcurrency = core.MaxPostprocConcurrency
	s.Queues.MinFree = core.MaxMinFree

	require.NoError(t, s.Validate())
	assert.False(t, s.Torrent.DHTEnabled())
	assert.False(t, s.Torrent.PEXEnabled())
	assert.Equal(t, core.MSERequire, s.Torrent.EffectiveMSE())
	assert.Equal(t, uint64(core.MaxMinFree), s.Queues.EffectiveMinFree())
}

func TestNewSettingsJSONKeys(t *testing.T) {
	s := core.DefaultSettings()
	s.Speed.Torrent.DownKBps = 7
	s.Torrent.ListenPort = 6881
	s.Queues.PostprocConcurrency = 2
	s.Queues.MinFree = 1 << 30

	raw, err := json.Marshal(s)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	queues := m["queues"].(map[string]any)
	assert.Equal(t, float64(2), queues["postproc_concurrency"])
	assert.Equal(t, float64(1<<30), queues["min_free"])
	assert.Equal(t, map[string]any{"disable_dht": false, "disable_pex": false, "lsd": false, "listen_port": float64(6881), "mse": ""}, m["torrent"])
	assert.Equal(t, float64(7), m["speed"].(map[string]any)["torrent"].(map[string]any)["down_kbps"])
}
