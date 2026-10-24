// Package torrenttest builds small torrents and local swarms for tests of
// code above the torrent engine, which must not import anacrolix themselves.
// Everything stays on 127.0.0.1: TCP only, no DHT, no trackers.
package torrenttest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/torrent"
)

// PieceLength is the piece length of the torrents Make builds.
const PieceLength = 16 << 10

// Content is a small torrent built in a test, with the directory that holds
// its data so a seeder can serve it.
type Content struct {
	Name    string
	Files   map[string][]byte // slash path relative to the torrent root; "" for a single-file torrent
	Torrent []byte            // the .torrent file
	Hash    core.InfoHash
	SeedDir string // storage dir that already holds <Name>/...
}

// Options tune Make.
type Options struct {
	Private bool
}

// MakeRandom builds a torrent of random files of the given sizes.
func MakeRandom(t testing.TB, name string, sizes map[string]int, opts ...Options) Content {
	t.Helper()
	rng := rand.New(rand.NewChaCha8([32]byte{byte(len(name)), byte(len(sizes))}))
	files := make(map[string][]byte, len(sizes))
	for rel, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		files[rel] = data
	}
	return Make(t, name, files, opts...)
}

// Make writes files below <tmp>/seed/<name> and builds the torrent for them.
// A single entry with an empty path makes a single-file torrent.
func Make(t testing.TB, name string, files map[string][]byte, opts ...Options) Content {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	seedDir := filepath.Join(t.TempDir(), "seed")
	root := filepath.Join(seedDir, name)
	for rel, data := range files {
		path := root
		if rel != "" {
			path = filepath.Join(root, filepath.FromSlash(rel))
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o644))
	}
	info := metainfo.Info{PieceLength: PieceLength}
	require.NoError(t, info.BuildFromFilePath(root))
	if o.Private {
		yes := true
		info.Private = &yes
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	raw, err := bencode.Marshal(mi)
	require.NoError(t, err)
	return Content{Name: name, Files: files, Torrent: raw, Hash: core.InfoHash(mi.HashInfoBytes()), SeedDir: seedDir}
}

// Magnet returns a magnet link for the content with the given peers as
// x.pe hints.
func (c Content) Magnet(peers ...string) string {
	var m strings.Builder
	m.WriteString("magnet:?xt=urn:btih:" + c.Hash.String() + "&dn=display+name")
	for _, p := range peers {
		m.WriteString("&x.pe=" + p)
	}
	return m.String()
}

// Size is the total size of the torrent's files.
func (c Content) Size() int64 {
	var n int64
	for _, d := range c.Files {
		n += int64(len(d))
	}
	return n
}

// AssertFilesEqual checks that dir/<Name>/... holds exactly the content.
func (c Content) AssertFilesEqual(t testing.TB, dir string) {
	t.Helper()
	for rel, want := range c.Files {
		path := filepath.Join(dir, c.Name)
		if rel != "" {
			path = filepath.Join(path, filepath.FromSlash(rel))
		}
		got, err := os.ReadFile(path)
		require.NoError(t, err, rel)
		require.True(t, bytes.Equal(want, got), "file %q differs", rel)
	}
}

// MemPieces is an in-memory torrent.PieceStore.
type MemPieces struct {
	mu   sync.Mutex
	rows map[core.InfoHash]store.TorrentPieces
}

// NewMemPieces returns an empty MemPieces.
func NewMemPieces() *MemPieces { return &MemPieces{rows: map[core.InfoHash]store.TorrentPieces{}} }

// Load implements torrent.PieceStore.
func (m *MemPieces) Load(_ context.Context, h core.InfoHash) (store.TorrentPieces, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[h]
	if !ok {
		return store.TorrentPieces{}, core.ErrNotFound
	}
	return p, nil
}

// Save implements torrent.PieceStore.
func (m *MemPieces) Save(_ context.Context, h core.InfoHash, p store.TorrentPieces) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[h] = p
	return nil
}

// Delete implements torrent.PieceStore.
func (m *MemPieces) Delete(_ context.Context, h core.InfoHash) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, h)
	return nil
}

// Config returns the engine config of a local-swarm peer: loopback, TCP only,
// no DHT, an in-memory piece store, and a random port.
func Config() torrent.Config {
	return torrent.Config{
		BindAddr: "127.0.0.1", PieceStore: NewMemPieces(), ShareLimitInterval: time.Hour, DisableUTP: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// NewEngine starts a loopback engine that is closed when the test ends.
func NewEngine(t testing.TB, mods ...func(*torrent.Config)) *torrent.Engine {
	t.Helper()
	cfg := Config()
	for _, m := range mods {
		m(&cfg)
	}
	e, err := torrent.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// Addr is the loopback address other engines can dial.
func Addr(e *torrent.Engine) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(e.Port())) }

// Seed starts an engine that seeds c from its SeedDir and returns once it
// has verified all pieces.
func Seed(t testing.TB, c Content, mods ...func(*torrent.Config)) *torrent.Engine {
	t.Helper()
	e := NewEngine(t, mods...)
	h, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: c.Torrent, StorageDir: c.SeedDir})
	require.NoError(t, err)
	require.Equal(t, c.Hash, h)
	WaitEvent(t, e, core.TorrentFetched, h)
	return e
}

// WaitEvent returns once e emits an event of kind for h, failing after 30 s.
// Other events are skipped.
func WaitEvent(t testing.TB, e *torrent.Engine, kind core.TorrentEventKind, h core.InfoHash) core.TorrentEvent {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-e.Events():
			require.True(t, ok, "events closed while waiting for %s", kind)
			if ev.Kind == kind && ev.Hash == h {
				return ev
			}
		case <-timeout:
			require.FailNow(t, "timed out waiting for event", "%s of %s", kind, h)
		}
	}
}
