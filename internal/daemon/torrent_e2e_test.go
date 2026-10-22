package daemon_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/daemon"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
	"gitlab.com/kyura4k/gitnews/internal/torrent/torrenttest"
)

// These tests run the whole daemon with the real torrent engine against
// local seeders and leechers on 127.0.0.1 (no network, TCP only, no DHT).

// newTorrentE2E is newE2E with the torrent side on and a real engine that
// keeps its piece completion in the daemon's database.
func newTorrentE2E(t *testing.T) *e2e {
	t.Helper()
	e := newE2E(t)
	e.torrents = true
	e.newEngine()
	return e
}

// newEngine starts a real engine for the daemon, closing the previous one.
func (e *e2e) newEngine() *torrent.Engine {
	e.t.Helper()
	if old, ok := e.engine.(*torrent.Engine); ok {
		require.NoError(e.t, old.Close())
	}
	eng := torrenttest.NewEngine(e.t, func(c *torrent.Config) {
		c.PieceStore = store.NewTorrentPieceRepo(e.db)
		c.FlushInterval = 20 * time.Millisecond
	})
	e.engine = eng
	return eng
}

// restart stops the daemon and the engine and starts both again on the same
// database, as a process restart does.
func (e *e2e) restart() *torrent.Engine {
	e.t.Helper()
	e.stop()
	eng := e.newEngine()
	e.start()
	return eng
}

func (e *e2e) realEngine() *torrent.Engine { return e.engine.(*torrent.Engine) }

func (e *e2e) addMagnet(link string) dl {
	e.t.Helper()
	var out dl
	e.call("POST", "/api/v1/downloads", map[string]any{"magnet": link}, http.StatusCreated, &out)
	return out
}

