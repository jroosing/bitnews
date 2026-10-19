package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/postproc/adapt"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

// The store repos match the runner's interfaces structurally; the store
// package must not import the runner, so the assertions live here.
var (
	_ runner.ProgressStore     = (*store.UsenetProgressRepo)(nil)
	_ runner.VolumeStore       = (*store.UsenetUndeferredRepo)(nil)
	_ runner.ReportStore       = reports{}
	_ runner.ServerSource      = (*serverSource)(nil)
	_ runner.NZBSource         = nzbSource{}
	_ adapt.DeferredController = (*runner.Runner)(nil)
	_ core.FetchReporter       = (*runner.Runner)(nil)
	_ fetchHolder              = (*runner.Runner)(nil)
	_ core.FS                  = osFS{}
)

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func openDB(t *testing.T) (*store.DB, store.Key) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bitnews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, store.GenerateKey()
}

func TestServerSourceMapsServersToSpecsAndRevealsThePassword(t *testing.T) {
	db, key := openDB(t)
	repo := store.NewServerRepo(db, key)
	require.NoError(t, repo.Create(context.Background(), core.Server{
		ID: core.NewID(), Name: "main", Host: "news.example", Port: 563, Username: "u", Password: "secret",
		MaxConnections: 4, RetentionDays: 1000, Priority: 2, Enabled: false, CreatedAt: time.Now(),
	}))
	src := newServerSource(repo, slog.New(slog.DiscardHandler))

	specs, err := src.Servers(context.Background())

	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, runner.ServerSpec{
		Name: "main", Host: "news.example", Port: 563, User: "u", Pass: "secret",
		Priority: 2, MaxConnections: 4, RetentionDays: 1000, Enabled: false,
	}, specs[0])
}

func TestServerSourceSkipsAServerWithoutAUsablePasswordAndWarnsOnce(t *testing.T) {
	db, key := openDB(t)
	ctx := context.Background()
	good := core.Server{ID: core.NewID(), Name: "good", Host: "a.example", Port: 563, MaxConnections: 2, Enabled: true, CreatedAt: time.Now()}
	bad := core.Server{ID: core.NewID(), Name: "bad", Host: "b.example", Port: 563, Password: "pw", MaxConnections: 2, Enabled: true, CreatedAt: time.Now()}
	require.NoError(t, store.NewServerRepo(db, key).Create(ctx, good))
	require.NoError(t, store.NewServerRepo(db, key).Create(ctx, bad))
	logs := &syncBuf{}
	// Another key cannot open the sealed password of "bad".
	src := newServerSource(store.NewServerRepo(db, store.GenerateKey()), slog.New(slog.NewTextHandler(logs, nil)))

	for range 3 {
		specs, err := src.Servers(ctx)
		require.NoError(t, err)
		require.Len(t, specs, 1)
		assert.Equal(t, "good", specs[0].Name)
	}

	assert.Equal(t, 1, strings.Count(logs.String(), "password cannot be decrypted"), logs.String())
	assert.NotContains(t, logs.String(), "pw")
}

func TestServerSourceWarnsAgainWhenAFixedPasswordBreaksAgain(t *testing.T) {
	db, key := openDB(t)
	ctx := context.Background()
	srv := core.Server{ID: core.NewID(), Name: "flaky", Host: "b.example", Port: 563, Password: "pw", MaxConnections: 2, Enabled: true, CreatedAt: time.Now()}
	require.NoError(t, store.NewServerRepo(db, key).Create(ctx, srv))
	logs := &syncBuf{}
	repo := &switchRepo{ServerRepo: store.NewServerRepo(db, store.GenerateKey())}
	src := newServerSource(repo, slog.New(slog.NewTextHandler(logs, nil)))

	_, _ = src.Servers(ctx) // broken: warns
	_, _ = src.Servers(ctx) // still broken: silent
	repo.ServerRepo = store.NewServerRepo(db, key)
	specs, err := src.Servers(ctx) // usable
	require.NoError(t, err)
	require.Len(t, specs, 1)
	repo.ServerRepo = store.NewServerRepo(db, store.GenerateKey())
	_, _ = src.Servers(ctx) // broken again: warns again

	assert.Equal(t, 2, strings.Count(logs.String(), "password cannot be decrypted"), logs.String())
}

type switchRepo struct{ core.ServerRepo }

func TestNZBSourceReturnsTheNZBPathEvenWhenThePasswordCannotBeDecrypted(t *testing.T) {
	db, key := openDB(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	d := core.Download{ID: core.NewID(), Name: "x", SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at}
	require.NoError(t, store.NewDownloadRepo(db).Create(ctx, d))
	require.NoError(t, store.NewDownloadSourceRepo(db, key).Create(ctx, d.ID, "/x.nzb", "pw"))

	ok, pw, err := nzbSource{repo: store.NewDownloadSourceRepo(db, key), log: slog.New(slog.DiscardHandler)}.Source(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, "/x.nzb", ok)
	assert.Equal(t, "pw", pw.Reveal())

	path, pw, err := nzbSource{repo: store.NewDownloadSourceRepo(db, store.GenerateKey()), log: slog.New(slog.DiscardHandler)}.Source(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, "/x.nzb", path)
	assert.Empty(t, pw.Reveal())
}

func TestReportAdapterRoundTripsAFetchDetail(t *testing.T) {
	db, _ := openDB(t)
	ctx := context.Background()
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	d := core.Download{ID: core.NewID(), Name: "x", SourceKind: core.SourceNZB, State: core.StateQueued,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at}
	require.NoError(t, store.NewDownloadRepo(db).Create(ctx, d))
	r := reports{repo: store.NewUsenetReportRepo(db)}
	want := runner.FetchDetail{MissingArticles: 1, MissingBytes: 2, DamagedArticles: 3, DamagedBytes: 4, MissingFiles: 5}

	_, err := r.LoadReport(ctx, d.ID)
	require.ErrorIs(t, err, core.ErrNotFound)
	require.NoError(t, r.SaveReport(ctx, d.ID, want))
	got, err := r.LoadReport(ctx, d.ID)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}
