package daemon_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
)

// settleGoroutines waits until the process has at most max goroutines and
// fails with a dump of the stacks when it never gets there.
func settleGoroutines(t *testing.T, max int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > max {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: %d goroutines, want at most %d\n%s", runtime.NumGoroutine(), max, buf)
		}
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonRunStopsEveryGoroutineItStarted(t *testing.T) {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()
	func() {
		e := newE2E(t)
		e.start()
		srv := e.addServer("main", 0)
		fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": payload(80_000, 7)}, 10_000)
		fx.Install(srv.Server)
		added := e.addNZB("Leak.Check", fx.NZB, "")
		e.waitState(added.ID, core.StateReady)
		e.stop()
		require.NoError(t, e.db.Close())
	}()

	settleGoroutines(t, baseline)
}

func TestDaemonRunCanOnlyBeCalledOnce(t *testing.T) {
	d := newE2E(t).build()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the first Run returns at once

	require.NoError(t, d.Run(ctx))

	assert.Error(t, d.Run(context.Background()))
}

func TestServeStopsTheHTTPServerBeforeTheWorkers(t *testing.T) {
	e := newE2E(t)
	srv := e.addServerOffline("main")
	data := payload(60_000, 8)
	fx := nntptest.BuildFixture(t, map[string][]byte{"movie.mkv": data}, 10_000)
	fx.Install(srv.Server)
	d := e.build()
	ctx, cancel := context.WithCancel(context.Background())
	var order []string
	served := make(chan struct{})
	served2 := make(chan error, 1)
	go func() {
		served2 <- d.Serve(ctx, "127.0.0.1:0", func(ctx context.Context, _ *http.Server) error {
			close(served)
			<-ctx.Done()
			order = append(order, "http stopped")
			// The workers are still up: a Download added now is picked up.
			id := e.queueDirect(fx.NZB, "late", d)
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if st := e.stateDirect(id); st != core.StateQueued {
					order = append(order, "worker active")
					return nil
				}
				time.Sleep(10 * time.Millisecond)
			}
			return nil
		})
	}()
	<-served
	cancel()

	select {
	case err := <-served2:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return")
	}
	assert.Equal(t, []string{"http stopped", "worker active"}, order)
}

// addServerOffline registers an in-memory Server straight in the database,
// for tests that never start the HTTP side.
func (e *e2e) addServerOffline(name string) *fakeNNTP {
	e.t.Helper()
	f := &fakeNNTP{Server: nntptest.NewServer(name), pass: "secret"}
	e.mu.Lock()
	e.servers[name] = f
	e.mu.Unlock()
	require.NoError(e.t, store.NewServerRepo(e.db, e.key).Create(context.Background(), core.Server{
		ID: core.NewID(), Name: name, Host: name + ".example", Port: 563, Username: "u", Password: "secret",
		MaxConnections: 2, Enabled: true, CreatedAt: time.Now(),
	}))
	return f
}

// queueDirect stores a Queued NZB Download without the API and announces it.
func (e *e2e) queueDirect(nzb []byte, name string, d interface{ Bus() *core.Bus }) core.ID {
	e.t.Helper()
	ctx := context.Background()
	id := core.NewID()
	dir := filepath.Join(e.incomplete, string(id))
	require.NoError(e.t, os.MkdirAll(dir, 0o777))
	path := filepath.Join(dir, name+".nzb")
	require.NoError(e.t, os.WriteFile(path, nzb, 0o600))
	at := time.Now().UTC()
	dl := core.Download{ID: id, Name: name, SourceKind: core.SourceNZB, State: core.StateAdded,
		Priority: core.DownloadPriorityNormal, AddedAt: at, UpdatedAt: at, TotalBytes: 1}
	repo := store.NewDownloadRepo(e.db)
	require.NoError(e.t, repo.Create(ctx, dl))
	require.NoError(e.t, store.NewDownloadSourceRepo(e.db, e.key).Create(ctx, id, path, ""))
	queued, err := repo.UpdateState(ctx, id, core.StateChange{To: core.StateQueued, At: at})
	require.NoError(e.t, err)
	d.Bus().Publish(core.DownloadAdded{Download: queued})
	return id
}

func (e *e2e) stateDirect(id core.ID) core.State {
	d, err := store.NewDownloadRepo(e.db).Get(context.Background(), id)
	require.NoError(e.t, err)
	return d.State
}
