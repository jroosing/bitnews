package daemon_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/usenet/nntp/nntptest"
)

// moveIncompleteRoot changes Paths.Usenet.Incomplete through the API, as the
// UI would, and returns the new root.
func (e *e2e) moveIncompleteRoot() string {
	e.t.Helper()
	var s core.Settings
	e.call("GET", "/api/v1/settings", nil, http.StatusOK, &s)
	s.Paths.Usenet.Incomplete = filepath.Join(e.root, "usenet", "incomplete-moved")
	e.call("PUT", "/api/v1/settings", s, http.StatusOK, nil)
	return s.Paths.Usenet.Incomplete
}

func slowFixture(t *testing.T, srv *fakeNNTP, name string, seed byte) (nntptest.Fixture, []byte) {
	t.Helper()
	data := payload(300_000, seed)
	fx := nntptest.BuildFixture(t, map[string][]byte{name: data}, 5_000) // 60 Articles
	fx.Install(srv.Server)
	return fx, data
}

func TestE2EChangingTheIncompleteRootAffectsOnlyNewDownloads(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	srv.SetLatency(4 * time.Millisecond)
	fxA, dataA := slowFixture(t, srv, "a.bin", 1)
	fxB, dataB := slowFixture(t, srv, "b.bin", 2)
	a := e.addNZB("Old.Download", fxA.NZB, "")
	oldDir := filepath.Join(e.incomplete, string(a.ID))
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 10 }, 10*time.Second, 2*time.Millisecond)
	require.DirExists(t, oldDir)

	newRoot := e.moveIncompleteRoot()
	b := e.addNZB("New.Download", fxB.NZB, "")

	gotA := e.waitState(a.ID, core.StateReady)
	assert.Equal(t, dataA, readTree(t, gotA.OutputPath)["a.bin"], "the in-flight Download finished byte-exact")
	assert.NoDirExists(t, filepath.Join(newRoot, string(a.ID)), "its fetch never moved to the new root")
	assert.NoDirExists(t, oldDir, "post-processing worked in, and moved, the original dir")
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(newRoot, string(b.ID)))
		return err == nil || e.get(b.ID).State == core.StateReady
	}, 10*time.Second, 2*time.Millisecond)
	gotB := e.waitState(b.ID, core.StateReady)
	assert.Equal(t, dataB, readTree(t, gotB.OutputPath)["b.bin"])
	assert.NoDirExists(t, filepath.Join(e.incomplete, string(b.ID)), "a Download added after the change uses the new root")
}

func TestE2EDeleteDataAfterTheIncompleteRootChangedRemovesTheOriginalDir(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	srv.SetLatency(4 * time.Millisecond)
	fx, _ := slowFixture(t, srv, "c.bin", 3)
	c := e.addNZB("To.Delete", fx.NZB, "")
	oldDir := filepath.Join(e.incomplete, string(c.ID))
	require.Eventually(t, func() bool { return srv.TotalRequests() >= 10 }, 10*time.Second, 2*time.Millisecond)
	require.DirExists(t, oldDir)
	e.moveIncompleteRoot()

	e.call("DELETE", "/api/v1/downloads/"+string(c.ID)+"?delete_data=true", nil, http.StatusNoContent, nil)

	assert.NoDirExists(t, oldDir)
	time.Sleep(200 * time.Millisecond)
	assert.NoDirExists(t, oldDir, "nothing recreated it")
}

func TestE2ERestartAfterTheIncompleteRootChangedResumesInTheOriginalDir(t *testing.T) {
	skipHeavy(t)
	e := newE2E(t)
	e.start()
	srv := e.addServer("main", 0)
	srv.SetLatency(4 * time.Millisecond)
	fx, data := slowFixture(t, srv, "d.bin", 4)
	d := e.addNZB("Survivor", fx.NZB, "")
	progress := store.NewUsenetProgressRepo(e.db)
	require.Eventually(t, func() bool {
		_, err := progress.Load(context.Background(), d.ID)
		return err == nil && srv.TotalRequests() >= 15
	}, 15*time.Second, 2*time.Millisecond)
	newRoot := e.moveIncompleteRoot()
	e.stop()
	firstRun := srv.TotalRequests()
	require.Less(t, firstRun, 60)

	e.start()
	got := e.waitState(d.ID, core.StateReady)

	assert.Equal(t, data, readTree(t, got.OutputPath)["d.bin"])
	assert.NoDirExists(t, filepath.Join(newRoot, string(d.ID)))
	assert.Less(t, srv.TotalRequests(), 60+firstRun-5, "it resumed from its saved progress in the original dir")
}
