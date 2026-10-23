package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func postprocessingForm(names ...string) url.Values {
	v := url.Values{}
	for _, n := range names {
		v.Set(n, "on")
	}
	return v
}

func TestPostProcessingPageShowsBothSwitchesOffByDefault(t *testing.T) {
	_, b := settingsSignedIn(t)

	rec := b.get("/settings/postprocessing")

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	form := byID(doc, "postprocessing")
	require.NotNil(t, form)
	assert.Equal(t, "post", attr(form, "method"))
	assert.Equal(t, "/settings/postprocessing", attr(form, "action"))
	assert.Equal(t, "/settings/postprocessing", attr(form, "hx-post"))
	assert.NotNil(t, find(form, func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "_csrf" }), "works without JavaScript")
	for _, id := range []string{"clean-names", "underscores-to-dots"} {
		in := byID(doc, id)
		require.NotNil(t, in, id)
		assert.Equal(t, "switch", attr(in, "role"), id)
		assert.False(t, hasAttr(in, "checked"), id+" is off by default")
	}
	assert.Equal(t, "clean_names", attr(byID(doc, "clean-names"), "name"))
	assert.Equal(t, "underscores_to_dots", attr(byID(doc, "underscores-to-dots"), "name"))
}

func TestPostProcessingPageExplainsWhatEachSwitchDoes(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/postprocessing").Body.String())

	card := text(byID(doc, "postprocessing-card"))
	for _, want := range []string{
		"Clean release names",
		"-NZBGeek", "-Obfuscated", "[eztv]",
		"As soon as an NZB is added, before anything is downloaded",
		"Clean.py",
		"Show.S01E01.1080p-GRP-Obfuscated", "Show.S01E01.1080p-GRP",
		"Torrents are never renamed",
		"Dots instead of underscores",
		"After unpacking, just before the download is moved to the complete folder",
		"replace_for.py",
		"Show_S01E01_1080p.mkv", "Show.S01E01.1080p.mkv",
		"A torrent's files are never renamed, because they are being seeded",
	} {
		assert.Contains(t, card, want)
	}
	page := text(doc)
	assert.Contains(t, page, "Both apply to the next download.")
	assert.Contains(t, page, "TRaSH guides")
	assert.Contains(t, page, "no scripts to install")
}

func TestPostProcessingPageShowsTheSavedSwitchesOn(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()
	cur.PostProcess = core.PostProcessOptions{UnderscoresToDots: true}
	require.NoError(t, e.settings.Update(t.Context(), cur))

	doc := parseHTML(t, b.get("/settings/postprocessing").Body.String())

	assert.False(t, hasAttr(byID(doc, "clean-names"), "checked"))
	assert.True(t, hasAttr(byID(doc, "underscores-to-dots"), "checked"))
}

func TestSavingEachPostProcessingSwitchPersistsAndLeavesTheOtherAndTheRestAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		form url.Values
		want core.PostProcessOptions
	}{
		"clean names":   {postprocessingForm("clean_names"), core.PostProcessOptions{CleanNames: true}},
		"dots":          {postprocessingForm("underscores_to_dots"), core.PostProcessOptions{UnderscoresToDots: true}},
		"both":          {postprocessingForm("clean_names", "underscores_to_dots"), core.PostProcessOptions{CleanNames: true, UnderscoresToDots: true}},
		"both switched": {url.Values{}, core.PostProcessOptions{}},
	} {
		t.Run(name, func(t *testing.T) {
			e, b := settingsSignedIn(t)
			before := e.settings.Current()
			before.Speed.Global.DownKBps = 100
			before.Queues.Usenet = 3
			before.Network.Usenet.Interface = "lo0"
			before.Torrent.ListenPort = 6881
			before.Seeding.RatioLimit = 2.5
			before.Auth.AllowedHosts = []string{"example.com"}
			require.NoError(t, e.settings.Update(t.Context(), before))

			rec := b.settingsPost("/settings/postprocessing", tc.form)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			after := e.settings.Current()
			assert.Equal(t, tc.want, after.PostProcess)
			before.PostProcess = tc.want
			assert.Equal(t, before, after, "everything else is untouched")
			level, msg := settingsToastOf(t, rec)
			assert.Equal(t, "success", level)
			assert.Contains(t, msg, "Post-processing options saved")
			assert.Contains(t, msg, "next download")
			assert.NotContains(t, rec.Body.String(), "<html", "the card only")
			assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "postprocessing-card"))
		})
	}
}

func TestSavingOneSwitchOffKeepsTheOtherOn(t *testing.T) {
	e, b := settingsSignedIn(t)
	cur := e.settings.Current()
	cur.PostProcess = core.PostProcessOptions{CleanNames: true, UnderscoresToDots: true}
	require.NoError(t, e.settings.Update(t.Context(), cur))

	rec := b.settingsPost("/settings/postprocessing", postprocessingForm("clean_names"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, core.PostProcessOptions{CleanNames: true}, e.settings.Current().PostProcess)
	doc := parseHTML(t, rec.Body.String())
	assert.True(t, hasAttr(byID(doc, "clean-names"), "checked"))
	assert.False(t, hasAttr(byID(doc, "underscores-to-dots"), "checked"))
}

func TestPostProcessingPlainFormPostRendersThePageAgain(t *testing.T) {
	e, b := settingsSignedIn(t)
	form := postprocessingForm("clean_names")
	form.Set("_csrf", b.csrf())

	rec := b.post("/settings/postprocessing", form)

	require.Equal(t, http.StatusOK, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.NotNil(t, find(doc, func(n *html.Node) bool { return n.Data == "html" }), "a whole page")
	assert.Contains(t, text(byID(doc, "postprocessing-card")), "Your changes were saved")
	assert.True(t, e.settings.Current().PostProcess.CleanNames)
}

func TestPostProcessingPostNeedsTheCSRFToken(t *testing.T) {
	e, b := settingsSignedIn(t)

	rec := b.post("/settings/postprocessing", postprocessingForm("clean_names"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.False(t, e.settings.Current().PostProcess.CleanNames)
}

func TestPostProcessingIsInTheSettingsNavigationBetweenSpeedAndTorrent(t *testing.T) {
	_, b := settingsSignedIn(t)

	nav := settingsNav(t, b.get("/settings/postprocessing").Body.String())

	var hrefs []string
	for _, a := range findAll(nav, func(n *html.Node) bool { return n.Data == "a" }) {
		hrefs = append(hrefs, attr(a, "href"))
	}
	assert.Equal(t, []string{"/settings/paths", "/settings/servers", "/settings/speed", "/settings/postprocessing", "/settings/torrent", "/settings/security"}, hrefs)
}

// addPostProcessingResponses adds the pages and cards of the post-processing
// section to the shared HTML invariant tests.
func addPostProcessingResponses(t *testing.T, out map[string]*httptest.ResponseRecorder) {
	t.Helper()
	_, b := settingsSignedIn(t)
	out["POST /settings/postprocessing (saved)"] = b.settingsPost("/settings/postprocessing", postprocessingForm("clean_names"))
	plain := postprocessingForm("underscores_to_dots")
	plain.Set("_csrf", b.csrf())
	out["POST /settings/postprocessing (plain form)"] = b.post("/settings/postprocessing", plain)
	out["GET /settings/postprocessing (saved)"] = b.get("/settings/postprocessing")
}
