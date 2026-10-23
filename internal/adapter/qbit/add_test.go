package qbit_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/qbit"
	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

const (
	magnetSecret = "p4ssk3y-abc123"
	magnet       = "magnet:?xt=urn:btih:c9e15763f722f23e98a29decdfae341b98d53056&dn=Show.S01E01&tr=https%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3D" + magnetSecret
	torrentBody  = "d4:infod6:lengthi1e4:name1:a12:piece lengthi16384e6:pieces20:AAAAAAAAAAAAAAAAAAAAee"
)

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

func TestAddAMagnetLink(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.add(sid, url.Values{"urls": {magnet}, "category": {"tv-sonarr"}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
	calls := s.engine.addCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, magnet, calls[0].URL, "handed to the adder as it came")
	got := s.info(sid, "")
	require.Len(t, got, 1)
	assert.Equal(t, "tv-sonarr", got[0].Category, "an unknown category is created")
	assert.Equal(t, "metaDL", got[0].State)
}

func TestAddSeveralURLsOnSeparateLines(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	urls := magnet + "\r\nhttps://indexer.example/dl/1.torrent?apikey=k1\n\n  http://indexer.example/dl/2.torrent  \n" + magnet

	rec := s.add(sid, url.Values{"urls": {urls}})

	assert.Equal(t, "Ok.", rec.Body.String())
	calls := s.engine.addCalls()
	require.Len(t, calls, 3, "the duplicate line is dropped")
	assert.Equal(t, "http://indexer.example/dl/2.torrent", calls[2].URL, "trimmed")
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
	assert.Empty(t, s.engine.addCalls(), "nothing reached the adder")
}

func TestAddIsCaseInsensitiveAboutTheScheme(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.add(sid, url.Values{"urls": {"HTTPS://Indexer.example/a.torrent\nMagnet:?xt=urn:btih:" + strings.Repeat("a", 40)}})

	assert.Equal(t, "Ok.", rec.Body.String())
	assert.Len(t, s.engine.addCalls(), 2)
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
	assert.Empty(t, s.engine.addCalls())
}

func TestAddATorrentFileUpload(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.upload(sid,
		part{name: "torrents", filename: "Show.S01E01.torrent", content: torrentBody},
		part{name: "category", content: "tv"},
		part{name: "paused", content: "true"},
	)

	assert.Equal(t, "Ok.", rec.Body.String())
	calls := s.engine.addCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].File)
	assert.Equal(t, torrentBody, string(calls[0].Data))
	assert.True(t, calls[0].Opts.Paused)
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
		part{name: "torrents", filename: "a.torrent", content: torrentBody},
		part{name: "torrents", filename: "b.torrent", content: torrentBody + "x"},
	)

	assert.Equal(t, "Ok.", rec.Body.String())
	assert.Len(t, s.engine.addCalls(), 3)
}

func TestTheStoppedOptionOfNewerClientsPausesToo(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	s.add(sid, url.Values{"urls": {magnet}, "stopped": {"true"}})

	assert.True(t, s.engine.addCalls()[0].Opts.Paused)
}

func TestAddUsesAnExistingCategoryIgnoringCaseAndRefusesABadName(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	tv := s.category("tv")
	sid := s.login()

	ok := s.add(sid, url.Values{"urls": {magnet}, "category": {"TV"}})
	bad := s.add(sid, url.Values{"urls": {magnet + "2"}, "category": {"not a valid/name"}})

	assert.Equal(t, "Ok.", ok.Body.String())
	assert.Equal(t, tv.ID, s.engine.addCalls()[0].Opts.CategoryID)
	assert.Equal(t, "Fails.", bad.Body.String())
	assert.Len(t, s.engine.addCalls(), 1)
	cats, err := s.categories.List(t.Context())
	require.NoError(t, err)
	assert.Len(t, cats, 1, "no stray Category")
}

func TestAddIgnoresTagsAndTheOtherOptions(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.add(sid, url.Values{
		"urls": {magnet}, "tags": {"sonarr,hd"}, "sequentialDownload": {"true"}, "firstLastPiecePrio": {"true"},
		"autoTMM": {"true"}, "rename": {"Other Name"}, "dlLimit": {"1"}, "upLimit": {"1"}, "contentLayout": {"Original"},
	})

	assert.Equal(t, "Ok.", rec.Body.String())
	assert.Equal(t, qbit.AddOptions{}, s.engine.addCalls()[0].Opts)
}

