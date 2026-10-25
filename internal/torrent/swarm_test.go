package torrent_test

import (
	"bytes"
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent"
)

const pieceLength = 16 << 10

// content is a small torrent built in the test, with the directory that
// holds its data.
type content struct {
	name    string
	files   map[string][]byte // slash path relative to the torrent root
	torrent []byte            // the .torrent file
	hash    core.InfoHash
	seedDir string // storage dir that already holds the data
}

type contentOpts struct {
	private  bool
	trackers []string
}

// makeContent writes random files below <tmp>/seed/<name> and builds the
// torrent for them with anacrolix's metainfo builder. sizes maps a file path
// to its size; a single entry with an empty path makes a single-file torrent.
func makeContent(t *testing.T, name string, sizes map[string]int, opts ...contentOpts) content {
	t.Helper()
	var o contentOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	seedDir := filepath.Join(t.TempDir(), "seed")
	c := content{name: name, files: map[string][]byte{}, seedDir: seedDir}
	rng := rand.New(rand.NewChaCha8([32]byte{byte(len(name)), byte(len(sizes))}))
	root := filepath.Join(seedDir, name)
	for rel, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		path := root
		if rel != "" {
			path = filepath.Join(root, filepath.FromSlash(rel))
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o644))
		c.files[rel] = data
	}
	info := metainfo.Info{PieceLength: pieceLength}
	require.NoError(t, info.BuildFromFilePath(root))
	if o.private {
		yes := true
		info.Private = &yes
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	if len(o.trackers) > 0 {
		mi.AnnounceList = [][]string{o.trackers}
	}
	c.torrent, err = bencode.Marshal(mi)
	require.NoError(t, err)
	c.hash = core.InfoHash(mi.HashInfoBytes())
	return c
}

// assertFilesEqual checks that dir/<name>/... holds exactly the content.
func (c content) assertFilesEqual(t *testing.T, dir string) {
	t.Helper()
	for rel, want := range c.files {
		path := filepath.Join(dir, c.name)
		if rel != "" {
			path = filepath.Join(path, filepath.FromSlash(rel))
		}
		got, err := os.ReadFile(path)
		require.NoError(t, err, rel)
		assert.True(t, bytes.Equal(want, got), "file %q differs", rel)
	}
}

var (
	registryMu sync.Mutex
	registry   = map[*testing.T][]*torrent.Engine{}
	logBufs    = map[*torrent.Engine]*syncBuffer{}
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func dumpEngines(t *testing.T) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, e := range registry[t] {
		t.Logf("engine %d logs:\n%s", i, logBufs[e].String())
		t.Logf("engine %d status:\n%s", i, e.DebugStatus())
	}
}

type engineOpts func(*torrent.Config)

func newEngine(t *testing.T, mods ...engineOpts) *torrent.Engine {
	t.Helper()
	logs := &syncBuffer{}
	cfg := torrent.Config{BindAddr: "127.0.0.1", PieceStore: newMemStore(), DisableUTP: true,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	for _, m := range mods {
		m(&cfg)
	}
	e, err := torrent.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	registryMu.Lock()
	registry[t] = append(registry[t], e)
	logBufs[e] = logs
	registryMu.Unlock()
	t.Cleanup(func() { registryMu.Lock(); delete(registry, t); registryMu.Unlock() })
	return e
}

// seed starts an engine that seeds c from its seedDir.
func seed(t *testing.T, c content, mods ...engineOpts) *torrent.Engine {
	t.Helper()
	e := newEngine(t, mods...)
	h, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: c.seedDir})
	require.NoError(t, err)
	require.Equal(t, c.hash, h)
	waitEvent(t, e, core.TorrentFetched, h)
	return e
}

// waitEvent returns once e emits an event of kind for h, failing after 30 s.
// Other events are skipped.
func waitEvent(t *testing.T, e *torrent.Engine, kind core.TorrentEventKind, h core.InfoHash) core.TorrentEvent {
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
			dumpEngines(t)
			require.FailNow(t, "timed out waiting for event", "%s of %s", kind, h)
		}
	}
}

func fileList(c content) []core.TorrentFile {
	var out []core.TorrentFile
	for rel, data := range c.files {
		p := c.name
		if rel != "" {
			p += "/" + rel
		}
		out = append(out, core.TorrentFile{Path: p, Size: int64(len(data))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func TestSwarmDownloadsAMultiFileTorrentByteExact(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Show.S01", map[string]int{"a.mkv": 100_000, "sub/b.srt": 5_000, "sub/deep/c.bin": 40_001, "empty": 0})
	seeder := seed(t, c)
	leech := newEngine(t)
	dir := t.TempDir()

	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)

	c.assertFilesEqual(t, dir)
	st, err := leech.Stats(h)
	require.NoError(t, err)
	assert.Equal(t, core.TorrentSeeding, st.State)
	assert.Equal(t, "Show.S01", st.Name)
	assert.EqualValues(t, 145_001, st.Size)
	assert.Equal(t, st.Size, st.DoneBytes)
	assert.False(t, st.Private)
	files := st.Files
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	assert.Equal(t, fileList(c), files)
	seedStats, err := seeder.Stats(c.hash)
	require.NoError(t, err)
	assert.Greater(t, seedStats.Uploaded, int64(100_000), "the seeder reports what it sent")
}

func TestSwarmDownloadsASingleFileTorrent(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "movie.mkv", map[string]int{"": 70_000})
	seeder := seed(t, c)
	leech := newEngine(t)
	dir := t.TempDir()

	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)

	c.assertFilesEqual(t, dir)
	st, _ := leech.Stats(h)
	assert.Equal(t, []core.TorrentFile{{Path: "movie.mkv", Size: 70_000}}, st.Files)
}

func magnetFor(h core.InfoHash, peers ...string) string {
	m := "magnet:?xt=urn:btih:" + h.String() + "&dn=display+name"
	for _, p := range peers {
		m += "&x.pe=" + p
	}
	return m
}

func TestSwarmFetchesMetadataFromAMagnetLink(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Show.S02", map[string]int{"ep1.mkv": 50_000, "ep2.mkv": 30_000})
	seeder := seed(t, c)
	leech := newEngine(t)
	dir := t.TempDir()

	h, err := leech.Add(context.Background(), core.TorrentSpec{Magnet: magnetFor(c.hash), StorageDir: dir})
	require.NoError(t, err)
	require.Equal(t, c.hash, h)
	st, err := leech.Stats(h)
	require.NoError(t, err)
	assert.Equal(t, core.TorrentMetadataPending, st.State)
	assert.Zero(t, st.Size)

	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentMetadataReceived, h)
	waitEvent(t, leech, core.TorrentFetched, h)

	c.assertFilesEqual(t, dir)
	st, _ = leech.Stats(h)
	assert.Equal(t, "Show.S02", st.Name)
	assert.EqualValues(t, 80_000, st.Size)
}

