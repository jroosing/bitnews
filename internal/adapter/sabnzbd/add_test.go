package sabnzbd_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

func TestAddFileStagesTheUploadAndReturnsItsNzoID(t *testing.T) {
	s := newStack(t)

	rec := s.upload("mode=addfile&cat=tv&priority=1&nzbname=Custom.Name", "Show.S01E01.nzb", plainNZB)

	d := s.only()
	assert.Equal(t, []string{"SABnzbd_nzo_" + string(d.ID)}, nzoIDs(t, rec))
	assert.JSONEq(t, `{"status":true,"nzo_ids":["SABnzbd_nzo_`+string(d.ID)+`"]}`, rec.Body.String())
	assert.Equal(t, "Custom.Name", d.Name)
	assert.Equal(t, core.StateQueued, d.State)
	assert.Equal(t, core.DownloadPriorityHigh, d.Priority)
	assert.False(t, d.Paused)
	assert.NotEmpty(t, d.CategoryID)
	assert.DirExists(t, filepath.Join(s.incomplete, string(d.ID)))
}

func TestAddFileNamesTheDownloadAfterTheUploadWhenThereIsNoNzbname(t *testing.T) {
	s := newStack(t)

	for _, filename := range []string{"My Show S01E01 [1080p] é世界.nzb", "plain.NZB", "../../etc/passwd.nzb", `..\..\evil.nzb`} {
		rec := s.upload("mode=addfile", filename, plainNZB)
		require.Len(t, nzoIDs(t, rec), 1, filename)
	}

	names := map[string]bool{}
	for _, d := range s.all() {
		names[d.Name] = true
		assert.True(t, strings.HasPrefix(filepath.Clean(filepath.Join(s.incomplete, string(d.ID))), s.incomplete), "files stay under the incomplete root")
	}
	assert.True(t, names["My Show S01E01 [1080p] é世界"], "%v", names)
	assert.True(t, names["plain"], "%v", names)
	assert.True(t, names["passwd"], "%v", names)
}

func TestAddFileAcceptsTheNzbfileFieldToo(t *testing.T) {
	s := newStack(t)

	rec := s.serve(uploadRequest(t, "/sabnzbd/api?mode=addfile&output=json&apikey="+s.key, "nzbfile", "x.nzb", plainNZB))

	assert.Len(t, nzoIDs(t, rec), 1)
}

func TestAddFileTakesPriorityFromTheQueryOrTheForm(t *testing.T) {
	s := newStack(t)
	for _, tc := range []struct {
		priority string
		want     core.DownloadPriority
		paused   bool
	}{
		{"2", core.DownloadPriorityForce, false}, {"1", core.DownloadPriorityHigh, false},
		{"0", core.DownloadPriorityNormal, false}, {"-1", core.DownloadPriorityLow, false},
		{"-100", core.DownloadPriorityNormal, false}, {"-2", core.DownloadPriorityNormal, true},
		{"", core.DownloadPriorityNormal, false}, {"garbage", core.DownloadPriorityNormal, false},
	} {
		rec := s.upload("mode=addfile&priority="+tc.priority, "x.nzb", plainNZB)
		id := strings.TrimPrefix(nzoIDs(t, rec)[0], "SABnzbd_nzo_")
		got, err := s.downloads.Get(context.Background(), core.ID(id))
		require.NoError(t, err)
		assert.Equal(t, tc.want, got.Priority, "priority=%q", tc.priority)
		assert.Equal(t, tc.paused, got.Paused, "priority=%q", tc.priority)
	}

	rec := s.upload("mode=addfile", "x.nzb", plainNZB, [2]string{"priority", "2"})
	got, err := s.downloads.Get(context.Background(), core.ID(strings.TrimPrefix(nzoIDs(t, rec)[0], "SABnzbd_nzo_")))
	require.NoError(t, err)
	assert.Equal(t, core.DownloadPriorityForce, got.Priority, "a form field works as well as the query")
}