func (e *e2e) addTorrentFile(name string, raw []byte) dl {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="torrent"; filename="%s.torrent"`, name))
	h.Set("Content-Type", "application/x-bittorrent")
	pw, err := mw.CreatePart(h)
	require.NoError(e.t, err)
	_, err = pw.Write(raw)
	require.NoError(e.t, err)
	require.NoError(e.t, mw.Close())
	req, err := http.NewRequest("POST", e.ts.URL+"/api/v1/downloads", &buf)
	require.NoError(e.t, err)
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := e.client.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	var out dl
	require.Equal(e.t, http.StatusCreated, resp.StatusCode)
	require.NoError(e.t, jsonDecode(resp.Body, &out))
	return out
}

// leech starts a fresh engine that downloads c from the given peer and
// returns once it has everything, with the files in the returned dir.
func leech(t *testing.T, c torrenttest.Content, peer string) string {
	t.Helper()
	l := torrenttest.NewEngine(t)
	dir := t.TempDir()
	h, err := l.Add(context.Background(), core.TorrentSpec{Metainfo: c.Torrent, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, l.AddPeers(h, peer))
	torrenttest.WaitEvent(t, l, core.TorrentFetched, h)
	c.AssertFilesEqual(t, dir)
	return dir
}

func (e *e2e) torrentRecord(id core.ID) core.TorrentRecord {
	e.t.Helper()
	rec, err := store.NewTorrentStateRepo(e.db).Get(context.Background(), id)
	require.NoError(e.t, err)
	return rec
}

func sumTree(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = sha256.Sum256(b)
		return nil
	}))
	return out
}

func TestTorrentEndToEndMagnetToReadyMovedAndStillSeeding(t *testing.T) {
	e := newTorrentE2E(t)
	c := torrenttest.MakeRandom(t, "Show.S01.1080p", map[string]int{"ep1.mkv": 90_000, "sub/ep1.srt": 3_000, "ep2.mkv": 70_000})
	seeder := torrenttest.Seed(t, c)
	e.start()

	added := e.addMagnet(c.Magnet(torrenttest.Addr(seeder)))
	assert.Equal(t, core.StateFetchingMetadata, added.State)
	ready := e.waitState(added.ID, core.StateReady)

	// The metadata gave the Download its name and size.
	assert.Equal(t, "Show.S01.1080p", ready.Name)
	assert.Equal(t, c.Size(), ready.TotalBytes)
	// Moved from the incomplete to the complete root, no rehash.
	rec := e.torrentRecord(added.ID)
	assert.True(t, rec.Moved)
	assert.Equal(t, filepath.Join(e.tcomplete, "Show.S01.1080p"), rec.Location)
	assert.NoDirExists(t, filepath.Join(e.tincomplete, string(added.ID)))
	assert.Equal(t, filepath.Join(rec.Location, "Show.S01.1080p"), ready.OutputPath)
	c.AssertFilesEqual(t, rec.Location)
	assert.NoFileExists(t, filepath.Join(ready.OutputPath, ".bitnews-done"), "the originals get no marker")
	assert.NoDirExists(t, filepath.Join(ready.OutputPath, "_unpack"), "no archives, nothing unpacked")
	// The torrent file was saved for the next start.
	assert.FileExists(t, filepath.Join(rec.Location, "Show.S01.1080p.torrent"))

	// Seeding continues from the new place: with the original seeder gone, a
	// new leecher can only be served by the daemon.
	require.NoError(t, seeder.Close())
	leech(t, c, torrenttest.Addr(e.realEngine()))
	st, err := e.realEngine().Stats(c.Hash)
	require.NoError(t, err)
	assert.Equal(t, core.TorrentSeeding, st.State)
	assert.GreaterOrEqual(t, st.Uploaded, c.Size())
}

func TestTorrentEndToEndTorrentFileUpload(t *testing.T) {
	e := newTorrentE2E(t)
	c := torrenttest.MakeRandom(t, "Movie.2026", map[string]int{"": 60_000})
	seeder := torrenttest.Seed(t, c)
	e.start()

	added := e.addTorrentFile("Movie.2026", c.Torrent)
	assert.Equal(t, core.StateQueued, added.State)
	require.Eventually(t, func() bool { return e.realEngine().AddPeers(c.Hash, torrenttest.Addr(seeder)) == nil },
		10*time.Second, 10*time.Millisecond, "the runner added the torrent")
	ready := e.waitState(added.ID, core.StateReady)

	c.AssertFilesEqual(t, e.torrentRecord(added.ID).Location)
	assert.Equal(t, e.torrentRecord(added.ID).Location, ready.OutputPath, "a single-file torrent's content dir is the download dir")
}

func TestTorrentEndToEndArchiveReleaseIsUnpackedWithoutTouchingTheOriginals(t *testing.T) {
	e := newTorrentE2E(t)
	archive, err := os.ReadFile(filepath.Join("..", "postproc", "unpack", "testdata", "7z", "t0.7z"))
	require.NoError(t, err)
	c := torrenttest.Make(t, "Release.With.Archive", map[string][]byte{"payload.7z": archive, "release.nfo": []byte("nfo")})
	seeder := torrenttest.Seed(t, c)
	e.start()

	added := e.addMagnet(c.Magnet(torrenttest.Addr(seeder)))
	ready := e.waitState(added.ID, core.StateReady)

	out := ready.OutputPath
	assert.Equal(t, "foo\n", readFile(t, filepath.Join(out, "_unpack", "foo")))
	assert.Equal(t, "bar\n", readFile(t, filepath.Join(out, "_unpack", "bar")))
	before := sumTree(t, out)
	assert.Equal(t, sha256.Sum256(archive), before["payload.7z"], "the archive is untouched")
	assert.Equal(t, sha256.Sum256([]byte("nfo")), before["release.nfo"])
	assert.NoDirExists(t, filepath.Join(out, "_unpack.tmp"))
	c.AssertFilesEqual(t, e.torrentRecord(added.ID).Location)

	// Still seeding the original files, byte for byte.
	require.NoError(t, seeder.Close())
	leech(t, c, torrenttest.Addr(e.realEngine()))
	assert.Equal(t, before, sumTree(t, out), "seeding changed nothing")
	st, err := e.realEngine().Stats(c.Hash)
	require.NoError(t, err)
	assert.Equal(t, core.TorrentSeeding, st.State)
}

func TestTorrentEndToEndShareLimitPausesTheTorrentButNotTheDownload(t *testing.T) {
	e := newTorrentE2E(t)
	c := torrenttest.MakeRandom(t, "Limited", map[string]int{"f.bin": 50_000})
	seeder := torrenttest.Seed(t, c)
	e.start()
	added := e.addMagnet(c.Magnet(torrenttest.Addr(seeder)))
	e.waitState(added.ID, core.StateReady)
	require.NoError(t, seeder.Close())

	// Ratio 1: one full copy uploaded.
	settings, err := store.NewSettingsRepo(e.db).Load(context.Background())
	require.NoError(t, err)
	settings.Seeding.RatioLimit = 1
	e.call("PUT", "/api/v1/settings", settings, http.StatusOK, nil)
	leech(t, c, torrenttest.Addr(e.realEngine()))

	require.Eventually(t, func() bool {
		st, err := e.realEngine().Stats(c.Hash)
		return err == nil && st.State == core.TorrentPaused
	}, 15*time.Second, 20*time.Millisecond, "paused once the ratio is reached")
	assert.Equal(t, core.StateReady, e.get(added.ID).State, "the Download stays Ready; the media manager removes it")
	require.Eventually(t, func() bool { return e.torrentRecord(added.ID).Uploaded >= c.Size() },
		10*time.Second, 20*time.Millisecond, "the counters are persisted")
}

func TestTorrentEndToEndRestartOfAFinishedTorrentDoesNotRehash(t *testing.T) {
	e := newTorrentE2E(t)
	c := torrenttest.MakeRandom(t, "Restarted", map[string]int{"a.bin": 120_000, "b.bin": 40_000})
	seeder := torrenttest.Seed(t, c)
	e.start()
	added := e.addMagnet(c.Magnet(torrenttest.Addr(seeder)))
	ready := e.waitState(added.ID, core.StateReady)
	require.NoError(t, seeder.Close())

	eng := e.restart()

	require.Eventually(t, func() bool {
		st, err := eng.Stats(c.Hash)
		return err == nil && st.State == core.TorrentSeeding
	}, 15*time.Second, 20*time.Millisecond, "Recover put the torrent back into the engine, complete")
	assert.Zero(t, eng.BytesHashed(c.Hash), "piece completion spared the rehash")
	assert.Equal(t, core.StateReady, e.get(added.ID).State)
	assert.Equal(t, ready.OutputPath, e.get(added.ID).OutputPath)
	leech(t, c, torrenttest.Addr(eng))
}

func TestTorrentEndToEndRestartInTheMiddleOfADownloadResumes(t *testing.T) {
	e := newTorrentE2E(t)
	c := torrenttest.MakeRandom(t, "Interrupted", map[string]int{"big.bin": 640_000})
	seeder := torrenttest.Seed(t, c)
	require.NoError(t, seeder.SetRateLimits(0, 120_000)) // about 5 s for the whole file
	e.start()
	added := e.addMagnet(c.Magnet(torrenttest.Addr(seeder)))
	require.Eventually(t, func() bool { return e.get(added.ID).DoneBytes >= c.Size()/2 }, 30*time.Second, 10*time.Millisecond,
		"half of it is there")
	require.Equal(t, core.StateFetching, e.get(added.ID).State)

	// The restart finds the Download Fetching and puts it back to Queued; the
	// runner adds it again from the stored .torrent file.
	eng := e.restart()
	require.NoError(t, seeder.SetRateLimits(0, 0))
	require.Eventually(t, func() bool { return eng.AddPeers(c.Hash, torrenttest.Addr(seeder)) == nil },
		10*time.Second, 10*time.Millisecond, "the runner added the torrent again")
	ready := e.waitState(added.ID, core.StateReady)

	c.AssertFilesEqual(t, e.torrentRecord(added.ID).Location)
	assert.Equal(t, c.Size(), ready.TotalBytes)
	st, err := seeder.Stats(c.Hash)
	require.NoError(t, err)
	// The restart happens once half is there. Without resuming, the seeder
	// would upload the whole file again on top of that half (at least 1.5x
	// the size); with resuming it uploads the rest plus whatever was in
	// flight when the first daemon stopped, which can be a few pieces.
	assert.LessOrEqual(t, st.Uploaded, c.Size()+c.Size()*2/5,
		"the pieces received before the restart were not fetched again")
	assert.Less(t, eng.BytesHashed(c.Hash), c.Size()-c.Size()/3, "and not hashed again either")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func jsonDecode(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

func TestBuildStartsItsOwnTorrentEngineAndRunClosesIt(t *testing.T) {
	e := newE2E(t)
	e.torrents = true
	port := freePort(t)
	e.torrentOpts = daemon.TorrentOptions{Port: port, DisableDHT: true, DisableUTP: true}

	e.start()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "the engine listens on the torrent port")
	e.stop()

	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = c.Close()
	}
	require.Error(t, err, "Run closed the engine")
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestTorrentsAreRefusedWhenTheTorrentSideIsOff(t *testing.T) {
	e := newE2E(t) // torrents off
	e.start()

	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	e.call("POST", "/api/v1/downloads", map[string]any{"magnet": "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, http.StatusServiceUnavailable, &out)

	assert.Equal(t, "torrents_unavailable", out.Error.Code)
}
