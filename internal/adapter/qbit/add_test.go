package qbit_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // an info hash, not a security hash
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/torrent/torrenttest"
)

const magnetSecret = "p4ssk3y-abc123"

var magnet = magnetN(0)

// magnetN is a distinct magnet link per n, with a passkey in its tracker.
func magnetN(n int) string {
	sum := sha1.Sum([]byte(fmt.Sprint("magnet", n))) //nolint:gosec
	return "magnet:?xt=urn:btih:" + hex.EncodeToString(sum[:]) + "&dn=Show.S01E01&tr=https%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3D" + magnetSecret
}

// torrentFile is a valid .torrent file named name.
func torrentFile(t testing.TB, name string) string {
	t.Helper()
	return string(torrenttest.MakeRandom(t, name, map[string]int{"a.bin": 100}).Torrent)
}

// indexer serves .torrent files at /dl/<name>.torrent, as an indexer does,
// and answers 404 for anything else.
func (s *stack) indexer() string {
	s.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Path, "/dl/")
		if !ok || !strings.HasSuffix(name, ".torrent") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(torrentFile(s.t, strings.TrimSuffix(name, ".torrent"))))
	}))
	s.t.Cleanup(srv.Close)
	return srv.URL
}

func (s *stack) add(sid *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.post(sid, "/api/v2/torrents/add", form)
}

func (s *stack) upload(sid *http.Cookie, parts ...part) *httptest.ResponseRecorder {
	s.t.Helper()
	r := multipartRequest(s.t, "/api/v2/torrents/add", parts...)
	r.AddCookie(sid)
	return s.serve(r)
}

// added lists the stored Downloads in the order they were added.
func (s *stack) added() []core.Download {
	s.t.Helper()
	ds, _, err := s.downloads.List(context.Background(), core.DownloadQuery{Limit: 1000})
	require.NoError(s.t, err)
	slices.SortFunc(ds, func(a, b core.Download) int { return a.QueuePosition - b.QueuePosition })
	return ds
}