func TestAddFileCategories(t *testing.T) {
	s := newStack(t)
	existing := s.category("tv")
	idOf := func(rec *httptest.ResponseRecorder) core.Download {
		d, err := s.downloads.Get(context.Background(), core.ID(strings.TrimPrefix(nzoIDs(t, rec)[0], "SABnzbd_nzo_")))
		require.NoError(t, err)
		return d
	}

	known := idOf(s.upload("mode=addfile&cat=tv", "a.nzb", plainNZB))
	otherCase := idOf(s.upload("mode=addfile&cat=TV", "b.nzb", plainNZB))
	star := idOf(s.upload("mode=addfile&cat=*", "c.nzb", plainNZB))
	empty := idOf(s.upload("mode=addfile", "d.nzb", plainNZB))
	fresh := idOf(s.upload("mode=addfile&cat=Movies-4k", "e.nzb", plainNZB))

	assert.Equal(t, existing.ID, known.CategoryID)
	assert.Equal(t, existing.ID, otherCase.CategoryID, "names match ignoring case")
	assert.Empty(t, star.CategoryID, "* means none")
	assert.Empty(t, empty.CategoryID)
	cats, err := s.categories.List(context.Background())
	require.NoError(t, err)
	require.Len(t, cats, 2, "an unknown name creates exactly one Category")
	assert.Equal(t, "movies-4k", cats[0].Name)
	assert.Equal(t, fresh.CategoryID, cats[0].ID)
	assert.Empty(t, cats[0].UsenetDir, "the default dir")
	assert.Empty(t, cats[0].TorrentDir)
}

func TestAddFileWithAnInvalidCategoryNameIsAnErrorAndAddsNothing(t *testing.T) {
	s := newStack(t)

	rec := s.upload("mode=addfile&cat=has%20space", "a.nzb", plainNZB)

	assert.Contains(t, errorOf(t, rec), "category name")
	assert.Empty(t, s.all())
	assert.Empty(t, dirEntries(t, s.incomplete), "the staged files are cleaned up")
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestAddFileStoresThePasswordButNeverShowsIt(t *testing.T) {
	s := newStack(t)

	rec := s.upload("mode=addfile&password=hunter2", "a.nzb", plainNZB)

	d := s.only()
	_, password, err := s.sources.Get(context.Background(), d.ID)
	require.NoError(t, err)
	assert.Equal(t, "hunter2", password.Reveal())
	assert.NotContains(t, rec.Body.String(), "hunter2")
	assert.NotContains(t, s.log.String(), "hunter2")
	assert.NotContains(t, s.call("mode=queue").Body.String(), "hunter2")
	assert.NotContains(t, s.call("mode=history").Body.String(), "hunter2")
}

func TestAddFileIgnoresPPAndScript(t *testing.T) {
	s := newStack(t)

	rec := s.upload("mode=addfile&pp=0&script=evil.sh", "a.nzb", plainNZB)

	assert.Len(t, nzoIDs(t, rec), 1)
}

func TestAddFileWithoutAFilePartIsAnErrorInSABsShape(t *testing.T) {
	s := newStack(t)

	noParts := s.upload("mode=addfile", "", "", [2]string{"cat", "tv"})
	get := s.call("mode=addfile")
	textOnly := s.serve(uploadRequest(t, "/sabnzbd/api?mode=addfile&output=json&apikey="+s.key, "name", "", "", [2]string{"name", "not a file"}))

	for _, rec := range []*httptest.ResponseRecorder{noParts, get, textOnly} {
		assert.Equal(t, "No file given", errorOf(t, rec))
	}
	assert.Empty(t, s.all())
}

func TestAddFileRejectsAnInvalidNZBAndLeavesNothingBehind(t *testing.T) {
	s := newStack(t)

	rec := s.upload("mode=addfile", "bad.nzb", "this is not xml")

	assert.Contains(t, errorOf(t, rec), "not a valid NZB")
	assert.Empty(t, s.all())
	assert.Empty(t, dirEntries(t, s.incomplete))
}

func TestAddFileRejectsAnOversizedNZB(t *testing.T) {
	s := newStack(t, withAddOptions(app.WithMaxNZBBytes(100)))

	rec := s.upload("mode=addfile", "big.nzb", plainNZB)

	assert.Contains(t, errorOf(t, rec), "larger than")
	assert.Empty(t, s.all())
}

func TestAddFileUsesOnlyTheFirstFilePart(t *testing.T) {
	s := newStack(t)
	r := uploadRequest(t, "/sabnzbd/api?mode=addfile&output=json&apikey="+s.key, "name", "first.nzb", plainNZB)

	rec := s.serve(r)

	assert.Len(t, nzoIDs(t, rec), 1)
	assert.Len(t, s.all(), 1)
}

// indexer is a fake NZB source that records its requests.
type indexer struct {
	*httptest.Server
	mu   sync.Mutex
	hits int
}

func newIndexer(t *testing.T, h http.HandlerFunc) *indexer {
	t.Helper()
	ix := &indexer{}
	ix.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ix.mu.Lock()
		ix.hits++
		ix.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(ix.Close)
	return ix
}

func serveNZB(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = io.WriteString(w, doc)
	}
}

