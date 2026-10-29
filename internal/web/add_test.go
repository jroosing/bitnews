package web_test

import (
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
	"golang.org/x/net/html"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/core/coretest"
)

var showEntries = []coretest.TorrentEntry{
	{Path: []string{"ep1.mkv"}, Size: 20000},
	{Path: []string{"extras", "readme.txt"}, Size: 5},
}

const magnetHash = "0123456789abcdef0123456789abcdef01234567"

func magnetLink() string {
	return "magnet:?xt=urn:btih:" + magnetHash + "&dn=Some.Magnet&tr=http%3A%2F%2Ftracker.example%2Fannounce%3Fpasskey%3DSECRET"
}

// addedDownloads lists every Download in the store.
func (d *denv) addedDownloads() []core.Download {
	d.t.Helper()
	items, _, err := d.repo.List(d.t.Context(), core.DownloadQuery{})
	require.NoError(d.t, err)
	return items
}

// dialogErrors is the text of the dialog's alerts and field errors.
func dialogErrors(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	doc := parseHTML(t, rec.Body.String())
	var out []string
	for _, n := range findAll(doc, func(n *html.Node) bool {
		return attr(n, "role") == "alert" || strings.Contains(attr(n, "class"), "field-error")
	}) {
		out = append(out, text(n))
	}
	return strings.Join(out, " | ")
}

func optionTexts(sel *html.Node) []string {
	var out []string
	for _, o := range findAll(sel, func(n *html.Node) bool { return n.Data == "option" }) {
		out = append(out, text(o))
	}
	return out
}

func TestAddFormIsAFragmentWithTabsCategoriesAndDefaults(t *testing.T) {
	d := newDetailEnv(t)
	_, err := d.catSvc.Create(t.Context(), "tv", "")
	require.NoError(t, err)

	rec := d.b.get("/downloads/add", htmx())

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "<html")
	doc := parseHTML(t, rec.Body.String())
	form := byID(doc, "add-form")
	require.NotNil(t, form)
	assert.Equal(t, "post", attr(form, "method"))
	assert.Equal(t, "multipart/form-data", attr(form, "enctype"))
	assert.Equal(t, "/downloads/add", attr(form, "hx-post"))
	assert.Equal(t, "multipart/form-data", attr(form, "hx-encoding"))
	for _, id := range []string{"add-file", "add-link", "add-name", "add-category", "add-priority", "add-password", "add-paused"} {
		assert.NotNil(t, byID(form, id), id)
	}
	assert.Equal(t, []string{"No category", "tv"}, optionTexts(byID(form, "add-category")))
	tabs := findAll(form, func(n *html.Node) bool { return attr(n, "role") == "tab" })
	require.Len(t, tabs, 2)
	for _, tab := range tabs {
		panel := byID(form, attr(tab, "aria-controls"))
		require.NotNil(t, panel)
		assert.Equal(t, "tabpanel", attr(panel, "role"))
		assert.Equal(t, attr(tab, "id"), attr(panel, "aria-labelledby"))
	}
	zone := find(form, func(n *html.Node) bool { return n.Data == "label" && strings.Contains(attr(n, "class"), "dropzone") })
	require.NotNil(t, zone)
	assert.Equal(t, "add-file", attr(zone, "for"))
	for _, a := range []string{"@dragenter.prevent", "@dragover.prevent", "@dragleave.prevent", "@drop.prevent"} {
		assert.True(t, hasAttr(zone, a), a)
	}
}

func TestAddFormFromABrowserNavigationGoesToTheQueue(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.b.get("/downloads/add")

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/queue", location(rec))
}

func TestEveryPageHasTheNewDownloadButtonAndDialog(t *testing.T) {
	d := newDetailEnv(t)

	for _, page := range []string{"/queue", "/history", "/ui/styleguide"} {
		doc := parseHTML(t, d.b.get(page).Body.String())

		btn := find(doc, func(n *html.Node) bool { return n.Data == "button" && hasAttr(n, "data-add-open") })
		require.NotNil(t, btn, page)
		assert.Contains(t, text(btn), "New download", page)
		dlg := byID(doc, "add-dialog")
		require.NotNil(t, dlg, page)
		assert.Equal(t, "dialog", dlg.Data)
		assert.Equal(t, "add-dialog-title", attr(dlg, "aria-labelledby"), page)
		assert.NotNil(t, byID(dlg, "add-dialog-title"), page)
		body := byID(dlg, "add-dialog-body")
		assert.Equal(t, "/downloads/add", attr(body, "hx-get"), page)
		assert.Equal(t, "add-open from:window", attr(body, "hx-trigger"), page)
		assert.NotNil(t, find(dlg, func(n *html.Node) bool { return n.Data == "button" && attr(n, "aria-label") == "Close the dialog" }), page)
	}
}

