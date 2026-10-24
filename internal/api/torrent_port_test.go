package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/api"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// fakePort is an api.TorrentPort that remembers the port it was given.
type fakePort struct {
	mu  sync.Mutex
	st  core.ListenPortStatus
	err error
}

func (f *fakePort) SetListenPort(_ context.Context, port int) (core.ListenPortStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return core.ListenPortStatus{}, f.err
	}
	f.st = core.ListenPortStatus{Port: port, Since: epoch}
	return f.st, nil
}

func (f *fakePort) ListenPortStatus() (core.ListenPortStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, true
}

func withPort(p api.TorrentPort) envOption {
	return withConfig(func(c *api.Config) { c.TorrentPort = p })
}

type portBody struct {
	Port         int        `json:"port"`
	Open         bool       `json:"open"`
	Since        time.Time  `json:"since"`
	LastIncoming *time.Time `json:"last_incoming"`
}

func TestSetTheTorrentPortWithTheAPIKey(t *testing.T) {
	t.Parallel()
	port := &fakePort{}
	e := newEnv(t, withPort(port))

	rec := e.do("POST", "/api/v1/torrent/port", `{"port":45678}`, withHeader("X-Api-Key", e.apiKey()))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out portBody
	decode(t, rec, &out)
	assert.Equal(t, 45678, out.Port)
	assert.False(t, out.Open)
	assert.Nil(t, out.LastIncoming)
	st, _ := port.ListenPortStatus()
	assert.Equal(t, 45678, st.Port)
}

func TestGetTheTorrentPortSaysWhetherItIsOpen(t *testing.T) {
	t.Parallel()
	seen := epoch.Add(time.Minute)
	port := &fakePort{st: core.ListenPortStatus{Port: 51413, Since: epoch, LastIncoming: seen}}
	e := newEnv(t, withPort(port))

	rec := e.do("GET", "/api/v1/torrent/port", "", withBearer(e.apiKey()))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out portBody
	decode(t, rec, &out)
	assert.Equal(t, 51413, out.Port)
	assert.True(t, out.Open)
	require.NotNil(t, out.LastIncoming)
	assert.True(t, seen.Equal(*out.LastIncoming))
}

func TestSetTheTorrentPortNeedsAuthentication(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withPort(&fakePort{}))

	rec := e.do("POST", "/api/v1/torrent/port", `{"port":45678}`)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestSetTheTorrentPortRejectsBadBodies(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withPort(&fakePort{}))
	key := withBearer(e.apiKey())

	for _, body := range []string{`{"port":0}`, `{"port":65536}`, `{"port":-5}`, `{"port":"x"}`, `{}`, `{"port":1,"extra":2}`} {
		rec := e.do("POST", "/api/v1/torrent/port", body, key)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
}

func TestSetTheTorrentPortPassesServiceErrorsOn(t *testing.T) {
	t.Parallel()
	port := &fakePort{err: &app.Error{Kind: core.ErrConflict, Code: "port_fixed", Message: "fixed by BITNEWS_TORRENT_PORT"}}
	e := newEnv(t, withPort(port))

	rec := e.do("POST", "/api/v1/torrent/port", `{"port":45678}`, withBearer(e.apiKey()))

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "port_fixed")
}

func TestTheTorrentPortIsUnavailableWithoutTorrents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := withBearer(e.apiKey())

	assert.Equal(t, http.StatusServiceUnavailable, e.do("GET", "/api/v1/torrent/port", "", key).Code)
	assert.Equal(t, http.StatusServiceUnavailable, e.do("POST", "/api/v1/torrent/port", `{"port":45678}`, key).Code)
}
