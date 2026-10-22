package runner_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestDirIsTheDirectoryOfTheStoredTorrentFile(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.build()

	assert.Equal(t, filepath.Join(h.incomplete, string(a.ID)), h.runner.Dir(a))
}

func TestDirFollowsTheMoveToComplete(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent(withState(core.StateFetched))
	target := filepath.Join(h.complete, "Moved")
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{DownloadID: a.ID, Moved: true, Location: target}))
	h.build()

	assert.Equal(t, target, h.runner.Dir(a))
}

func TestDirOfAMagnetWithoutAFileIsBelowTheIncompleteRoot(t *testing.T) {
	h := newHarness(t)
	m := h.addMagnet()
	h.build()

	assert.Equal(t, filepath.Join(h.incomplete, string(m.ID)), h.runner.Dir(m))
}

func TestContentDirIsTheTorrentsRootDirectory(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	dir := filepath.Join(h.incomplete, string(a.ID))
	write(t, filepath.Join(dir, "Release", "release.rar"), "r")
	write(t, filepath.Join(dir, "Release", "_unpack", "film.mkv"), "f")
	h.build()

	assert.Equal(t, filepath.Join(dir, "Release"), h.runner.ContentDir(a))
}

func TestContentDirIgnoresOurOwnFilesNextToTheTorrent(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	dir := filepath.Join(h.incomplete, string(a.ID))
	write(t, filepath.Join(dir, "Release", "a.mkv"), "a")
	write(t, filepath.Join(dir, ".bitnews-done"), "id")
	write(t, filepath.Join(dir, ".bitnews-metainfo.torrent"), "x")
	h.build()

	assert.Equal(t, filepath.Join(dir, "Release"), h.runner.ContentDir(a))
}

func TestContentDirOfASingleFileTorrentIsTheDownloadDirectory(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	dir := filepath.Join(h.incomplete, string(a.ID))
	write(t, filepath.Join(dir, "movie.7z"), "7z")
	h.build()

	assert.Equal(t, dir, h.runner.ContentDir(a))
}

func TestContentDirDoesNotFollowASymlinkedRoot(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	dir := filepath.Join(h.incomplete, string(a.ID))
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "Release")))
	h.build()

	assert.Equal(t, dir, h.runner.ContentDir(a))
}

func TestManagedMeansTheEngineHasTheTorrent(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)

	assert.True(t, h.runner.Managed(a.ID))
	assert.False(t, h.runner.Managed(core.NewID()))
}

func TestATorrentTheEngineRejectsFailsItsDownloadAndIsNotTriedAgain(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.engine.FailNext("Add", fmt.Errorf("bad: %w", core.ErrInvalidTorrentSpec))
	sub := h.bus.Subscribe(t.Context())
	h.start()

	h.waitState(a.ID, core.StateFailed)

	d := h.get(a.ID)
	assert.Equal(t, core.FailureSourceInvalid, d.FailureReason)
	assert.Equal(t, core.Failed{ID: a.ID, Reason: core.FailureSourceInvalid, Detail: d.FailureDetail}, waitEventOf[core.Failed](t, sub))
	assert.Len(t, h.calls("Add"), 1)
}

func TestATransientAddFailureIsRetried(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.engine.FailNext("Add", errors.New("engine busy"))
	h.start()

	h.waitState(a.ID, core.StateFetching)

	assert.Len(t, h.calls("Add"), 2)
}

func TestAnEngineErrorFailsTheDownloadThatIsStillBeingFetched(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	b := h.addTorrent(withState(core.StateReady))
	h.start()
	h.waitState(a.ID, core.StateFetching)
	h.waitInEngine(b.ID)

	h.engine.Emit(core.TorrentEvent{Kind: core.TorrentErrored, Hash: hashOf(a.ID), Err: fmt.Errorf("magnet metadata: %w", core.ErrInvalidTorrentSpec)})
	h.engine.Emit(core.TorrentEvent{Kind: core.TorrentErrored, Hash: hashOf(b.ID), Err: errors.New("write: disk on fire")})

	h.waitState(a.ID, core.StateFailed)
	assert.Equal(t, core.FailureSourceInvalid, h.get(a.ID).FailureReason)
	h.eventually(func() bool { return len(h.callsFor("Remove", a.ID)) == 1 }, "the failed torrent leaves the engine")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, core.StateReady, h.get(b.ID).State, "a Ready Download is not failed after the fact")
}

func TestAPermissionErrorFromTheEngineFailsTheDownloadNamingTheFolderAndTheUser(t *testing.T) {
	h := newHarness(t)
	a := h.addTorrent()
	h.start()
	h.waitState(a.ID, core.StateFetching)
	folder := filepath.Join(h.incomplete, string(a.ID))

	h.engine.Emit(core.TorrentEvent{Kind: core.TorrentErrored, Hash: hashOf(a.ID),
		Err: &fs.PathError{Op: "open", Path: filepath.Join(folder, "file.bin"), Err: syscall.EACCES}})

	h.waitState(a.ID, core.StateFailed)
	got := h.get(a.ID)
	assert.Equal(t, core.FailureInternal, got.FailureReason)
	assert.Contains(t, got.FailureDetail, folder)
	assert.Contains(t, got.FailureDetail, fmt.Sprintf("uid %d", os.Geteuid()))
}

func TestInfoDescribesARegisteredTorrentFromMemory(t *testing.T) {
	h := newHarness(t)
	h.setSettings(func(s *core.Settings) { s.Seeding.RatioLimit = 1 })
	a := h.addSeeding()
	require.NoError(t, h.records.Upsert(t.Context(), core.TorrentRecord{
		DownloadID: a.ID, Uploaded: 600, SeedingTime: 90 * time.Second, RatioLimit: ptr(2.0),
	}))
	h.start()
	h.waitInEngine(a.ID)

	info, ok := h.runner.Info(a.ID)

	require.True(t, ok)
	assert.Equal(t, hashOf(a.ID), info.Hash)
	assert.Equal(t, filepath.Join(h.incomplete, string(a.ID)), info.Dir)
	assert.EqualValues(t, 600, info.BaseUploaded)
	assert.Equal(t, 90*time.Second, info.BaseSeeding)
	assert.Equal(t, ptr(2.0), info.RatioLimit)
	assert.Nil(t, info.SeedingTimeLimit)
	assert.True(t, info.Fetched)
	assert.False(t, info.LimitPaused)
}

func TestInfoOfAnUnknownDownloadIsNotOK(t *testing.T) {
	h := newHarness(t)
	h.build()

	_, ok := h.runner.Info(core.NewID())

	assert.False(t, ok)
}

func TestInfoShowsALimitPausedTorrentAndFollowsSetShareLimit(t *testing.T) {
	h := newHarness(t)
	a := h.addSeeding()
	h.start()
	h.waitInEngine(a.ID)
	h.engine.SetStats(hashOf(a.ID), seedingStats(600, time.Hour))

	require.NoError(t, h.runner.SetShareLimit(t.Context(), a.ID, core.ShareLimit{Ratio: 0.5, SeedingTime: -1}))

	info, _ := h.runner.Info(a.ID)
	assert.True(t, info.LimitPaused)
	assert.Equal(t, ptr(0.5), info.RatioLimit)
	assert.Equal(t, ptr(time.Duration(-1)), info.SeedingTimeLimit, "unlimited is a negative duration")
}