func TestAddUploadedNZBCreatesAQueuedDownload(t *testing.T) {
	d := newDetailEnv(t)
	cat, err := d.catSvc.Create(t.Context(), "tv", "")
	require.NoError(t, err)

	rec := d.upload(
		formPart{name: "file", filename: "My.Show.nzb", value: nzbXML("Meta.Name")},
		formPart{name: "name", value: "Custom Name"}, formPart{name: "category_id", value: string(cat.ID)},
		formPart{name: "priority", value: "high"}, formPart{name: "paused", value: "on"},
	)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := d.addedDownloads()
	require.Len(t, got, 1)
	assert.Equal(t, "Custom Name", got[0].Name)
	assert.Equal(t, core.SourceNZB, got[0].SourceKind)
	assert.Equal(t, cat.ID, got[0].CategoryID)
	assert.Equal(t, core.DownloadPriorityHigh, got[0].Priority)
	assert.True(t, got[0].Paused)
	trigger := rec.Header().Get("HX-Trigger")
	assert.Contains(t, trigger, `"download-added":{"id":"`+string(got[0].ID)+`"}`)
	assert.Contains(t, trigger, "queue-refresh")
	assert.Contains(t, trigger, "Added")
	assert.Contains(t, trigger, "Custom Name")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "add-form"), "a fresh form")
}

func TestAddNZBTakesTheNameFromTheFileWhenNoneIsGiven(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "file", filename: "Release.Name.nzb", value: strings.ReplaceAll(nzbXML("x"), `<meta type="name">x</meta>`, "")})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "Release.Name", d.addedDownloads()[0].Name)
}

func TestAddFieldsMayComeBeforeTheFile(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "name", value: "First"}, formPart{name: "file", filename: "a.nzb", value: nzbXML("m")})

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "First", d.addedDownloads()[0].Name)
}

func TestAddUploadedTorrentCreatesATorrentDownload(t *testing.T) {
	d := newDetailEnv(t)
	data, _ := coretest.BuildTorrent("Show.S01", showEntries)

	rec := d.upload(formPart{name: "file", filename: "show.torrent", value: string(data)}, formPart{name: "priority", value: "normal"})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := d.addedDownloads()
	require.Len(t, got, 1)
	assert.Equal(t, core.SourceTorrent, got[0].SourceKind)
	assert.Equal(t, "Show.S01", got[0].Name)
	assert.Equal(t, core.StateQueued, got[0].State)
	_, err := os.Stat(filepath.Join(d.root, "torrents-incomplete", string(got[0].ID)))
	assert.NoError(t, err)
}

func TestAddDecidesByContentNotByExtensionAlone(t *testing.T) {
	d := newDetailEnv(t)
	data, _ := coretest.BuildTorrent("Odd.Name", showEntries)

	rec := d.upload(formPart{name: "file", filename: "download.bin", value: string(data)})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, core.SourceTorrent, d.addedDownloads()[0].SourceKind)

	d2 := newDetailEnv(t)
	rec = d2.upload(formPart{name: "file", filename: "download.xml", value: nzbXML("x")})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, core.SourceNZB, d2.addedDownloads()[0].SourceKind)
}

func TestAddRefusesAnExtensionThatContradictsTheContent(t *testing.T) {
	d := newDetailEnv(t)
	data, _ := coretest.BuildTorrent("T", showEntries)

	for name, part := range map[string]formPart{
		"torrent named nzb": {name: "file", filename: "a.nzb", value: string(data)},
		"nzb named torrent": {name: "file", filename: "a.torrent", value: nzbXML("x")},
	} {
		rec := d.upload(part)

		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Contains(t, dialogErrors(t, rec), "ends in", name)
	}
	assert.Empty(t, d.addedDownloads())
}

