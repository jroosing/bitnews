package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// pathsForm is a valid paths form for four existing directories.
func pathsForm(t *testing.T) url.Values {
	t.Helper()
	root := t.TempDir()
	v := url.Values{}
	for _, name := range []string{"usenet_incomplete", "usenet_complete", "torrent_incomplete", "torrent_complete"} {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		v.Set(name, dir)
	}
	return v
}

func settingsToastOf(t *testing.T, rec *httptest.ResponseRecorder) (level, message string) {
	t.Helper()
	h := rec.Header().Get("HX-Trigger")
	require.NotEmpty(t, h, "HX-Trigger")
	var m struct {
		Toast struct{ Level, Message string } `json:"toast"`
	}
	require.NoError(t, json.Unmarshal([]byte(h), &m))
	return m.Toast.Level, m.Toast.Message
}

func TestPathsPageShowsTheCurrentFoldersInLabelledGroups(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()

	doc := parseHTML(t, b.get("/settings/paths").Body.String())

	form := byID(doc, "paths")
	require.NotNil(t, form)
	assert.Equal(t, "post", attr(form, "method"))
	assert.Equal(t, "/settings/paths", attr(form, "action"))
	assert.Equal(t, "/settings/paths", attr(form, "hx-post"))
	legends := findAll(form, func(n *html.Node) bool { return n.Data == "legend" })
	require.Len(t, legends, 2)
	assert.Equal(t, "Usenet", text(legends[0]))
	assert.Equal(t, "Torrents", text(legends[1]))
	for id, want := range map[string]string{
		"usenet-incomplete":  cur.Paths.Usenet.Incomplete,
		"usenet-complete":    cur.Paths.Usenet.Complete,
		"torrent-incomplete": cur.Paths.Torrent.Incomplete,
		"torrent-complete":   cur.Paths.Torrent.Complete,
	} {
		in := byID(doc, id)
		require.NotNil(t, in, id)
		assert.Equal(t, want, attr(in, "value"), id)
	}
	body := text(doc)
	assert.Contains(t, body, "same filesystem")
	assert.Contains(t, body, "Sonarr and Radarr")
	assert.Contains(t, body, "create missing folders")
	assert.Contains(t, body, "new downloads only")
}

func TestSavingFoldersChangesOnlyThePathsOfTheSettings(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	before.Auth.AllowedHosts = []string{"example.com"}
	before.Network.Usenet.Interface = "lo0"
	before.Seeding.RatioLimit = 2.5
	before.Queues.Usenet = 3
	require.NoError(t, e.settings.Update(context.Background(), before))
	form := pathsForm(t)

	rec := b.settingsPost("/settings/paths", form)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := e.settings.Current()
	assert.Equal(t, form.Get("usenet_incomplete"), after.Paths.Usenet.Incomplete)
	assert.Equal(t, form.Get("usenet_complete"), after.Paths.Usenet.Complete)
	assert.Equal(t, form.Get("torrent_incomplete"), after.Paths.Torrent.Incomplete)
	assert.Equal(t, form.Get("torrent_complete"), after.Paths.Torrent.Complete)
	before.Paths = after.Paths
	assert.Equal(t, before, after, "auth, network, seeding and queue settings are untouched")
	level, msg := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	assert.Contains(t, msg, "saved")
	// The answer is the card only, ready to swap in.
	assert.NotContains(t, rec.Body.String(), "<html")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "paths-card"))
}

func TestARelativeFolderIsRejectedNextToItsField(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	form := pathsForm(t)
	form.Set("usenet_complete", "relative/dir")
	form.Set("torrent_incomplete", "")

	rec := b.settingsPost("/settings/paths", form)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, before, e.settings.Current(), "nothing is saved")
	doc := parseHTML(t, rec.Body.String())
	in := byID(doc, "usenet-complete")
	assert.Equal(t, "relative/dir", attr(in, "value"), "what was typed stays")
	assert.Equal(t, "true", attr(in, "aria-invalid"))
	assert.Contains(t, attr(in, "aria-describedby"), "usenet-complete-error")
	assert.Contains(t, text(byID(doc, "usenet-complete-error")), "absolute")
	assert.Contains(t, text(byID(doc, "torrent-incomplete-error")), "Enter the full path")
	assert.Nil(t, byID(doc, "usenet-incomplete-error"), "the good fields have no error")
	assert.Equal(t, "alert", attr(byID(doc, "paths-error"), "role"))
	assert.Equal(t, "true", attr(byID(doc, "paths"), "data-dirty"), "Save stays enabled")
}

func TestFolderWithControlCharactersIsRejected(t *testing.T) {
	_, b := settingsSignedIn(t)
	form := pathsForm(t)
	form.Set("usenet_complete", "/data/a\x00b")

	rec := b.settingsPost("/settings/paths", form)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "usenet-complete-error")), "control characters")
}

func TestFoldersAreCleanedBeforeTheyAreSaved(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := pathsForm(t)
	dir := form.Get("usenet_complete")
	form.Set("usenet_complete", dir+"/")

	rec := b.settingsPost("/settings/paths", form)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, dir, e.settings.Current().Paths.Usenet.Complete)
}