func TestAddSavePathOnlyUnderATorrentRoot(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	require.NoError(t, os.MkdirAll(filepath.Join(s.torrents, "tv"), 0o755))
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(s.incomplete, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(s.torrents, "escape")))
	tests := []struct {
		name, path, want string
	}{
		{"the complete root", s.torrents, s.torrents},
		{"below the complete root", filepath.Join(s.torrents, "tv", "new"), filepath.Join(s.torrents, "tv", "new")},
		{"below the incomplete root", filepath.Join(s.incomplete, "x"), filepath.Join(s.incomplete, "x")},
		{"unclean but inside", s.torrents + "/tv/../tv//x/", filepath.Join(s.torrents, "tv", "x")},
		{"outside", outside, ""},
		{"traversal out of the root", s.torrents + "/../../etc", ""},
		{"a sibling with the same prefix", s.torrents + "-evil", ""},
		{"relative", "downloads/tv", ""},
		{"through a symlink out of the root", filepath.Join(s.torrents, "escape", "x"), ""},
		{"root of the disk", "/", ""},
		{"with a NUL", s.torrents + "/a\x00b", ""},
	}
	for i, tc := range tests {
		rec := s.add(sid, url.Values{"urls": {fmt.Sprintf("%s%d", magnet, i)}, "savepath": {tc.path}})

		assert.Equal(t, "Ok.", rec.Body.String(), "%s: the add still works, the path is ignored", tc.name)
		calls := s.engine.addCalls()
		assert.Equal(t, tc.want, calls[len(calls)-1].Opts.SavePath, tc.name)
	}
}

func TestAddShareLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		form url.Values
		want *qbit.ShareLimit
	}{
		{"none given", url.Values{}, nil},
		{"both global", url.Values{"ratioLimit": {"-2"}, "seedingTimeLimit": {"-2"}}, nil},
		{"both set", url.Values{"ratioLimit": {"2"}, "seedingTimeLimit": {"60"}}, &qbit.ShareLimit{Ratio: 2, SeedingSeconds: 3600}},
		{"only a ratio", url.Values{"ratioLimit": {"1.5"}}, &qbit.ShareLimit{Ratio: 1.5, SeedingSeconds: -1}},
		{"only a time", url.Values{"seedingTimeLimit": {"10"}}, &qbit.ShareLimit{Ratio: -1, SeedingSeconds: 600}},
		{"unlimited", url.Values{"ratioLimit": {"-1"}, "seedingTimeLimit": {"-1"}}, &qbit.ShareLimit{Ratio: -1, SeedingSeconds: -1}},
		{"garbage is ignored", url.Values{"ratioLimit": {"lots"}, "seedingTimeLimit": {"NaN"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStack(t)
			sid := s.login()
			tc.form.Set("urls", magnet)

			require.Equal(t, "Ok.", s.add(sid, tc.form).Body.String())

			assert.Equal(t, tc.want, s.engine.addCalls()[0].Opts.ShareLimit)
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
	assert.Empty(t, s.engine.addCalls())
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

	rec := s.add(sid, url.Values{"urls": {magnet + "\nhttps://indexer.example/new.torrent"}})

	assert.Equal(t, "Ok.", rec.Body.String(), "partial success is success")
}

func TestAnAdderErrorIsFailsAndNeverLogsTheURL(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	secretURL := "https://indexer.example/dl/1.torrent?apikey=hunter2secret&file=a"
	s.engine.failAdd = fmt.Errorf("fetch %s: %w", secretURL, app.ErrInvalidInput)

	rec := s.add(sid, url.Values{"urls": {secretURL + "\n" + magnet}})

	assert.Equal(t, "Fails.", rec.Body.String())
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
	s.engine.failAdd = errors.New("boom " + magnet)
	s.add(sid, url.Values{"urls": {magnet + "2"}})

	assert.NotContains(t, s.log.String(), magnetSecret)
	assert.NotContains(t, s.log.String(), "tracker.example")
}

func TestAddRefusesAnUnauthenticatedUploadWithoutReadingIt(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	r := multipartRequest(t, "/api/v2/torrents/add", part{name: "torrents", filename: "a.torrent", content: torrentBody})
	r.Body = &tripwire{t: t}

	rec := s.serve(r)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, s.engine.addCalls())
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
	assert.Empty(t, s.engine.addCalls())
}

func TestAddAcceptsATorrentFileOfTheLimitSize(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.upload(sid, part{name: "torrents", filename: "big.torrent", content: strings.Repeat("x", 10<<20)})

	assert.Equal(t, "Ok.", rec.Body.String())
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
		files = append(files, part{name: "torrents", filename: fmt.Sprintf("%d.torrent", i), content: torrentBody})
	}
	for i := range 70 {
		fields = append(fields, part{name: fmt.Sprintf("f%d", i), content: "x"})
	}

	assert.Equal(t, http.StatusRequestEntityTooLarge, whole.Code, "the whole body is capped")
	assert.Equal(t, http.StatusBadRequest, s.upload(sid, files...).Code, "17 files")
	assert.Equal(t, http.StatusBadRequest, s.upload(sid, fields...).Code, "70 parts")
	assert.Equal(t, http.StatusRequestEntityTooLarge, s.upload(sid, part{name: "urls", content: strings.Repeat("a", 300<<10)}).Code, "a huge text field")
	assert.Empty(t, s.engine.addCalls())
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
	assert.Empty(t, s.engine.addCalls())
}

func TestAddedDownloadAppearsWithTheRightCategoryAndPausedFlag(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	s.add(sid, url.Values{"urls": {magnet}, "category": {"Radarr"}, "paused": {"true"}})
	hash := s.info(sid, "")[0].Hash

	assert.Equal(t, 40, len(hash))
	d, err := s.dls.Get(t.Context(), s.engine.torrents[hash].DownloadID)
	require.NoError(t, err)
	assert.True(t, d.Paused)
	assert.Equal(t, core.SourceTorrent, d.SourceKind)
}