func TestAddInvalidFilesAreDialogErrorsAndLeaveNothingBehind(t *testing.T) {
	d := newDetailEnv(t)

	for name, tc := range map[string]struct {
		part formPart
		want string
	}{
		"bad nzb":     {formPart{name: "file", filename: "a.nzb", value: "<nzb>nope"}, "not a valid NZB"},
		"empty nzb":   {formPart{name: "file", filename: "a.nzb", value: `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"></nzb>`}, "not a valid NZB"},
		"bad torrent": {formPart{name: "file", filename: "a.torrent", value: "d3:foo3:bare"}, "not a valid torrent"},
		"empty file":  {formPart{name: "file", filename: "a.nzb", value: ""}, "empty"},
	} {
		rec := d.upload(tc.part)

		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Contains(t, dialogErrors(t, rec), tc.want, name)
		assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "add-form"), name)
	}
	assert.Empty(t, d.addedDownloads())
	for _, dir := range []string{"incomplete", "torrents-incomplete"} {
		entries, _ := os.ReadDir(filepath.Join(d.root, dir))
		assert.Empty(t, entries, dir)
	}
}

func TestAddWithoutAFileOrLinkAsksForOne(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "name", value: "x"})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, dialogErrors(t, rec), "Choose an NZB or torrent file, or paste a link.")
	doc := parseHTML(t, rec.Body.String())
	assert.Equal(t, "x", attr(byID(doc, "add-name"), "value"), "the typed values stay")
}

func TestAddAFileAndALinkTogetherIsRefused(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "file", filename: "a.nzb", value: nzbXML("x")}, formPart{name: "link", value: magnetLink()})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, dialogErrors(t, rec), "not both")
	assert.Empty(t, d.addedDownloads())
}

func TestAddMagnetLink(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "link", value: "  " + magnetLink() + "  "}, formPart{name: "mode", value: "link"})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := d.addedDownloads()
	require.Len(t, got, 1)
	assert.Equal(t, core.SourceTorrent, got[0].SourceKind)
	assert.Equal(t, core.StateFetchingMetadata, got[0].State)
	assert.Equal(t, "Some.Magnet", got[0].Name)
	assert.NotContains(t, d.log.String(), "SECRET", "the passkey never reaches the log")
}

func TestAddInvalidMagnetShowsAnErrorAndKeepsTheLink(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "link", value: "magnet:?xt=urn:btih:nothex"}, formPart{name: "mode", value: "link"})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Contains(t, text(byID(doc, "add-link-error")), "magnet")
	assert.Equal(t, "magnet:?xt=urn:btih:nothex", attr(byID(doc, "add-link"), "value"))
	assert.Equal(t, "true", attr(byID(doc, "add-link"), "aria-invalid"))
	assert.Equal(t, "link", attr(byID(doc, "add-form"), "data-mode"), "the link tab stays open")
}

// indexer serves an NZB, a torrent and a redirect to a magnet link.
func indexer(t *testing.T) *httptest.Server {
	t.Helper()
	torrent, _ := coretest.BuildTorrent("From.Indexer", showEntries)
	mux := http.NewServeMux()
	mux.HandleFunc("/get/file.nzb", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(nzbXML("Indexer.NZB"))) })
	mux.HandleFunc("/get/file.torrent", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(torrent) })
	mux.HandleFunc("/get/magnet", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, magnetLink(), http.StatusFound)
	})
	mux.HandleFunc("/get/gone", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAddFromURLs(t *testing.T) {
	srv := indexer(t)
	for path, want := range map[string]struct {
		kind core.SourceKind
		name string
	}{
		"/get/file.nzb":     {core.SourceNZB, "Indexer.NZB"},
		"/get/file.torrent": {core.SourceTorrent, "From.Indexer"},
		"/get/magnet":       {core.SourceTorrent, "Some.Magnet"},
	} {
		d := newDetailEnv(t)

		rec := d.upload(formPart{name: "link", value: srv.URL + path}, formPart{name: "mode", value: "link"})

		require.Equal(t, http.StatusOK, rec.Code, path+rec.Body.String())
		got := d.addedDownloads()
		require.Len(t, got, 1, path)
		assert.Equal(t, want.kind, got[0].SourceKind, path)
		assert.Equal(t, want.name, got[0].Name, path)
	}
}

func TestAddFromAFailingURLShowsTheUpstreamError(t *testing.T) {
	srv := indexer(t)
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "link", value: srv.URL + "/get/gone"}, formPart{name: "mode", value: "link"})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, text(byID(parseHTML(t, rec.Body.String()), "add-link-error")), "404")
	rec = d.upload(formPart{name: "link", value: "ftp://example.com/x.nzb"}, formPart{name: "mode", value: "link"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, dialogErrors(t, rec), "http or https")
}