const indexerSecret = "INDEXERSECRET987"

func TestAddURLFetchesTheNZBAndReturnsItsNzoID(t *testing.T) {
	ix := newIndexer(t, serveNZB(plainNZB))
	s := newStack(t)
	u := ix.URL + "/getnzb/abc.nzb?apikey=" + indexerSecret

	rec := s.call("mode=addurl&cat=tv&priority=2&nzbname=From.Url&name=" + urlQuery(u))

	d := s.only()
	assert.Equal(t, []string{nzo(d)}, nzoIDs(t, rec))
	assert.Equal(t, "From.Url", d.Name)
	assert.Equal(t, core.DownloadPriorityForce, d.Priority)
	assert.NotEmpty(t, d.CategoryID)
	assert.Equal(t, 1, ix.hits)
}

func urlQuery(s string) string {
	return strings.NewReplacer("%", "%25", "&", "%26", "?", "%3F", "=", "%3D", "+", "%2B", " ", "%20", "#", "%23").Replace(s)
}

func TestAddURLNeverPersistsOrLogsTheURL(t *testing.T) {
	ix := newIndexer(t, serveNZB(plainNZB))
	s := newStack(t)
	u := ix.URL + "/getnzb?apikey=" + indexerSecret

	rec := s.call("mode=addurl&name=" + urlQuery(u))
	require.Len(t, nzoIDs(t, rec), 1)

	assert.NotContains(t, s.log.String(), indexerSecret)
	assert.NotContains(t, s.log.String(), s.key)
	assert.NotContains(t, rec.Body.String(), indexerSecret)
	root := filepath.Dir(s.dbPath)
	for _, dir := range []string{root, filepath.Dir(s.incomplete)} {
		_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			raw, _ := os.ReadFile(p)
			assert.NotContains(t, string(raw), indexerSecret, p)
			return nil
		})
	}
}

