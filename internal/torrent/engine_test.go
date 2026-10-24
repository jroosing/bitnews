package torrent_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent"
)

func TestNewRejectsInvalidConfig(t *testing.T) {
	_, err := torrent.New(torrent.Config{})
	assert.ErrorIs(t, err, torrent.ErrInvalidConfig)
}

func TestNewFailsWhenThePortIsTaken(t *testing.T) {
	first := newEngine(t)
	_, err := torrent.New(torrent.Config{BindAddr: "127.0.0.1", PieceStore: newMemStore(), ListenPort: portOf(first)})
	assert.Error(t, err)
}

func portOf(e *torrent.Engine) int { return e.LocalPort() }

func TestEngineAfterCloseReturnsErrClosed(t *testing.T) {
	c := makeContent(t, "Closed", map[string]int{"f": 1000})
	e := newEngine(t)
	h, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: c.seedDir})
	require.NoError(t, err)
	require.NoError(t, e.Close())
	require.NoError(t, e.Close(), "closing twice is fine")

	_, err = e.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: c.seedDir})
	assert.ErrorIs(t, err, torrent.ErrClosed)
	_, err = e.Stats(h)
	assert.ErrorIs(t, err, torrent.ErrClosed)
	assert.ErrorIs(t, e.Pause(h), torrent.ErrClosed)
	assert.ErrorIs(t, e.SetRateLimits(1, 1), torrent.ErrClosed)
}

func TestRelocateNeedsMetadata(t *testing.T) {
	c := makeContent(t, "NoMeta", map[string]int{"f": 1000})
	e := newEngine(t)
	h, err := e.Add(context.Background(), core.TorrentSpec{Magnet: magnetFor(c.hash), StorageDir: t.TempDir()})
	require.NoError(t, err)

	err = e.Relocate(context.Background(), h, t.TempDir()+"/x")

	assert.ErrorIs(t, err, torrent.ErrNoMetadata)
}