func TestAddDuplicateTorrentLinksToTheExistingDownload(t *testing.T) {
	d := newDetailEnv(t)
	first := d.upload(formPart{name: "link", value: magnetLink()}, formPart{name: "mode", value: "link"})
	require.Equal(t, http.StatusOK, first.Code)
	existing := d.addedDownloads()[0]

	rec := d.upload(formPart{name: "link", value: magnetLink()}, formPart{name: "mode", value: "link"})

	assert.Equal(t, http.StatusConflict, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	alert := byID(doc, "add-error")
	require.NotNil(t, alert)
	assert.Contains(t, text(alert), "Already added")
	assert.Contains(t, text(alert), "Some.Magnet")
	link := find(alert, func(n *html.Node) bool { return n.Data == "a" })
	require.NotNil(t, link)
	assert.Equal(t, "/downloads/"+string(existing.ID), attr(link, "href"))
	assert.Len(t, d.addedDownloads(), 1)
}

func TestAddUnknownCategoryAndPriorityAreFieldErrors(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "file", filename: "a.nzb", value: nzbXML("x")}, formPart{name: "category_id", value: string(core.NewID())})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	doc := parseHTML(t, rec.Body.String())
	assert.Contains(t, text(byID(doc, "add-category-error")), "does not exist")
	assert.Equal(t, "true", attr(byID(doc, "add-category"), "aria-invalid"))

	rec = d.upload(formPart{name: "file", filename: "a.nzb", value: nzbXML("x")}, formPart{name: "priority", value: "urgent"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "add-priority-error"))
	assert.Empty(t, d.addedDownloads())
}

func TestAddWithoutTorrentSupportExplainsIt(t *testing.T) {
	d := newDetailEnv(t, withoutTorrentEngine())
	data, _ := coretest.BuildTorrent("T", showEntries)

	for _, parts := range [][]formPart{
		{{name: "file", filename: "a.torrent", value: string(data)}},
		{{name: "link", value: magnetLink()}},
	} {
		rec := d.upload(parts...)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, dialogErrors(t, rec), "Torrents are not available")
	}
	rec := d.upload(formPart{name: "file", filename: "a.nzb", value: nzbXML("x")})
	assert.Equal(t, http.StatusOK, rec.Code, "NZBs still work")
}

func TestAddWithoutTheAddServiceSaysSo(t *testing.T) {
	d := newDetailEnv(t, withoutAdder())

	assert.Contains(t, text(parseHTML(t, d.b.get("/downloads/add", htmx()).Body.String())), "not available")
	rec := d.upload(formPart{name: "file", filename: "a.nzb", value: nzbXML("x")})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestAddOversizedUploadIsA413DialogError(t *testing.T) {
	d := newDetailEnv(t)
	huge := strings.Repeat("A", 65<<20+10)

	rec := d.upload(formPart{name: "file", filename: "huge.nzb", value: huge})

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(t, dialogErrors(t, rec), "larger than")
	assert.NotNil(t, byID(parseHTML(t, rec.Body.String()), "add-form"))
	assert.Empty(t, d.addedDownloads())
	entries, _ := os.ReadDir(filepath.Join(d.root, "incomplete"))
	assert.Empty(t, entries)
}

func TestAddOversizedTorrentFile(t *testing.T) {
	d := newDetailEnv(t)
	huge := "d" + strings.Repeat("x", 11<<20)

	rec := d.upload(formPart{name: "file", filename: "huge.torrent", value: huge})

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(t, dialogErrors(t, rec), "larger than")
}

func TestAddRefusesMalformedForms(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.postRaw("/downloads/add", "name=x", "application/x-www-form-urlencoded")
	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)

	rec = d.postRaw("/downloads/add", "not multipart", "multipart/form-data; boundary=zzz")
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	for name, parts := range map[string][]formPart{
		"unknown field":   {{name: "bogus", value: "x"}},
		"duplicate field": {{name: "name", value: "a"}, {name: "name", value: "b"}},
		"two files":       {{name: "file", filename: "a.nzb", value: nzbXML("a")}, {name: "file", filename: "b.nzb", value: nzbXML("b")}},
		"long field":      {{name: "name", value: strings.Repeat("a", 5000)}},
	} {
		rec := d.upload(parts...)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
	}
	parts := make([]formPart, 20)
	for i := range parts {
		parts[i] = formPart{name: fmt.Sprintf("f%d", i), value: "x"}
	}
	assert.Equal(t, http.StatusBadRequest, d.upload(parts...).Code)
	assert.Empty(t, d.addedDownloads())
}