func TestAddURLFailuresAreSABErrorsWithoutTheSecret(t *testing.T) {
	s := newStack(t)
	notFound := newIndexer(t, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	notNZB := newIndexer(t, serveNZB("<html>login</html>"))
	redirect := newIndexer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://evil.example/x?apikey="+indexerSecret, http.StatusFound)
	})
	dead := newIndexer(t, nil)
	dead.Close()

	for name, tc := range map[string]struct{ url, want string }{
		"404":          {notFound.URL + "/x?apikey=" + indexerSecret, "404"},
		"not an nzb":   {notNZB.URL + "/x?apikey=" + indexerSecret, "not a valid NZB"},
		"bad redirect": {redirect.URL + "/x", "redirect"},
		"refused":      {dead.URL + "/x?apikey=" + indexerSecret, "could not fetch"},
		"ftp":          {"ftp://host/x?apikey=" + indexerSecret, "http or https"},
		"relative":     {"/just/a/path", "http or https"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := s.call("mode=addurl&name=" + urlQuery(tc.url))

			msg := errorOf(t, rec)
			assert.Contains(t, msg, tc.want)
			assert.NotContains(t, msg, indexerSecret)
			assert.NotContains(t, s.log.String(), indexerSecret)
		})
	}
	assert.Empty(t, s.all())
	assert.Empty(t, dirEntries(t, s.incomplete))
}

func TestAddURLWithoutAURLIsAnError(t *testing.T) {
	s := newStack(t)

	assert.Equal(t, "expect one parameter", errorOf(t, s.call("mode=addurl")))
	assert.Equal(t, "expect one parameter", errorOf(t, s.call("mode=addurl&name=")))
}

func TestAddURLChecksThePriorityBeforeFetching(t *testing.T) {
	ix := newIndexer(t, serveNZB(plainNZB))
	s := newStack(t)

	rec := s.call("mode=addurl&password=" + strings.Repeat("x", 2000) + "&name=" + urlQuery(ix.URL+"/x"))

	assert.Contains(t, errorOf(t, rec), "password")
	assert.Equal(t, 0, ix.hits, "nothing was fetched for a request that would be refused")
}

func TestAddURLAsFormPost(t *testing.T) {
	ix := newIndexer(t, serveNZB(plainNZB))
	s := newStack(t)
	form := strings.NewReader("mode=addurl&apikey=" + s.key + "&name=" + urlQuery(ix.URL+"/x") + "&cat=tv")
	r := httptest.NewRequest("POST", "/sabnzbd/api", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := s.serve(r)

	assert.Len(t, nzoIDs(t, rec), 1)
}

func TestAddedDownloadsAppearInTheQueue(t *testing.T) {
	s := newStack(t)
	s.upload("mode=addfile&cat=tv", "Show.S01E01.nzb", plainNZB)

	var q struct {
		Queue struct {
			NoOfSlots int `json:"noofslots"`
			Slots     []struct {
				Filename string `json:"filename"`
				Cat      string `json:"cat"`
				Status   string `json:"status"`
				NzoID    string `json:"nzo_id"`
			} `json:"slots"`
		} `json:"queue"`
	}
	body(t, s.call("mode=queue"), &q)

	require.Len(t, q.Queue.Slots, 1)
	assert.Equal(t, "Show.S01E01", q.Queue.Slots[0].Filename)
	assert.Equal(t, "tv", q.Queue.Slots[0].Cat)
	assert.Equal(t, "Queued", q.Queue.Slots[0].Status)
	assert.Equal(t, fmt.Sprint("SABnzbd_nzo_", s.only().ID), q.Queue.Slots[0].NzoID)
}

func TestAddFileCleansTheNzbnameWhenCleanNamesIsOn(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	cur := s.settings.Current()
	cur.PostProcess.CleanNames = true
	require.NoError(t, s.settings.Update(ctx, cur))

	s.upload("mode=addfile&nzbname=Some.Show.S01E01.1080p-GRP-Obfuscated", "x.nzb", plainNZB)

	assert.Equal(t, "Some.Show.S01E01.1080p-GRP", s.only().Name)
}

func TestAddFileKeepsTheNzbnameWhenCleanNamesIsOff(t *testing.T) {
	s := newStack(t)

	s.upload("mode=addfile&nzbname=Some.Show.S01E01.1080p-GRP-Obfuscated", "x.nzb", plainNZB)

	assert.Equal(t, "Some.Show.S01E01.1080p-GRP-Obfuscated", s.only().Name)
}
