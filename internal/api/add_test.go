package api_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// nzbXML builds an NZB. head is the inner XML of <head>. It has a 150-byte
// data file, a 25-byte PAR2 index and a deferred 1000-byte PAR2 volume.
func nzbXML(head string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
<head>` + head + `</head>
<file poster="p@example.com" date="1700000000" subject="[1/3] - &quot;show.part1.rar&quot; yEnc (1/2)">
<groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="100" number="1">one@example.com</segment><segment bytes="50" number="2">two@example.com</segment></segments>
</file>
<file poster="p@example.com" date="1700000000" subject="[2/3] - &quot;show.par2&quot; yEnc (1/1)">
<groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="25" number="1">three@example.com</segment></segments>
</file>
<file poster="p@example.com" date="1700000000" subject="[3/3] - &quot;show.vol00+01.par2&quot; yEnc (1/1)">
<groups><group>alt.binaries.test</group></groups>
<segments><segment bytes="1000" number="1">four@example.com</segment></segments>
</file>
</nzb>`
}

const metaName = `<meta type="name">Meta.Release.Name</meta>`

type formPart struct{ name, filename, value string }

// multipartBody returns the body and Content-Type of a form with the parts
// in the given order.
func multipartBody(t *testing.T, parts ...formPart) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.filename != "" {
			fw, err := w.CreateFormFile(p.name, p.filename)
			require.NoError(t, err)
			_, err = io.WriteString(fw, p.value)
			require.NoError(t, err)
			continue
		}
		require.NoError(t, w.WriteField(p.name, p.value))
	}
	require.NoError(t, w.Close())
	return buf.String(), w.FormDataContentType()
}

// newAddEnv is an env whose Usenet incomplete directory is a fresh temp dir.
func newAddEnv(t *testing.T, opts ...envOption) (*env, string) {
	t.Helper()
	e := newEnv(t, opts...)
	root := t.TempDir()
	s := e.settings.Current()
	s.Paths.Usenet.Incomplete = filepath.Join(root, "incomplete")
	s.Paths.Usenet.Complete = filepath.Join(root, "complete")
	s.Paths.Torrent.Incomplete = filepath.Join(root, "torrents-incomplete")
	s.Paths.Torrent.Complete = filepath.Join(root, "torrents-complete")
	require.NoError(t, e.settings.Update(context.Background(), s))
	return e, s.Paths.Usenet.Incomplete
}

// upload posts an NZB with extra form parts after it.
func (e *env) upload(key, nzb string, extra ...formPart) *httptest.ResponseRecorder {
	e.t.Helper()
	parts := append([]formPart{{"nzb", "release.nzb", nzb}}, extra...)
	body, ct := multipartBody(e.t, parts...)
	return e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct))
}

func (e *env) addedDownload(rec *httptest.ResponseRecorder) downloadJSON {
	e.t.Helper()
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	var d downloadJSON
	decode(e.t, rec, &d)
	return d
}

// leftovers lists what is under the incomplete root.
func leftovers(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestUploadCreatesAQueuedNZBDownloadAndStoresTheNZB(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	events := e.subscribe()
	doc := nzbXML(metaName)

	rec := e.upload(key, doc)

	got := e.addedDownload(rec)
	assert.Equal(t, "queued", got.State)
	assert.Equal(t, "nzb", got.SourceKind)
	assert.Equal(t, "Meta.Release.Name", got.Name)
	assert.Equal(t, "normal", got.Priority)
	assert.False(t, got.Paused)
	assert.Equal(t, 0, got.QueuePosition)
	assert.EqualValues(t, 175, got.TotalBytes, "deferred PAR2 volumes are not counted")
	assert.Nil(t, got.CategoryID)

	dir := filepath.Join(root, got.ID)
	path := filepath.Join(dir, "Meta.Release.Name.nzb")
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, doc, string(stored), "the raw bytes are kept")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, []string{dir, path}, leftovers(t, root), "no temp files remain")

	srcPath, _, err := e.sources.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, path, srcPath)
	repo, err := e.downloads.Get(context.Background(), core.ID(got.ID))
	require.NoError(t, err)
	assert.Equal(t, core.StateQueued, repo.State)

	evs := events()
	require.Len(t, evs, 1)
	added, ok := evs[0].(core.DownloadAdded)
	require.True(t, ok)
	assert.Equal(t, core.ID(got.ID), added.Download.ID)
}

func TestUploadHonorsFormFieldsInAnyOrder(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, e.cats.Create(context.Background(), cat))
	body, ct := multipartBody(t,
		formPart{"name", "", "My Show S01"},
		formPart{"category_id", "", string(cat.ID)},
		formPart{"nzb", "x.nzb", nzbXML("")},
		formPart{"priority", "", "high"},
		formPart{"paused", "", "true"},
	)

	rec := e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct))

	got := e.addedDownload(rec)
	assert.Equal(t, "My Show S01", got.Name)
	require.NotNil(t, got.CategoryID)
	assert.Equal(t, string(cat.ID), *got.CategoryID)
	assert.Equal(t, "high", got.Priority)
	assert.True(t, got.Paused)
	assert.Equal(t, "queued", got.State)
}

func TestUploadNameFallsBackFromRequestToMetaToFileNameToFirstFileToID(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	noFiles := func(head string) string { return nzbXML(head) }

	byRequest := e.addedDownload(e.upload(key, noFiles(metaName), formPart{"name", "", "Requested"}))
	byMeta := e.addedDownload(e.upload(key, noFiles(metaName)))
	body, ct := multipartBody(t, formPart{"nzb", "From.Upload.File.NZB", noFiles("")})
	byFileName := e.addedDownload(e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct)))
	body, ct = multipartBody(t, formPart{"nzb", "x.nzb", noFiles("")})
	byUploadName := e.addedDownload(e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct)))

	assert.Equal(t, "Requested", byRequest.Name)
	assert.Equal(t, "Meta.Release.Name", byMeta.Name)
	assert.Equal(t, "From.Upload.File", byFileName.Name)
	assert.Equal(t, "x", byUploadName.Name, "the upload file name still beats the file guess")
}

func TestUploadNameUsesFirstFileGuessThenIDWhenNothingElseIsGiven(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	// No file name on the part: a plain form field named nzb is not accepted
	// as a file, so use a part with a blank-ish file name.
	body, ct := multipartBody(t, formPart{"nzb", ".nzb", nzbXML("")})

	got := e.addedDownload(e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct)))

	assert.Equal(t, "show.part1.rar", got.Name)
	noGuess := strings.NewReplacer(`subject="[1/3] - &quot;show.part1.rar&quot; yEnc (1/2)"`, `subject=""`,
		`subject="[2/3] - &quot;show.par2&quot; yEnc (1/1)"`, `subject=""`,
		`subject="[3/3] - &quot;show.vol00+01.par2&quot; yEnc (1/1)"`, `subject=""`).Replace(nzbXML(""))
	body, ct = multipartBody(t, formPart{"nzb", ".nzb", noGuess})
	byID := e.addedDownload(e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct)))
	assert.Equal(t, "nzb-"+byID.ID, byID.Name)
}

func TestUploadDisplayNameIsSanitizedAndNeverPartOfThePath(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	hostile := "../../etc/pass\x00wd\u202e\n" + strings.Repeat("x", 400)

	got := e.addedDownload(e.upload(key, nzbXML(""), formPart{"name", "", hostile}))

	assert.NotContains(t, got.Name, "\x00")
	assert.NotContains(t, got.Name, "\n")
	assert.NotContains(t, got.Name, "\u202e")
	assert.LessOrEqual(t, len(got.Name), 255)
	assert.True(t, strings.HasPrefix(got.Name, "../../etc/passwd"), "it is a display name, kept as typed")
	entries, err := os.ReadDir(filepath.Join(root, got.ID))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.NotContains(t, entries[0].Name(), "/")
	assert.LessOrEqual(t, len(entries[0].Name()), 255)
	assert.True(t, strings.HasSuffix(entries[0].Name(), ".nzb"))
	assert.Equal(t, []string{filepath.Join(root, got.ID), filepath.Join(root, got.ID, entries[0].Name())}, leftovers(t, root))
}

func rawSourcePassword(t *testing.T, e *env, id string) (raw string, isNull bool) {
	t.Helper()
	var ns *string
	require.NoError(t, e.db.Reader().QueryRow(`SELECT password FROM download_sources WHERE download_id = ?`, id).Scan(&ns))
	if ns == nil {
		return "", true
	}
	return *ns, false
}

func TestUploadPasswordPrecedenceIsRequestThenMetaThenNameSuffix(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	metaPW := `<meta type="password">meta-pw</meta>`
	check := func(rec *httptest.ResponseRecorder, wantName, wantPW string) {
		t.Helper()
		got := e.addedDownload(rec)
		assert.Equal(t, wantName, got.Name)
		_, pw, err := e.sources.Get(context.Background(), core.ID(got.ID))
		require.NoError(t, err)
		assert.Equal(t, wantPW, pw.Reveal())
		if wantPW != "" {
			assert.NotContains(t, rec.Body.String(), wantPW, "the password is never echoed")
			raw, isNull := rawSourcePassword(t, e, got.ID)
			assert.False(t, isNull)
			assert.NotContains(t, raw, wantPW, "encrypted at rest")
			assert.True(t, strings.HasPrefix(raw, "v1:"))
		}
	}

	check(e.upload(key, nzbXML(metaPW), formPart{"password", "", "request-pw"}, formPart{"name", "", "Rel{{name-pw}}"}), "Rel", "request-pw")
	check(e.upload(key, nzbXML(metaPW), formPart{"name", "", "Rel{{name-pw}}"}), "Rel", "meta-pw")
	check(e.upload(key, nzbXML(""), formPart{"name", "", "Rel{{name-pw}}"}), "Rel", "name-pw")
	check(e.upload(key, nzbXML(""), formPart{"name", "", "Rel"}), "Rel", "")
}

func TestUploadResolvesTheMetaCategoryByNameAndIgnoresUnknownOnes(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, e.cats.Create(context.Background(), cat))

	known := e.addedDownload(e.upload(key, nzbXML(`<meta type="category">TV</meta>`)))
	unknown := e.addedDownload(e.upload(key, nzbXML(`<meta type="category">TV &gt; HD</meta>`)))

	require.NotNil(t, known.CategoryID)
	assert.Equal(t, string(cat.ID), *known.CategoryID)
	assert.Nil(t, unknown.CategoryID)
}

func TestUploadQueuePositionGoesToTheEndOfTheNZBQueue(t *testing.T) {
	t.Parallel()
	e, _ := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)

	var positions []int
	for range 3 {
		positions = append(positions, e.addedDownload(e.upload(key, nzbXML(""))).QueuePosition)
	}

	assert.Equal(t, []int{0, 1, 2}, positions)
}

func TestUploadRejectsInvalidFieldsWithoutLeftovers(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	for name, tc := range map[string]struct {
		extra  []formPart
		status int
		code   string
	}{
		"unknown category": {[]formPart{{"category_id", "", "nope"}}, 400, "unknown_category"},
		"bad priority":     {[]formPart{{"priority", "", "urgent"}}, 400, "invalid_priority"},
		"bad paused":       {[]formPart{{"paused", "", "maybe"}}, 400, "invalid_paused"},
		"unknown field":    {[]formPart{{"url", "", "http://x"}}, 400, "unknown_field"},
		"duplicate field":  {[]formPart{{"name", "", "a"}, {"name", "", "b"}}, 400, "duplicate_field"},
		"long field":       {[]formPart{{"name", "", strings.Repeat("x", 5000)}}, 400, "field_too_long"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.upload(key, nzbXML(""), tc.extra...)

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
			assert.Empty(t, leftovers(t, root))
		})
	}
	list, _, err := e.downloads.List(context.Background(), core.DownloadQuery{})
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestUploadOfAnInvalidNZBIs400AndLeavesNothing(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	for name, doc := range map[string]string{
		"not xml":    "this is not an nzb",
		"empty":      "",
		"no files":   `<nzb></nzb>`,
		"wrong root": `<html><body>login</body></html>`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.upload(key, doc)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "invalid_nzb", errorOf(t, rec))
			assert.Empty(t, leftovers(t, root))
		})
	}
}

func TestUploadWithoutAnNZBPartIs400(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	body, ct := multipartBody(t, formPart{"name", "", "x"})

	rec := e.do("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", ct))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "missing_nzb", errorOf(t, rec))
	assert.Empty(t, leftovers(t, root))
}

func TestUploadOverTheNZBLimitIs413AndLeavesNothing(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t, withAddOptions(app.WithMaxNZBBytes(1024)))
	key := e.newKey(core.APIKeyScopeWrite)

	rec := e.upload(key, nzbXML("")+strings.Repeat(" ", 2048))

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "nzb_too_large", errorOf(t, rec))
	assert.Empty(t, leftovers(t, root))
}

type spaces struct{}

func (spaces) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestUploadOf64MiBPlusIsRefusedWith413WithoutBufferingIt(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	var head bytes.Buffer
	w := multipart.NewWriter(&head)
	fw, err := w.CreateFormFile("nzb", "big.nzb")
	require.NoError(t, err)
	_, _ = io.WriteString(fw, `<nzb>`)
	body := io.MultiReader(&head, io.LimitReader(spaces{}, 64<<20+1024), strings.NewReader("\r\n--"+w.Boundary()+"--\r\n"))

	r := newRequest("POST", "/api/v1/downloads", body, withBearer(key), withHeader("Content-Type", w.FormDataContentType()))
	rec := e.serve(r)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Empty(t, leftovers(t, root))
}

func TestAddRejectsOtherContentTypesWith415(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	key := e.newKey(core.APIKeyScopeWrite)
	for _, ct := range []string{"text/plain", "application/x-nzb", "application/x-www-form-urlencoded", ""} {
		rec := e.do("POST", "/api/v1/downloads", "<nzb/>", withBearer(key), withHeader("Content-Type", ct))

		assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code, ct)
		assert.Equal(t, "unsupported_media_type", errorOf(t, rec))
	}
	assert.Empty(t, leftovers(t, root))
}

func TestAddNeedsAWriteKeyOrASessionWithCSRF(t *testing.T) {
	t.Parallel()
	e, root := newAddEnv(t)
	rkey := e.newKey(core.APIKeyScopeRead)
	sess := e.login()
	body, ct := multipartBody(t, formPart{"nzb", "x.nzb", nzbXML("")})
	ctMod := withHeader("Content-Type", ct)

	readKey := e.do("POST", "/api/v1/downloads", body, withBearer(rkey), ctMod)
	anonymous := e.do("POST", "/api/v1/downloads", body, ctMod)
	noToken := e.do("POST", "/api/v1/downloads", body, withCookie(sess.cookie), ctMod)
	withToken := e.do("POST", "/api/v1/downloads", body, append(sess.mods(), ctMod)...)

	assert.Equal(t, http.StatusForbidden, readKey.Code)
	assert.Equal(t, "insufficient_scope", errorOf(t, readKey))
	assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
	assert.Equal(t, http.StatusForbidden, noToken.Code)
	assert.Equal(t, "csrf_token_invalid", errorOf(t, noToken))
	assert.Equal(t, http.StatusCreated, withToken.Code, withToken.Body.String())
	assert.Len(t, leftovers(t, root), 2, "only the accepted request left a download dir")
}

func newRequest(method, path string, body io.Reader, mods ...func(*http.Request)) *http.Request {
	r := httptest.NewRequest(method, path, body)
	for _, m := range mods {
		m(r)
	}
	return r
}

// serve runs a prepared request through the full middleware stack.
func (e *env) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, r)
	return rec
}
