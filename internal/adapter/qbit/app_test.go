package qbit_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/qbit"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestVersionEndpointsReturnBareStrings(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	version := s.get(sid, "/api/v2/app/version")
	api := s.get(sid, "/api/v2/app/webapiVersion")

	assert.Equal(t, http.StatusOK, version.Code)
	assert.Equal(t, qbit.DefaultVersion, version.Body.String())
	assert.Regexp(t, `^v\d+\.\d+\.\d+$`, version.Body.String())
	assert.Contains(t, version.Header().Get("Content-Type"), "text/plain")
	assert.Equal(t, qbit.DefaultWebAPIVersion, api.Body.String())
	assert.Regexp(t, `^\d+\.\d+(\.\d+)?$`, api.Body.String())
}

func TestTheVersionsAreConfigurable(t *testing.T) {
	t.Parallel()
	s := newStack(t, withDeps(func(d *qbit.Deps) { d.Version, d.WebAPIVersion = "v5.0.1", "2.11.2" }))
	sid := s.login()

	assert.Equal(t, "v5.0.1", s.get(sid, "/api/v2/app/version").Body.String())
	assert.Equal(t, "2.11.2", s.get(sid, "/api/v2/app/webapiVersion").Body.String())
}

func TestWebAPI211ReportsStoppedInsteadOfPaused(t *testing.T) {
	t.Parallel()
	s := newStack(t, withDeps(func(d *qbit.Deps) { d.WebAPIVersion = "2.11.2" }))
	s.seed("down", func(d *core.Download, _ *torrent) { d.Paused = true })
	s.seed("up", ready("/d/up"), func(_ *core.Download, v *torrent) { v.Paused = true })
	sid := s.login()

	got := s.info(sid, "")

	assert.Equal(t, "stoppedDL", got[0].State)
	assert.Equal(t, "stoppedUP", got[1].State)
}

func TestWebAPIAtLeast(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"2.11": true, "2.11.2": true, "2.9.3": false, "2.10.9": false, "3.0": true, "10.0.0": true, "v2.11": true,
		"2": false, "": false, "x.y": false, "2.x": false,
	} {
		assert.Equal(t, want, qbit.WebAPIAtLeast(v, 2, 11), v)
	}
}

type prefsJSON struct {
	SavePath           string  `json:"save_path"`
	TempPath           string  `json:"temp_path"`
	QueueingEnabled    bool    `json:"queueing_enabled"`
	MaxActiveDownloads int     `json:"max_active_downloads"`
	MaxRatioEnabled    bool    `json:"max_ratio_enabled"`
	MaxRatio           float64 `json:"max_ratio"`
	MaxRatioAct        int     `json:"max_ratio_act"`
	MaxSeedingTimeOn   bool    `json:"max_seeding_time_enabled"`
	MaxSeedingTime     int     `json:"max_seeding_time"`
	DHT                bool    `json:"dht"`
}

func TestPreferencesIsAReadOnlyViewOfTheSettings(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	cur := s.settings.Current()
	cur.Queues.Torrent = 7
	require.NoError(t, s.settings.Update(context.Background(), cur))
	sid := s.login()

	var p prefsJSON
	decode(t, s.get(sid, "/api/v2/app/preferences"), &p)

	assert.Equal(t, s.torrents, p.SavePath)
	assert.Equal(t, s.incomplete, p.TempPath)
	assert.True(t, p.QueueingEnabled)
	assert.Equal(t, 7, p.MaxActiveDownloads)
	assert.True(t, p.DHT)
	assert.False(t, p.MaxRatioEnabled)
	assert.False(t, p.MaxSeedingTimeOn)
	assert.EqualValues(t, -1, p.MaxRatio)
	assert.EqualValues(t, -1, p.MaxSeedingTime)
}

func TestPreferencesReportsTheTorrentNetworkSettings(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set        func(*core.TorrentSettings)
		dht, pex   bool
		lsd        bool
		encryption int
		port       int
	}{
		"defaults": {func(*core.TorrentSettings) {}, true, true, false, 0, core.DefaultTorrentPort},
		"all off": {func(t *core.TorrentSettings) {
			t.DisableDHT, t.DisablePEX, t.MSE, t.ListenPort = true, true, core.MSEDisable, 6881
		}, false, false, false, 2, 6881},
		"required": {func(t *core.TorrentSettings) { t.MSE, t.LSD = core.MSERequire, true }, true, true, true, 1, core.DefaultTorrentPort},
		"prefer":   {func(t *core.TorrentSettings) { t.MSE = core.MSEPrefer }, true, true, false, 0, core.DefaultTorrentPort},
	} {
		s := newStack(t)
		cur := s.settings.Current()
		tc.set(&cur.Torrent)
		require.NoError(t, s.settings.Update(context.Background(), cur))
		sid := s.login()

		var p struct {
			DHT        bool `json:"dht"`
			PeX        bool `json:"pex"`
			LSD        bool `json:"lsd"`
			Encryption int  `json:"encryption"`
			ListenPort int  `json:"listen_port"`
		}
		decode(t, s.get(sid, "/api/v2/app/preferences"), &p)

		assert.Equal(t, tc.dht, p.DHT, name)
		assert.Equal(t, tc.pex, p.PeX, name)
		assert.Equal(t, tc.lsd, p.LSD, name)
		assert.Equal(t, tc.encryption, p.Encryption, name)
		assert.Equal(t, tc.port, p.ListenPort, name)
	}
}

func TestPreferencesReportsTheGlobalShareLimit(t *testing.T) {
	t.Parallel()
	for action, want := range map[string]int{"pause": 0, "remove": 1, "remove_files": 3, "": 0} {
		s := newStack(t, withDeps(func(d *qbit.Deps) {
			d.GlobalShare = func() qbit.GlobalShare {
				return qbit.GlobalShare{RatioEnabled: true, Ratio: 1.5, SeedingTimeEnabled: true, SeedingMinutes: 600, Action: action}
			}
		}))
		sid := s.login()

		var p prefsJSON
		decode(t, s.get(sid, "/api/v2/app/preferences"), &p)

		assert.True(t, p.MaxRatioEnabled)
		assert.Equal(t, 1.5, p.MaxRatio)
		assert.True(t, p.MaxSeedingTimeOn)
		assert.Equal(t, 600, p.MaxSeedingTime)
		assert.Equal(t, want, p.MaxRatioAct, action)
	}
}

func TestSetPreferencesIsAcceptedAndChangesNothing(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	before := s.settings.Current()
	sid := s.login()

	rec := s.post(sid, "/api/v2/app/setPreferences", url.Values{"json": {`{"save_path":"/evil","max_active_downloads":99}`}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, before, s.settings.Current())
}

func TestNewRefusesToRunWithoutTheViewModule(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() { newStack(t, withDeps(func(d *qbit.Deps) { d.View = nil })) })
}