func TestAddHostileNamesAreEscapedInTheToastAndForm(t *testing.T) {
	d := newDetailEnv(t)

	rec := d.upload(formPart{name: "file", filename: "a.nzb", value: nzbXML("x")}, formPart{name: "name", value: `<img src=x onerror=alert(1)>é` + "\u202e"})

	require.Equal(t, http.StatusOK, rec.Code)
	for _, r := range rec.Header().Get("HX-Trigger") {
		assert.Less(t, r, rune(0x7f), "the header is ASCII")
	}
	assert.NotContains(t, rec.Body.String(), "<img")

	rec = d.upload(formPart{name: "name", value: `"><script>alert(1)</script>`})
	assert.NotContains(t, rec.Body.String(), "<script>alert")
}

func TestAddNeedsASessionAndTheSameSite(t *testing.T) {
	d := newDetailEnv(t)
	body, ct := multipartBody(t, formPart{name: "file", filename: "a.nzb", value: nzbXML("x")})

	rec := d.postRaw("/downloads/add", body, ct, header("Sec-Fetch-Site", "cross-site"))
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = d.postRaw("/downloads/add", body, ct, header("Origin", "https://evil.test"))
	assert.Equal(t, http.StatusForbidden, rec.Code)

	r := httptest.NewRequest(http.MethodPost, "/downloads/add", strings.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", "http://example.com")
	anon := d.env.browser().send(r)
	assert.NotEqual(t, http.StatusOK, anon.Code)
	assert.Empty(t, d.addedDownloads())
}

func TestAddWithoutHTMXRedirectsToTheNewDownload(t *testing.T) {
	d := newDetailEnv(t)
	body, ct := multipartBody(t, formPart{name: "file", filename: "a.nzb", value: nzbXML("x")})
	r := httptest.NewRequest(http.MethodPost, "/downloads/add", strings.NewReader(body))
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", "http://example.com")

	rec := d.b.send(r)

	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/downloads/"+string(d.addedDownloads()[0].ID), location(rec))
}

func TestUploadLimitAppliesOnlyToTheAddRoute(t *testing.T) {
	d := newDetailEnv(t)
	big := url.Values{"x": {strings.Repeat("a", 70<<10)}}

	rec := d.act("/downloads/"+string(core.NewID())+"/delete", big)

	assert.Equal(t, http.StatusBadRequest, rec.Code, "ordinary forms keep the 64 KiB cap")
}

// The form posts every named control it renders, so a field the server does
// not know would break the dialog for real browsers.
func TestAddAcceptsEveryFieldTheFormRenders(t *testing.T) {
	d := newDetailEnv(t)
	doc := parseHTML(t, d.b.get("/downloads/add", htmx()).Body.String())
	var parts []formPart
	for _, in := range findAll(byID(doc, "add-form"), func(n *html.Node) bool {
		return (n.Data == "input" || n.Data == "select") && attr(n, "name") != "" && attr(n, "type") != "file" && attr(n, "type") != "checkbox"
	}) {
		value := attr(in, "value")
		if in.Data == "select" {
			value = attr(find(in, func(n *html.Node) bool { return n.Data == "option" && hasAttr(n, "selected") }), "value")
		}
		if attr(in, "name") == "link" {
			continue
		}
		parts = append(parts, formPart{name: attr(in, "name"), value: value})
	}
	parts = append(parts, formPart{name: "file", filename: "a.nzb", value: nzbXML("x")})

	rec := d.upload(parts...)

	require.Equal(t, http.StatusOK, rec.Code, dialogErrors(t, rec))
	assert.Len(t, d.addedDownloads(), 1)
}

func TestAddFormKeepsTheModeOfTheTabInAHiddenField(t *testing.T) {
	d := newDetailEnv(t)

	doc := parseHTML(t, d.b.get("/downloads/add", htmx()).Body.String())

	mode := find(byID(doc, "add-form"), func(n *html.Node) bool { return n.Data == "input" && attr(n, "name") == "mode" })
	require.NotNil(t, mode)
	assert.Equal(t, "hidden", attr(mode, "type"))
	assert.Equal(t, "mode", attr(mode, ":value"))
}

// htmx copies the "style" attribute of old elements to the new ones with the
// same id while it settles a swap. That is an inline style (blocked by the
// CSP) and wipes the display Alpine's x-show set, so it must be off.
func TestHTMXDoesNotSettleStyleAttributes(t *testing.T) {
	d := newDetailEnv(t)

	doc := parseHTML(t, d.b.get("/history").Body.String())

	cfg := find(doc, func(n *html.Node) bool { return n.Data == "meta" && attr(n, "name") == "htmx-config" })
	require.NotNil(t, cfg)
	assert.Contains(t, attr(cfg, "content"), `"attributesToSettle":["class","width","height"]`)
}