func TestSavingFoldersMakesNoClaimAboutTheDisk(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.settingsPost("/settings/paths", pathsForm(t))

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Nil(t, byID(doc, "paths-warnings"), "no checks, so no verdict")
	body := strings.ToLower(text(doc))
	for _, claim := range []string{"look good", "writable", "does not exist", "different filesystems"} {
		assert.NotContains(t, body, claim)
	}
}

func TestAMissingButWellFormedFolderSavesWithoutAWarningOrAProbe(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := pathsForm(t)
	missing := filepath.Join(t.TempDir(), "not-there-yet")
	form.Set("usenet_incomplete", missing)

	rec := b.settingsPost("/settings/paths", form)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, missing, e.settings.Current().Paths.Usenet.Incomplete, "saved")
	assert.Nil(t, byID(parseHTML(t, rec.Body.String()), "paths-warnings"))
	level, _ := settingsToastOf(t, rec)
	assert.Equal(t, "success", level)
	_, statErr := os.Stat(missing)
	assert.True(t, os.IsNotExist(statErr), "saving never creates the folder")
}

func TestAFolderWithDotDotIsRejectedNextToItsField(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	form := pathsForm(t)
	form.Set("torrent_complete", "/data/torrents/../etc")

	rec := b.settingsPost("/settings/paths", form)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, before, e.settings.Current(), "nothing is saved")
	doc := parseHTML(t, rec.Body.String())
	assert.Equal(t, "/data/torrents/../etc", attr(byID(doc, "torrent-complete"), "value"), "what was typed stays")
	assert.Contains(t, text(byID(doc, "torrent-complete-error")), "..")
	assert.Nil(t, byID(doc, "torrent-incomplete-error"))
}

func TestNestedFoldersAreRejectedOnTheInnerField(t *testing.T) {
	e, b := settingsSignedIn(t)
	before := e.settings.Current()
	form := pathsForm(t)
	incomplete := form.Get("usenet_incomplete")
	form.Set("usenet_complete", filepath.Join(incomplete, "done"))

	rec := b.settingsPost("/settings/paths", form)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Equal(t, before, e.settings.Current(), "nothing is saved")
	doc := parseHTML(t, rec.Body.String())
	msg := text(byID(doc, "usenet-complete-error"))
	assert.Contains(t, msg, "inside")
	assert.Contains(t, msg, incomplete)
	assert.Nil(t, byID(doc, "usenet-incomplete-error"), "the outer folder is fine")
	assert.Equal(t, "alert", attr(byID(doc, "paths-error"), "role"))
}

func TestPlainFormPostRendersThePageAgain(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := pathsForm(t)

	rec := b.post("/settings/paths", form)

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, find(doc, func(n *html.Node) bool { return n.Data == "html" }), "a whole page")
	assert.Contains(t, text(byID(doc, "paths-card")), "Your changes were saved")
	assert.Equal(t, form.Get("usenet_complete"), e.settings.Current().Paths.Usenet.Complete)

	form.Set("usenet_complete", "nope")
	rec = b.post("/settings/paths", form)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "usenet-complete-error"))
}

func TestMutatingSettingsRoutesNeedASessionAndTheSameSite(t *testing.T) {
	e, b := settingsSignedIn(t)
	cat, err := e.cats.Create(context.Background(), "tv", "")
	require.NoError(t, err)
	srv := mustCreateServer(t, e, "main")
	form := pathsForm(t)

	routes := []struct {
		method, path string
		form         url.Values
	}{
		{"POST", "/settings/paths", form},
		{"POST", "/settings/categories", url.Values{"name": {"movies"}}},
		{"POST", "/settings/categories/" + string(cat.ID), url.Values{"dir_override": {"/x"}}},
		{"DELETE", "/settings/categories/" + string(cat.ID), nil},
		{"POST", "/settings/servers", serverForm("other")},
		{"POST", "/settings/servers/test", serverForm("other")},
		{"POST", "/settings/servers/" + string(srv.ID), serverForm("main")},
		{"PATCH", "/settings/servers/" + string(srv.ID), url.Values{"enabled": {"on"}}},
		{"DELETE", "/settings/servers/" + string(srv.ID), nil},
		{"POST", "/settings/servers/" + string(srv.ID) + "/move/down", nil},
		{"POST", "/settings/servers/" + string(srv.ID) + "/test", nil},
	}
	for _, rt := range routes {
		send := func(br *browser, extra ...reqOpt) *httptest.ResponseRecorder {
			r := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://example.com")
			for _, o := range extra {
				o(r)
			}
			return br.send(r)
		}

		anon := send(e.browser())
		assert.Equal(t, http.StatusSeeOther, anon.Code, "%s %s signed out", rt.method, rt.path)
		assert.Equal(t, "/login", location(anon), "%s %s", rt.method, rt.path)

		crossSite := send(b, header("Origin", "https://evil.test"))
		assert.Equal(t, http.StatusForbidden, crossSite.Code, "%s %s from another origin", rt.method, rt.path)
	}
	// Nothing above had any effect.
	cats, _ := e.cats.List(context.Background())
	assert.Len(t, cats, 1)
	servers, _ := e.servers.List(context.Background())
	require.Len(t, servers, 1)
	assert.True(t, servers[0].Enabled)
}
