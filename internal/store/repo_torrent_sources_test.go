package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

var _ core.TorrentSourceRepo = (*store.TorrentSourceRepo)(nil)

func newTorrentSourceRepo(t *testing.T) (*store.TorrentSourceRepo, *store.DB, store.Key) {
	t.Helper()
	db, _ := openTemp(t)
	key := store.GenerateKey()
	return store.NewTorrentSourceRepo(db, key), db, key
}

func hashOf(b byte) core.InfoHash {
	var h core.InfoHash
	for i := range h {
		h[i] = b
	}
	return h
}

func TestTorrentSourceRepoCreateThenGetRoundTripsAFileSource(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	id := aDownload(t, db)
	want := core.TorrentSource{DownloadID: id, InfoHash: hashOf(0xab), TorrentPath: "/data/" + string(id) + "/x.torrent"}

	require.NoError(t, repo.Create(ctx, want))
	got, err := repo.Get(ctx, id)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTorrentSourceRepoStoresTheMagnetSealedWithTheRowAAD(t *testing.T) {
	ctx := context.Background()
	repo, db, key := newTorrentSourceRepo(t)
	id := aDownload(t, db)
	magnet := "magnet:?xt=urn:btih:abababababababababababababababababababab&tr=http://t.example/announce?passkey=SECRET123"
	require.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(0xab), Magnet: core.Secret(magnet)}))

	var raw string
	require.NoError(t, db.Reader().QueryRow(`SELECT magnet FROM torrent_sources WHERE download_id = ?`, string(id)).Scan(&raw))
	got, err := repo.Get(ctx, id)

	require.NoError(t, err)
	assert.Equal(t, magnet, got.Magnet.Reveal())
	assert.Empty(t, got.TorrentPath)
	assert.True(t, strings.HasPrefix(raw, "v1:"), "sealed format, got %q", raw)
	assert.NotContains(t, raw, "SECRET123")
	plain, err := store.Unseal(key, raw, store.AAD("torrent_sources", "magnet", string(id)))
	require.NoError(t, err)
	assert.Equal(t, magnet, plain)
}

func TestTorrentSourceRepoGetByInfoHash(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(1), Magnet: "magnet:?xt=urn:btih:x"}))

	got, err := repo.GetByInfoHash(ctx, hashOf(1))
	require.NoError(t, err)
	assert.Equal(t, id, got.DownloadID)
	assert.Equal(t, "magnet:?xt=urn:btih:x", got.Magnet.Reveal())

	_, err = repo.GetByInfoHash(ctx, hashOf(2))
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentSourceRepoGetUnknownIsNotFound(t *testing.T) {
	repo, _, _ := newTorrentSourceRepo(t)

	_, err := repo.Get(context.Background(), core.NewID())

	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentSourceRepoDuplicateInfoHashIsConflict(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	first, second := aDownload(t, db), aDownload(t, db)
	require.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: first, InfoHash: hashOf(7), TorrentPath: "/a.torrent"}))

	err := repo.Create(ctx, core.TorrentSource{DownloadID: second, InfoHash: hashOf(7), TorrentPath: "/b.torrent"})

	assert.ErrorIs(t, err, core.ErrConflict)
	_, err = repo.Get(ctx, second)
	assert.ErrorIs(t, err, core.ErrNotFound)
}

func TestTorrentSourceRepoSecondSourceForTheSameDownloadIsConflict(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(7), TorrentPath: "/a.torrent"}))

	err := repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(8), TorrentPath: "/b.torrent"})

	assert.ErrorIs(t, err, core.ErrConflict)
}

func TestTorrentSourceRepoNeedsAnExistingDownloadAndAPathOrMagnet(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	id := aDownload(t, db)

	assert.Error(t, repo.Create(ctx, core.TorrentSource{DownloadID: core.NewID(), InfoHash: hashOf(1), TorrentPath: "/a"}), "no such Download")
	assert.Error(t, repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(1)}), "neither path nor magnet")
}

func TestTorrentSourceTableChecksItsColumns(t *testing.T) {
	db := openDefault(t)
	dl := aDownload(t, db)

	for name, stmt := range map[string]string{
		"short hash":     `INSERT INTO torrent_sources VALUES ('` + string(dl) + `', 'abcd', '/p', NULL, 1)`,
		"neither source": `INSERT INTO torrent_sources VALUES ('` + string(dl) + `', '` + hashOf(1).String() + `', NULL, NULL, 1)`,
		"no created_at":  `INSERT INTO torrent_sources (download_id, info_hash, torrent_path) VALUES ('` + string(dl) + `', '` + hashOf(1).String() + `', '/p')`,
	} {
		_, err := db.Writer().ExecContext(context.Background(), stmt)
		assert.Error(t, err, name)
	}
}

func TestTorrentSourceRepoWrongKeyIsSecretUnavailableButKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(3), Magnet: "magnet:?xt=urn:btih:y"}))

	got, err := store.NewTorrentSourceRepo(db, store.GenerateKey()).Get(ctx, id)

	assert.ErrorIs(t, err, core.ErrSecretUnavailable)
	assert.Equal(t, id, got.DownloadID)
	assert.Equal(t, hashOf(3), got.InfoHash)
	assert.Empty(t, got.Magnet)
}

func TestTorrentSourceRepoSetTorrentPath(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(3), Magnet: "magnet:?xt=urn:btih:y"}))

	require.NoError(t, repo.SetTorrentPath(ctx, id, "/d/x.torrent"))
	got, err := repo.Get(ctx, id)

	require.NoError(t, err)
	assert.Equal(t, "/d/x.torrent", got.TorrentPath)
	assert.Equal(t, "magnet:?xt=urn:btih:y", got.Magnet.Reveal(), "the magnet stays")
	assert.ErrorIs(t, repo.SetTorrentPath(ctx, core.NewID(), "/x"), core.ErrNotFound)
	assert.Error(t, repo.SetTorrentPath(ctx, id, ""))
}

func TestTorrentSourceIsDeletedWithItsDownload(t *testing.T) {
	ctx := context.Background()
	repo, db, _ := newTorrentSourceRepo(t)
	id := aDownload(t, db)
	require.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: id, InfoHash: hashOf(3), TorrentPath: "/a"}))

	require.NoError(t, store.NewDownloadRepo(db).Delete(ctx, id))

	_, err := repo.Get(ctx, id)
	assert.ErrorIs(t, err, core.ErrNotFound)
	// the hash is free again
	id2 := aDownload(t, db)
	assert.NoError(t, repo.Create(ctx, core.TorrentSource{DownloadID: id2, InfoHash: hashOf(3), TorrentPath: "/b"}))
}
