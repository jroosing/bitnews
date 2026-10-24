package web_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

func TestTheTorrentQueueShowsAnOpenPort(t *testing.T) {
	st := core.ListenPortStatus{Port: 45678, Since: time.Now().Add(-time.Hour), LastIncoming: time.Now().Add(-3 * time.Minute)}
	q := newQueueEnv(t, withQueueConfig(&fakeConfig{port: &st}))

	doc := queueDoc(t, q)

	badge := byID(doc, "torrent-port")
	require.NotNil(t, badge)
	assert.Equal(t, "open", attr(badge, "data-state"))
	assert.Contains(t, text(badge), "Port 45678 open")
	info := byID(doc, "torrent-port-info")
	require.NotNil(t, info)
	assert.Contains(t, text(info), "only needed for seeding")
	assert.Contains(t, text(info), "3 minutes ago")
}

func TestTheTorrentQueueShowsAnUnconfirmedPort(t *testing.T) {
	st := core.ListenPortStatus{Port: 51413, Since: time.Now()}
	q := newQueueEnv(t, withQueueConfig(&fakeConfig{port: &st}))

	doc := queueDoc(t, q)

	badge := byID(doc, "torrent-port")
	require.NotNil(t, badge)
	assert.Equal(t, "unconfirmed", attr(badge, "data-state"))
	assert.Contains(t, text(badge), "Port 51413 not confirmed")
	assert.Contains(t, text(byID(doc, "torrent-port-info")), "No peer has connected in")
}

func TestTheTorrentQueueShowsNoPortWithoutTorrents(t *testing.T) {
	q := newQueueEnv(t, withQueueConfig(&fakeConfig{}))

	assert.Nil(t, byID(queueDoc(t, q), "torrent-port"))
}

func TestTheTorrentSettingsShowThePortStatus(t *testing.T) {
	st := core.ListenPortStatus{Port: 45678, Since: time.Now(), LastIncoming: time.Now()}
	_, b := settingsSignedIn(t, withConfig(&fakeConfig{started: core.DefaultSettings(), port: &st}))

	doc := parseHTML(t, b.get("/settings/torrent").Body.String())

	line := byID(doc, "torrent-port-status")
	require.NotNil(t, line)
	assert.Equal(t, "open", attr(line, "data-state"))
	assert.Contains(t, text(line), "45678")
	assert.Contains(t, text(byID(doc, "connection-card")), "only needed for seeding")
}