func TestAddAMagnetLink(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.add(sid, url.Values{"urls": {magnet}, "category": {"tv-sonarr"}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
	require.Len(t, s.added(), 1)
	got := s.info(sid, "")
	require.Len(t, got, 1)
	assert.Equal(t, "tv-sonarr", got[0].Category, "an unknown category is created")
	assert.Equal(t, "metaDL", got[0].State)
}

func TestAddSeveralURLsOnSeparateLines(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	base := s.indexer()
	urls := magnet + "\r\n" + base + "/dl/one.torrent?apikey=k1\n\n  " + base + "/dl/two.torrent  \n" + magnet

	rec := s.add(sid, url.Values{"urls": {urls}})

	assert.Equal(t, "Ok.", rec.Body.String())
	assert.Len(t, s.added(), 3, "the duplicate line is dropped, the spaces are trimmed")
}

func TestAddRefusesURLsThatAreNotMagnetOrHTTP(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	for _, u := range []string{
		"file:///etc/passwd", "ftp://host/x.torrent", "gopher://x", "javascript:alert(1)", "/etc/passwd", "C:\\x.torrent",
		"http://host/\x00x", "http://host/\x1bx", "data:text/plain,hi", "MAGNET", "",
	} {
		rec := s.add(sid, url.Values{"urls": {u}})

		assert.Equal(t, "Fails.", rec.Body.String(), "%q", u)
	}
	assert.Empty(t, s.added(), "nothing was added")
}

func TestAddIsCaseInsensitiveAboutTheScheme(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	base := strings.Replace(s.indexer(), "http://", "HTTP://", 1)

	rec := s.add(sid, url.Values{"urls": {base + "/dl/a.torrent\nMagnet:?xt=urn:btih:" + strings.Repeat("a", 40)}})

	assert.Equal(t, "Ok.", rec.Body.String())
	assert.Len(t, s.added(), 2)
}

func TestAddARefusesTooManyURLsAndHugeOnes(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	var many []string
	for i := range 33 {
		many = append(many, fmt.Sprintf("http://indexer.example/%d.torrent", i))
	}

	tooMany := s.add(sid, url.Values{"urls": {strings.Join(many, "\n")}})
	huge := s.add(sid, url.Values{"urls": {"http://indexer.example/" + strings.Repeat("a", 17<<10)}})

	assert.Equal(t, http.StatusBadRequest, tooMany.Code)
	assert.Equal(t, "Fails.", huge.Body.String())
	assert.Empty(t, s.added())
}

func TestAddATorrentFileUpload(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.upload(sid,
		part{name: "torrents", filename: "Show.S01E01.torrent", content: torrentFile(t, "Show.S01E01")},
		part{name: "category", content: "tv"},
		part{name: "paused", content: "true"},
	)

	assert.Equal(t, "Ok.", rec.Body.String())
	ds := s.added()
	require.Len(t, ds, 1)
	assert.True(t, ds[0].Paused)
	got := s.info(sid, "")
	require.Len(t, got, 1)
	assert.Equal(t, "tv", got[0].Category)
	assert.Equal(t, "pausedDL", got[0].State)
}

func TestAddATorrentFileTogetherWithURLsInTheSameCall(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.upload(sid,
		part{name: "urls", content: magnet},
		part{name: "torrents", filename: "a.torrent", content: torrentFile(t, "a")},
		part{name: "torrents", filename: "b.torrent", content: torrentFile(t, "b")},
	)

	assert.Equal(t, "Ok.", rec.Body.String())
	assert.Len(t, s.added(), 3)
}

func TestTheStoppedOptionOfNewerClientsPausesToo(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	s.add(sid, url.Values{"urls": {magnet}, "stopped": {"true"}})

	assert.True(t, s.added()[0].Paused)
}

func TestAddUsesAnExistingCategoryIgnoringCaseAndRefusesABadName(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	tv := s.category("tv")
	sid := s.login()

	ok := s.add(sid, url.Values{"urls": {magnet}, "category": {"TV"}})
	bad := s.add(sid, url.Values{"urls": {magnetN(1)}, "category": {"not a valid/name"}})

	assert.Equal(t, "Ok.", ok.Body.String())
	assert.Equal(t, "Fails.", bad.Body.String())
	ds := s.added()
	require.Len(t, ds, 1)
	assert.Equal(t, tv.ID, ds[0].CategoryID)
	cats, err := s.categories.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, cats, 1, "no stray Category")
}

func TestAddIgnoresTagsTheSavePathAndTheOtherOptions(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.add(sid, url.Values{
		"urls": {magnet}, "tags": {"sonarr,hd"}, "sequentialDownload": {"true"}, "firstLastPiecePrio": {"true"},
		"autoTMM": {"true"}, "rename": {"Other Name"}, "dlLimit": {"1"}, "upLimit": {"1"}, "contentLayout": {"Original"},
		"savepath": {"/elsewhere"},
	})

	assert.Equal(t, "Ok.", rec.Body.String())
	d := s.added()[0]
	assert.Empty(t, d.CategoryID)
	assert.False(t, d.Paused)
	_, limited := s.engine.limit(d.ID)
	assert.False(t, limited, "no Share Limit of its own")
}

func TestAddShareLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		form url.Values
		want *core.ShareLimit
	}{
		{"none given", url.Values{}, nil},
		{"both global", url.Values{"ratioLimit": {"-2"}, "seedingTimeLimit": {"-2"}}, nil},
		{"both set", url.Values{"ratioLimit": {"2"}, "seedingTimeLimit": {"60"}}, &core.ShareLimit{Ratio: 2, SeedingTime: time.Hour}},
		{"only a ratio", url.Values{"ratioLimit": {"1.5"}}, &core.ShareLimit{Ratio: 1.5, SeedingTime: -1}},
		{"only a time", url.Values{"seedingTimeLimit": {"10"}}, &core.ShareLimit{Ratio: -1, SeedingTime: 10 * time.Minute}},
		{"unlimited", url.Values{"ratioLimit": {"-1"}, "seedingTimeLimit": {"-1"}}, &core.ShareLimit{Ratio: -1, SeedingTime: -1}},
		{"garbage is ignored", url.Values{"ratioLimit": {"lots"}, "seedingTimeLimit": {"NaN"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStack(t)
			sid := s.login()
			tc.form.Set("urls", magnet)

			require.Equal(t, "Ok.", s.add(sid, tc.form).Body.String())

			got, ok := s.engine.limit(s.added()[0].ID)
			if tc.want == nil {
				assert.False(t, ok, "the global default applies")
				return
			}
			assert.Equal(t, limitCall{Limit: *tc.want}, got)
		})
	}
}

func TestAddWithNothingToAddFails(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	assert.Equal(t, "Fails.", s.add(sid, url.Values{"category": {"tv"}}).Body.String())
	assert.Equal(t, "Fails.", s.upload(sid, part{name: "category", content: "tv"}).Body.String())
	assert.Equal(t, "Fails.", s.upload(sid, part{name: "torrents", filename: "empty.torrent", content: ""}).Body.String())
	assert.Empty(t, s.added())
	cats, err := s.categories.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, cats, "no category is made for a call that adds nothing")
}

func TestAddAnExistingTorrentFails(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	require.Equal(t, "Ok.", s.add(sid, url.Values{"urls": {magnet}}).Body.String())

	again := s.add(sid, url.Values{"urls": {magnet}})

	assert.Equal(t, http.StatusOK, again.Code)
	assert.Equal(t, "Fails.", again.Body.String(), "qBittorrent says Fails. for a duplicate")
	assert.Len(t, s.info(sid, ""), 1)
}

