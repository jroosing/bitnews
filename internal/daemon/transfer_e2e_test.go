package daemon_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/core/coretest"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

// rateLimitCalls returns the (down, up) of every SetRateLimits call the fake
// engine has seen, in order.
func rateLimitCalls(f *coretest.FakeTorrentEngine) [][2]int64 {
	var out [][2]int64
	for _, c := range f.Calls() {
		if c.Method == "SetRateLimits" {
			out = append(out, [2]int64{c.Down, c.Up})
		}
	}
	return out
}

func lastRateLimit(f *coretest.FakeTorrentEngine) ([2]int64, bool) {
	calls := rateLimitCalls(f)
	if len(calls) == 0 {
		return [2]int64{}, false
	}
	return calls[len(calls)-1], true
}

// The speed limits saved in the Settings are in force when bitnews starts, and
// a change made on the Speed & queue page reaches the torrent engine at once,
// without a restart. The overall limit caps each protocol.
func TestE2ESpeedLimitsFromTheSettingsReachTheTorrentEngineLive(t *testing.T) {
	e := newE2E(t)
	e.torrents = true
	fake := coretest.NewFakeTorrentEngine()
	e.engine = fake
	s := core.DefaultSettings()
	s.Paths.Usenet = core.Roots{Incomplete: e.incomplete, Complete: e.complete}
	s.Paths.Torrent = core.Roots{Incomplete: e.tincomplete, Complete: e.tcomplete}
	s.Speed = core.SpeedSettings{
		Global:  core.SpeedLimit{DownKBps: 1000},
		Torrent: core.SpeedLimit{DownKBps: 5000, UpKBps: 200},
	}
	require.NoError(t, store.NewSettingsRepo(e.db).Save(context.Background(), s))
	e.start()

	require.Eventually(t, func() bool {
		got, ok := lastRateLimit(fake)
		return ok && got == [2]int64{1000 * 1024, 200 * 1024}
	}, 10*time.Second, 10*time.Millisecond, "the limits of the Settings are in force at start-up: %v", rateLimitCalls(fake))

	w := newWebClient(t, e.ts.URL)
	w.login()
	resp, body := w.hx("POST", "/settings/speed", url.Values{"torrent_down_kbps": {"300"}, "torrent_up_kbps": {"50"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	require.Eventually(t, func() bool {
		got, ok := lastRateLimit(fake)
		return ok && got == [2]int64{300 * 1024, 50 * 1024}
	}, 10*time.Second, 10*time.Millisecond, "the page's change is applied without a restart: %v", rateLimitCalls(fake))

	// Removing the limits lifts them.
	resp, body = w.hx("POST", "/settings/speed", url.Values{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Eventually(t, func() bool {
		got, ok := lastRateLimit(fake)
		return ok && got == [2]int64{0, 0}
	}, 10*time.Second, 10*time.Millisecond)

	// A setting that is not a speed limit leaves them alone.
	before := len(rateLimitCalls(fake))
	resp, body = w.hx("POST", "/settings/speed/space", url.Values{"min_free_gb": {"2"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, rateLimitCalls(fake), before)
}