func TestSwarmMagnetWithPeerHintFindsTheSeederByItself(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Hinted", map[string]int{"f.bin": 20_000})
	seeder := seed(t, c)
	leech := newEngine(t)
	dir := t.TempDir()

	h, err := leech.Add(context.Background(), core.TorrentSpec{Magnet: magnetFor(c.hash, seeder.ListenAddr()), StorageDir: dir})
	require.NoError(t, err)

	waitEvent(t, leech, core.TorrentFetched, h)
	c.assertFilesEqual(t, dir)
}

func TestAddRejectsDuplicatesAndBadSpecs(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Dup", map[string]int{"f": 1000})
	e := newEngine(t)
	ctx := context.Background()
	dir := t.TempDir()
	_, err := e.Add(ctx, core.TorrentSpec{Metainfo: c.torrent, StorageDir: dir})
	require.NoError(t, err)

	h, err := e.Add(ctx, core.TorrentSpec{Magnet: magnetFor(c.hash), StorageDir: t.TempDir()})
	assert.ErrorIs(t, err, core.ErrConflict, "the same info hash again, even as a magnet")
	assert.Equal(t, c.hash, h)

	for name, spec := range map[string]core.TorrentSpec{
		"empty":              {},
		"relative dir":       {Metainfo: c.torrent, StorageDir: "rel/dir"},
		"garbage metainfo":   {Metainfo: []byte("not bencode"), StorageDir: dir},
		"truncated metainfo": {Metainfo: c.torrent[:len(c.torrent)/2], StorageDir: dir},
		"bad magnet":         {Magnet: "magnet:?dn=nohash", StorageDir: dir},
		"oversized metainfo": {Metainfo: make([]byte, core.MaxMetainfoSize+1), StorageDir: dir},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.Add(ctx, spec)
			assert.Error(t, err)
		})
	}
}

func TestUnknownTorrentIsNotFound(t *testing.T) {
	t.Parallel()
	e := newEngine(t)
	var h core.InfoHash
	h[0] = 9
	assert.ErrorIs(t, e.Pause(h), core.ErrNotFound)
	assert.ErrorIs(t, e.Resume(h), core.ErrNotFound)
	assert.ErrorIs(t, e.Remove(context.Background(), h), core.ErrNotFound)
	assert.ErrorIs(t, e.Relocate(context.Background(), h, t.TempDir()), core.ErrNotFound)
	_, err := e.Stats(h)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestMetainfoOfAMagnetTorrentIsAvailableOnceTheMetadataArrived(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Meta.Show", map[string]int{"ep1.mkv": 20_000})
	seeder := seed(t, c)
	leech := newEngine(t)
	h, err := leech.Add(context.Background(), core.TorrentSpec{Magnet: magnetFor(c.hash), StorageDir: t.TempDir(), Paused: false})
	require.NoError(t, err)

	_, err = leech.Metainfo(h)
	require.Error(t, err, "no metadata yet")

	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentMetadataReceived, h)
	raw, err := leech.Metainfo(h)

	require.NoError(t, err)
	mi, err := metainfo.Load(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, c.hash, core.InfoHash(mi.HashInfoBytes()), "the bytes are a .torrent file of the same torrent")
	// The stored file can be added again.
	again := newEngine(t)
	h2, err := again.Add(context.Background(), core.TorrentSpec{Metainfo: raw, StorageDir: t.TempDir(), Paused: true})
	require.NoError(t, err)
	assert.Equal(t, h, h2)
}

func TestMetainfoOfAnUnknownTorrentIsNotFound(t *testing.T) {
	t.Parallel()
	e := newEngine(t)

	_, err := e.Metainfo(core.InfoHash{1})

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestAddPeersOffersPeersToATorrent(t *testing.T) {
	t.Parallel()
	c := makeContent(t, "Peers", map[string]int{"f.bin": 20_000})
	seeder := seed(t, c)
	leech := newEngine(t)
	h, err := leech.Add(context.Background(), core.TorrentSpec{Metainfo: c.torrent, StorageDir: t.TempDir()})
	require.NoError(t, err)

	require.NoError(t, leech.AddPeers(h, net.JoinHostPort("127.0.0.1", strconv.Itoa(seeder.Port()))))

	waitEvent(t, leech, core.TorrentFetched, h)
	assert.Error(t, leech.AddPeers(core.InfoHash{2}, "127.0.0.1:1"), "unknown torrent")
	assert.Error(t, leech.AddPeers(h, "not an address"))
}