func TestAddWithOneGoodOneBadURLIsOk(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	require.Equal(t, "Ok.", s.add(sid, url.Values{"urls": {magnet}}).Body.String())

	rec := s.add(sid, url.Values{"urls": {magnet + "\n" + s.indexer() + "/dl/new.torrent"}})

	assert.Equal(t, "Ok.", rec.Body.String(), "partial success is success")
}

func TestAFailedAddIsFailsAndNeverLogsTheURL(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	secretURL := s.indexer() + "/missing?apikey=hunter2secret&file=a"
	require.Equal(t, "Ok.", s.add(sid, url.Values{"urls": {magnet}}).Body.String())

	rec := s.add(sid, url.Values{"urls": {secretURL + "\n" + magnet}})

	assert.Equal(t, "Fails.", rec.Body.String(), "a 404 and a duplicate")
	logs := s.log.String()
	assert.NotContains(t, logs, "hunter2secret")
	assert.NotContains(t, logs, magnetSecret)
	assert.Contains(t, logs, "torrent not added")
}

func TestAddedURLsNeverReachTheLogs(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	s.add(sid, url.Values{"urls": {magnet + "\nfile:///etc/passwd?passkey=" + magnetSecret}, "category": {"tv"}})
	s.add(sid, url.Values{"urls": {magnet}})

	assert.NotContains(t, s.log.String(), magnetSecret)
	assert.NotContains(t, s.log.String(), "tracker.example")
}

func TestAddRefusesAnUnauthenticatedUploadWithoutReadingIt(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	r := multipartRequest(t, "/api/v2/torrents/add", part{name: "torrents", filename: "a.torrent", content: torrentFile(t, "a")})
	r.Body = &tripwire{t: t}

	rec := s.serve(r)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, s.added())
}

type tripwire struct{ t *testing.T }

func (b *tripwire) Read([]byte) (int, error) {
	b.t.Error("the body of an unauthenticated request was read")
	return 0, errors.New("must not read")
}
func (*tripwire) Close() error { return nil }

func TestAddRefusesAHugeTorrentFile(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.upload(sid, part{name: "torrents", filename: "big.torrent", content: strings.Repeat("x", 10<<20+1)})

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Empty(t, s.added())
}

func TestAddReadsATorrentFileOfTheLimitSize(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.upload(sid, part{name: "torrents", filename: "big.torrent", content: strings.Repeat("x", 10<<20)})

	assert.Equal(t, http.StatusOK, rec.Code, "read in full; it is no torrent, so the answer is Fails.")
	assert.Equal(t, "Fails.", rec.Body.String())
}

func TestAddRefusesAHugeBodyManyFilesAndManyParts(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	big := strings.Repeat("x", 9<<20)

	whole := s.upload(sid,
		part{name: "torrents", filename: "1.torrent", content: big}, part{name: "torrents", filename: "2.torrent", content: big},
		part{name: "torrents", filename: "3.torrent", content: big}, part{name: "torrents", filename: "4.torrent", content: big})
	var files, fields []part
	for i := range 17 {
		files = append(files, part{name: "torrents", filename: fmt.Sprintf("%d.torrent", i), content: "d4:infodee"})
	}
	for i := range 70 {
		fields = append(fields, part{name: fmt.Sprintf("f%d", i), content: "x"})
	}

	assert.Equal(t, http.StatusRequestEntityTooLarge, whole.Code, "the whole body is capped")
	assert.Equal(t, http.StatusBadRequest, s.upload(sid, files...).Code, "17 files")
	assert.Equal(t, http.StatusBadRequest, s.upload(sid, fields...).Code, "70 parts")
	assert.Equal(t, http.StatusRequestEntityTooLarge, s.upload(sid, part{name: "urls", content: strings.Repeat("a", 300<<10)}).Code, "a huge text field")
	assert.Empty(t, s.added())
}

func TestAddWithABrokenMultipartBodyIs400(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	r := multipartRequest(t, "/api/v2/torrents/add", part{name: "urls", content: magnet})
	r.Header.Set("Content-Type", "multipart/form-data") // no boundary
	r.AddCookie(sid)

	rec := s.serve(r)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, s.added())
}

func TestAddedDownloadAppearsWithTheRightCategoryAndPausedFlag(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	s.add(sid, url.Values{"urls": {magnet}, "category": {"Radarr"}, "paused": {"true"}})
	got := s.info(sid, "")

	require.Len(t, got, 1)
	assert.Equal(t, 40, len(got[0].Hash))
	d, err := s.dls.ByInfoHash(t.Context(), got[0].Hash)
	require.NoError(t, err)
	assert.True(t, d.Paused)
	assert.Equal(t, core.SourceTorrent, d.SourceKind)
	assert.Equal(t, "radarr", got[0].Category)
}
