package blackbox_test

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// Adding NZB Downloads: the multipart upload and the JSON {url} of
// POST /api/v1/downloads, with every refusal the Go daemon has. No Server is
// configured, so an added Download stays queued and nothing is fetched.

// startAdder starts a daemon with data roots in temp dirs and no Server.
func startAdder(t *testing.T) (*daemon, *usenetSetup) {
	t.Helper()
	u := newUsenetSetup(t, newTestCA(t), nil)
	return u.start(), u
}

const downloadsPath = "/api/v1/downloads"

// requireNothingAdded asserts the refusal left no Download and no staged
// files behind.
func requireNothingAdded(t *testing.T, d *daemon, u *usenetSetup) {
	t.Helper()
	assert.Empty(t, itemsOf(t, d.apiGet(downloadsPath)), "no Download")
	ents, err := os.ReadDir(u.roots.usenetIncomplete)
	require.NoError(t, err)
	assert.Empty(t, ents, "nothing staged in the incomplete root")
}

func smallNZB(t testing.TB, opts ...relOption) release {
	return newRelease(t, []relSpec{{Name: "first.bin", Data: randBytes(500, 4000)}, {Name: "second.bin", Data: randBytes(501, 1000)}}, opts...)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUsenetAddByUploadAnswersTheNewDownloadAndStoresTheNZB(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	ev := d.watch()
	rel := smallNZB(t)

	before := time.Now().Add(-2 * time.Second)
	r := d.upload("My.Release.nzb", rel.NZB)

	require.Equal(t, http.StatusCreated, r.status, r.body)
	requireAPIHeaders(t, r)
	m := obj(t, r)
	assert.ElementsMatch(t, []string{"id", "name", "source_kind", "category_id", "state", "priority", "queue_position", "paused",
		"total_bytes", "done_bytes", "added_at", "fetched_at", "ready_at", "updated_at"}, keysOf(m), "no failure or output fields on a new Download")
	id := m["id"].(string)
	assert.Regexp(t, `^[a-z2-7]{26}$`, id)
	assert.Equal(t, "My.Release", m["name"], "the upload's file name without .nzb")
	assert.Equal(t, "nzb", m["source_kind"])
	assert.Nil(t, m["category_id"])
	assert.Equal(t, "queued", m["state"])
	assert.Equal(t, "normal", m["priority"])
	assert.Equal(t, float64(0), m["queue_position"])
	assert.Equal(t, false, m["paused"])
	assert.Equal(t, rel.Total, num(m, "total_bytes"), "the sum of the segment sizes of the NZB")
	assert.Zero(t, num(m, "done_bytes"))
	assert.Nil(t, m["fetched_at"])
	assert.Nil(t, m["ready_at"])
	assert.True(t, parseTime(t, m["added_at"]).After(before))

	// What GET says, and the event.
	got0 := d.download(id)
	assert.Equal(t, parseTime(t, m["updated_at"]).Truncate(time.Millisecond), parseTime(t, got0["updated_at"]), "the answer to the add has the time as it was taken, the stored one is in milliseconds")
	delete(m, "updated_at")
	delete(got0, "updated_at")
	assert.Equal(t, m, got0)
	eventually(t, 5*time.Second, "the download_added event", func() bool { return len(ev.all()) >= 1 })
	frames := ev.all()
	assert.Equal(t, "download_added", frames[0].name)
	ev0 := fjson(t, frames[0])
	assert.Equal(t, id, ev0["id"])
	assert.Equal(t, "queued", ev0["state"])
	assert.Equal(t, m["added_at"], ev0["added_at"])

	// The NZB is kept, unchanged, in the Download's work dir with mode 0600.
	p := filepath.Join(u.workDir(id), "My.Release.nzb")
	got, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, rel.NZB, got)
	st, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	assert.Equal(t, []string{"My.Release.nzb"}, u.workFiles(id), "no staging file is left")
}

func TestUsenetAddedDownloadsGetTheNextQueuePosition(t *testing.T) {
	t.Parallel()
	d, _ := startAdder(t)
	rel := smallNZB(t)

	var positions []float64
	for range 3 {
		positions = append(positions, d.addRelease(rel)["queue_position"].(float64))
	}

	assert.Equal(t, []float64{0, 1, 2}, positions)
}

func TestUsenetAddNamesTheDownloadFromTheRequestTheNZBTheFileOrTheFirstFile(t *testing.T) {
	t.Parallel()
	d, _ := startAdder(t)
	plain := smallNZB(t)
	named := smallNZB(t, relMeta("name", "Name From The NZB"))

	for _, tc := range []struct {
		name     string
		nzb      release
		fileName string
		fields   []string
		want     string
	}{
		{"the name field wins", named, "file.nzb", []string{"name", "Chosen"}, "Chosen"},
		{"the NZB's own name", named, "file.nzb", nil, "Name From The NZB"},
		{"the upload's file name", plain, "Some.Show.S01E02.nzb", nil, "Some.Show.S01E02"},
		{"any case of .nzb is cut", plain, "Loud.NZB", nil, "Loud"},
		{"a name that only has the extension falls through", plain, ".nzb", nil, "first.bin"},
		{"the first file when the upload has no name", plain, "", nil, "first.bin"},
		{"a path in the file name is cut", plain, "some/dir/Unix.Name.nzb", nil, "Unix.Name"},
		{"a blank name field falls through", named, "file.nzb", []string{"name", "   "}, "Name From The NZB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := d.upload(tc.fileName, tc.nzb.NZB, tc.fields...)
			require.Equal(t, http.StatusCreated, r.status, r.body)
			assert.Equal(t, tc.want, obj(t, r)["name"])
		})
	}
}

func TestUsenetAddCleansTheDisplayName(t *testing.T) {
	t.Parallel()
	d, _ := startAdder(t)
	rel := smallNZB(t)

	for name, tc := range map[string]struct{ in, want string }{
		"control characters go":    {"a\x00b\x07c\n", "abc"},
		"bidi overrides go":        {"gnp.\u202eexe", "gnp.exe"},
		"an invalid byte goes":     {"a\xffb", "ab"},
		"outer spaces are trimmed": {"  padded  ", "padded"},
		"capped at 255 bytes":      {strings.Repeat("x", 300), strings.Repeat("x", 255)},
		"cut on a rune boundary":   {strings.Repeat("é", 200), strings.Repeat("é", 127)},
	} {
		t.Run(name, func(t *testing.T) {
			r := d.upload("f.nzb", rel.NZB, "name", tc.in)
			require.Equal(t, http.StatusCreated, r.status, r.body)
			assert.Equal(t, tc.want, obj(t, r)["name"])
		})
	}
}

func TestUsenetAddTakesThePasswordFromTheFieldTheNZBOrTheNameSuffix(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	plain := smallNZB(t)
	withPW := smallNZB(t, relMeta("password", "from-nzb"))

	for _, tc := range []struct {
		name     string
		nzb      release
		fileName string
		fields   []string
		wantName string
		wantPW   string
	}{
		{"the field wins", withPW, "x.nzb", []string{"password", "from-field"}, "x", "from-field"},
		{"the NZB's", withPW, "x.nzb", nil, "x", "from-nzb"},
		{"the suffix of the name", plain, "Release{{from-suffix}}.nzb", nil, "Release", "from-suffix"},
		{"the NZB's before the suffix", withPW, "Release{{from-suffix}}.nzb", nil, "Release", "from-nzb"},
		{"the suffix of the name field", plain, "x.nzb", []string{"name", "Named {{s3cr3t}}"}, "Named", "s3cr3t"},
		{"none", plain, "x.nzb", nil, "x", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := d.upload(tc.fileName, tc.nzb.NZB, tc.fields...)
			require.Equal(t, http.StatusCreated, r.status, r.body)
			m := obj(t, r)
			assert.Equal(t, tc.wantName, m["name"])
			if tc.wantPW != "" {
				assert.NotContains(t, r.body, tc.wantPW, "the password is not in the answer")
			}
			assert.Equal(t, tc.wantPW, storedPassword(t, u, core.ID(m["id"].(string))))
		})
	}
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "from-field")
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "s3cr3t")
}

// storedPassword reads the archive password of a Download from the
// database, where it is sealed with the master key.
func storedPassword(t *testing.T, u *usenetSetup, id core.ID) string {
	t.Helper()
	db := openDB(t, u.cfg)
	defer func() { require.NoError(t, db.Close()) }()
	key, err := store.LoadOrCreateKey(u.cfg.KeyFile, discard)
	require.NoError(t, err)
	_, pw, err := store.NewDownloadSourceRepo(db, key).Get(t.Context(), id)
	require.NoError(t, err)
	return pw.Reveal()
}

func TestUsenetAddUsesTheCategoryOfTheRequestOrOfTheNZB(t *testing.T) {
	t.Parallel()
	d, _ := startAdder(t)
	tv := d.cfg.CategoryID // "tv", stored by prepareConfigDir
	mr := d.api(http.MethodPost, "/api/v1/categories", `{"name":"movies"}`)
	require.Equal(t, http.StatusCreated, mr.status, mr.body)
	movies := obj(t, mr)["id"].(string)

	for _, tc := range []struct {
		name   string
		nzb    release
		fields []string
		want   any
	}{
		{"none", smallNZB(t), nil, nil},
		{"the id field", smallNZB(t), []string{"category_id", movies}, movies},
		{"the NZB's category by name", smallNZB(t, relMeta("category", "tv")), nil, string(tv)},
		{"in any case", smallNZB(t, relMeta("category", "MoViEs")), nil, movies},
		{"the id wins over the NZB", smallNZB(t, relMeta("category", "tv")), []string{"category_id", movies}, movies},
		{"a category no one has is ignored", smallNZB(t, relMeta("category", "TV > HD")), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := d.upload("x.nzb", tc.nzb.NZB, tc.fields...)
			require.Equal(t, http.StatusCreated, r.status, r.body)
			assert.Equal(t, tc.want, obj(t, r)["category_id"])
		})
	}
	assert.Len(t, itemsOf(t, d.apiGet("/api/v1/categories")), 2, "an unknown name in the NZB creates no Category")
}

func TestUsenetAddTakesThePriorityAndPausedFields(t *testing.T) {
	t.Parallel()
	d, _ := startAdder(t)
	rel := smallNZB(t)

	for _, p := range []string{"force", "high", "normal", "low"} {
		r := d.upload("x.nzb", rel.NZB, "priority", p)
		require.Equal(t, http.StatusCreated, r.status, r.body)
		assert.Equal(t, p, obj(t, r)["priority"])
	}
	for _, tc := range []struct {
		in   string
		want bool
	}{{"true", true}, {"1", true}, {"t", true}, {"false", false}, {"0", false}, {"", false}} {
		r := d.upload("x.nzb", rel.NZB, "paused", tc.in)
		require.Equal(t, http.StatusCreated, r.status, "%q: %s", tc.in, r.body)
		m := obj(t, r)
		assert.Equal(t, tc.want, m["paused"], tc.in)
		assert.Equal(t, "queued", m["state"])
	}
}

func TestUsenetAddRefusesBadFieldsAndLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	rel := smallNZB(t)

	for _, tc := range []struct {
		name    string
		fields  []string
		status  int
		code    string
		message string
	}{
		{"unknown priority", []string{"priority", "urgent"}, 400, "invalid_priority", `unknown priority "urgent": use force, high, normal or low`},
		{"priority is case sensitive", []string{"priority", "HIGH"}, 400, "invalid_priority", `unknown priority "HIGH": use force, high, normal or low`},
		{"unknown category", []string{"category_id", "nosuchcategory"}, 400, "unknown_category", `category "nosuchcategory" does not exist`},
		{"password too long", []string{"password", strings.Repeat("p", 1025)}, 400, "invalid_password", "the password is longer than 1024 bytes"},
		{"paused that is no bool", []string{"paused", "maybe"}, 400, "invalid_paused", "paused must be true or false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := d.upload("x.nzb", rel.NZB, tc.fields...)
			requireErrorMessage(t, r, tc.status, tc.code, tc.message)
			requireNothingAdded(t, d, u)
		})
	}
	t.Run("a password of exactly 1024 bytes is fine", func(t *testing.T) {
		r := d.upload("x.nzb", rel.NZB, "password", strings.Repeat("p", 1024))
		assert.Equal(t, http.StatusCreated, r.status, r.body)
	})
}

func TestUsenetAddRefusesTwoNZBParts(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	rel := smallNZB(t)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for range 2 {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="nzb"; filename="a.nzb"`)
		p, _ := w.CreatePart(h)
		_, _ = p.Write(rel.NZB)
	}
	_ = w.Close()
	r := d.request(http.MethodPost, downloadsPath, map[string]string{"Authorization": "Bearer " + d.cfg.AppKey, "Content-Type": w.FormDataContentType()}, buf.String())

	// Both parts are named "nzb", so the repeated field is refused first.
	requireError(t, r, 400, "duplicate_field")
	requireNothingAdded(t, d, u)
}

func TestUsenetAddRefusesWhatIsNotAValidNZB(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)

	for _, tc := range []struct {
		name    string
		content string
		message string // exact when set, else a prefix
		prefix  string
	}{
		{"no file element", `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"></nzb>`, "not a valid NZB: nzb: no usable files", ""},
		{"a file without segments", `<nzb><file subject="x"><segments></segments></file></nzb>`, "not a valid NZB: nzb: no usable files", ""},
		{"another root element", `<html><body>hello</body></html>`, "not a valid NZB: nzb: malformed document: root element is <html>, want <nzb>", ""},
		{"not XML at all", `this is not an nzb`, "", "not a valid NZB: nzb: malformed document"},
		{"broken XML", `<nzb><file subject="x"><segments><segment bytes="1" number="1">a@b</segment></segments></nzb>`, "", "not a valid NZB: nzb: malformed document"},
		{"an empty file", ``, "", "not a valid NZB: nzb: "},
		{"a torrent", `d8:announce3:foo4:infod6:lengthi1eee`, "", "not a valid NZB: nzb: malformed document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := d.upload("x.nzb", []byte(tc.content))
			msg := requireError(t, r, 400, "invalid_nzb")
			if tc.message != "" {
				assert.Equal(t, tc.message, msg)
			} else {
				assert.True(t, strings.HasPrefix(msg, tc.prefix), msg)
			}
			requireNothingAdded(t, d, u)
		})
	}
}

// spaces reads white space for ever.
type spaces struct{}

func (spaces) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// bigUpload posts an NZB-looking body of n bytes (a valid start, then white
// space) as a multipart upload without ever holding it in memory.
func bigUpload(t *testing.T, d *daemon, n int64) response {
	t.Helper()
	const start = `<?xml version="1.0"?><nzb>`
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	go func() {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="nzb"; filename="big.nzb"`)
		p, err := w.CreatePart(h)
		if err == nil {
			_, err = io.WriteString(p, start)
		}
		if err == nil {
			_, err = io.CopyN(p, spaces{}, n-int64(len(start)))
		}
		if err == nil {
			err = w.Close()
		}
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequest(http.MethodPost, "http://"+d.addr+downloadsPath, pr)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+d.cfg.AppKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

func TestUsenetAddRefusesAnNZBOverTheSizeLimit(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)

	t.Run("64 MiB and a byte is too large for an NZB", func(t *testing.T) {
		r := bigUpload(t, d, 64<<20+1)
		requireErrorMessage(t, r, http.StatusRequestEntityTooLarge, "nzb_too_large", "the NZB is larger than 64 MiB")
		requireNothingAdded(t, d, u)
	})
	t.Run("a body past the route's cap is cut off", func(t *testing.T) {
		r := bigUpload(t, d, 66<<20)
		assert.Equal(t, http.StatusRequestEntityTooLarge, r.status)
		assert.Contains(t, []any{"nzb_too_large", "request_too_large"}, obj(t, r)["error"].(map[string]any)["code"])
		requireNothingAdded(t, d, u)
	})
	t.Run("the daemon is fine afterwards", func(t *testing.T) {
		r := d.upload("ok.nzb", smallNZB(t).NZB)
		assert.Equal(t, http.StatusCreated, r.status, r.body)
	})
}

// ---- JSON {url} ----

// nzbHost serves NZBs for the URL tests and remembers the requests.
type nzbHost struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func (h *nzbHost) requests() []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*http.Request(nil), h.reqs...)
}

func startNZBHost(t *testing.T, handler http.Handler) *nzbHost {
	t.Helper()
	h := &nzbHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.reqs = append(h.reqs, r.Clone(r.Context()))
		h.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	return h
}

func (d *daemon) addURL(body string) response {
	d.t.Helper()
	return d.api(http.MethodPost, downloadsPath, body)
}

func TestUsenetAddByURLFetchesTheNZBAndNamesItFromItsContent(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	rel := smallNZB(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/get/some.release.nzb", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-nzb")
		_, _ = w.Write(rel.NZB)
	})
	host := startNZBHost(t, mux)

	r := d.addURL(fmt.Sprintf(`{"url":%q,"priority":"high","paused":true}`, host.URL+"/get/some.release.nzb?apikey=SECRETKEY"))

	require.Equal(t, http.StatusCreated, r.status, r.body)
	m := obj(t, r)
	assert.Equal(t, "first.bin", m["name"], "the URL's file name is not used: the first file of the NZB is")
	assert.Equal(t, "nzb", m["source_kind"])
	assert.Equal(t, "queued", m["state"])
	assert.Equal(t, "high", m["priority"])
	assert.Equal(t, true, m["paused"])
	assert.Equal(t, rel.Total, num(m, "total_bytes"))
	got, err := os.ReadFile(filepath.Join(u.workDir(m["id"].(string)), "first.bin.nzb"))
	require.NoError(t, err)
	assert.Equal(t, rel.NZB, got)

	reqs := host.requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, http.MethodGet, reqs[0].Method)
	assert.Equal(t, "SECRETKEY", reqs[0].URL.Query().Get("apikey"), "the URL is fetched as given")
	assert.Equal(t, "bitnews (+https://github.com/jroosing/bitnews)", reqs[0].Header.Get("User-Agent"))
	assert.Equal(t, "application/x-bittorrent, application/x-nzb, text/xml;q=0.9, */*;q=0.1", reqs[0].Header.Get("Accept"))
	assert.Empty(t, reqs[0].Header.Get("Cookie"))
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "SECRETKEY", "the API key in the URL is never logged")
	assert.NotContains(t, r.body, "SECRETKEY")
}

func TestUsenetAddByURLUsesTheNameCategoryAndPasswordOfTheRequestAndTheNZB(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	rel := smallNZB(t, relMeta("name", "Meta Name"), relMeta("category", "tv"), relMeta("password", "pw-from-nzb"))
	host := startNZBHost(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(rel.NZB) }))

	r := d.addURL(fmt.Sprintf(`{"url":%q}`, host.URL+"/a"))
	require.Equal(t, http.StatusCreated, r.status, r.body)
	m := obj(t, r)
	assert.Equal(t, "Meta Name", m["name"])
	assert.Equal(t, string(d.cfg.CategoryID), m["category_id"])
	assert.Equal(t, "pw-from-nzb", storedPassword(t, u, core.ID(m["id"].(string))))

	r = d.addURL(fmt.Sprintf(`{"url":%q,"name":"Mine","password":"mine","category_id":%q}`, host.URL+"/b", d.cfg.CategoryID))
	require.Equal(t, http.StatusCreated, r.status, r.body)
	m = obj(t, r)
	assert.Equal(t, "Mine", m["name"])
	assert.Equal(t, "mine", storedPassword(t, u, core.ID(m["id"].(string))))
}

func TestUsenetAddByURLSendsCredentialsOnlyWhenTheyAreInTheURL(t *testing.T) {
	t.Parallel()
	d, _ := startAdder(t)
	rel := smallNZB(t)
	host := startNZBHost(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(rel.NZB) }))

	r := d.addURL(fmt.Sprintf(`{"url":%q}`, strings.Replace(host.URL, "http://", "http://user:p%40ss@", 1)+"/x"))

	require.Equal(t, http.StatusCreated, r.status, r.body)
	reqs := host.requests()
	require.Len(t, reqs, 1)
	user, pass, ok := reqs[0].BasicAuth()
	assert.True(t, ok)
	assert.Equal(t, "user", user)
	assert.Equal(t, "p@ss", pass)
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "p%40ss")
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "p@ss")
}

func TestUsenetAddByURLFollowsFiveRedirectsAndNoMore(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	rel := smallNZB(t)
	mux := http.NewServeMux()
	for i := range 5 {
		mux.HandleFunc(fmt.Sprintf("/hop/%d", i), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fmt.Sprintf("/hop/%d", i+1), http.StatusFound)
		})
	}
	mux.HandleFunc("/hop/5", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(rel.NZB) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusMovedPermanently)
	})
	host := startNZBHost(t, mux)

	r := d.addURL(fmt.Sprintf(`{"url":%q}`, host.URL+"/hop/0"))
	assert.Equal(t, http.StatusCreated, r.status, "five redirects are followed: %s", r.body)

	r = d.addURL(fmt.Sprintf(`{"url":%q}`, host.URL+"/loop"))
	requireErrorMessage(t, r, http.StatusBadGateway, "fetch_failed", "could not fetch the NZB or torrent: stopped after 5 redirects")
	assert.Len(t, itemsOf(t, d.apiGet(downloadsPath)), 1)
	ents, err := os.ReadDir(u.roots.usenetIncomplete)
	require.NoError(t, err)
	assert.Len(t, ents, 1, "only the first Download's directory")
}

func TestUsenetAddByURLRefusesARedirectToAnotherScheme(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	host := startNZBHost(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://files.example/x.nzb", http.StatusFound)
	}))

	r := d.addURL(fmt.Sprintf(`{"url":%q}`, host.URL+"/x"))

	requireErrorMessage(t, r, http.StatusBadGateway, "fetch_failed", `could not fetch the NZB or torrent: refused a redirect to a "ftp" URL: only http and https are allowed`)
	requireNothingAdded(t, d, u)
}

func TestUsenetAddByURLRefusesWhatTheServerAnswersBadly(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	mux.HandleFunc("/forbidden", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad api key SECRETKEY", http.StatusForbidden)
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html><body>login</body></html>")
	})
	mux.HandleFunc("/empty", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/huge-header", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(70<<20))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<nzb>")
	})
	host := startNZBHost(t, mux)

	for _, tc := range []struct {
		path    string
		status  int
		code    string
		message string
		prefix  string
	}{
		{"/missing", 502, "fetch_failed", "could not fetch the NZB or torrent: the server answered 404 Not Found", ""},
		{"/broken", 502, "fetch_failed", "could not fetch the NZB or torrent: the server answered 500 Internal Server Error", ""},
		{"/forbidden?apikey=SECRETKEY", 502, "fetch_failed", "could not fetch the NZB or torrent: the server answered 403 Forbidden", ""},
		{"/html", 400, "invalid_nzb", "not a valid NZB: nzb: malformed document: root element is <html>, want <nzb>", ""},
		{"/empty", 400, "invalid_nzb", "", "not a valid NZB: nzb: "},
		{"/huge-header", 413, "nzb_too_large", "the NZB is larger than 64 MiB", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := d.addURL(fmt.Sprintf(`{"url":%q}`, host.URL+tc.path))
			msg := requireError(t, r, tc.status, tc.code)
			if tc.message != "" {
				assert.Equal(t, tc.message, msg)
			} else {
				assert.True(t, strings.HasPrefix(msg, tc.prefix), msg)
			}
			assert.NotContains(t, r.body, "SECRETKEY")
			requireNothingAdded(t, d, u)
		})
	}
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "SECRETKEY")
}

func TestUsenetAddByURLRefusesABodyOverTheLimitThatHasNoLength(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)
	host := startNZBHost(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<?xml version="1.0"?><nzb>`)
		_, _ = io.CopyN(w, spaces{}, 65<<20)
	}))

	r := d.addURL(fmt.Sprintf(`{"url":%q}`, host.URL+"/endless"))

	requireErrorMessage(t, r, http.StatusRequestEntityTooLarge, "nzb_too_large", "the NZB is larger than 64 MiB")
	requireNothingAdded(t, d, u)
}

func TestUsenetAddByURLReportsAServerThatCannotBeReached(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)

	r := d.addURL(fmt.Sprintf(`{"url":"http://127.0.0.1:%d/x.nzb?apikey=SECRETKEY"}`, freePort(t)))

	msg := requireError(t, r, http.StatusBadGateway, "fetch_failed")
	assert.True(t, strings.HasPrefix(msg, "could not fetch the NZB or torrent: "), msg)
	assert.Contains(t, msg, "connection refused")
	assert.NotContains(t, msg, "SECRETKEY")
	assert.NotContains(t, d.stdout.String()+d.stderr.String(), "SECRETKEY")
	requireNothingAdded(t, d, u)
}

func TestUsenetAddByURLRefusesURLsThatAreNotHTTP(t *testing.T) {
	t.Parallel()
	d, u := startAdder(t)

	for _, link := range []string{"ftp://host.example/a.nzb", "file:///etc/passwd", "not a url", "/relative/a.nzb", "http://", "//host.example/a.nzb", "javascript:alert(1)", "http:///nohost"} {
		r := d.addURL(fmt.Sprintf(`{"url":%q}`, link))
		requireErrorMessage(t, r, http.StatusBadRequest, "invalid_url", "url must be an absolute http or https URL")
	}
	requireNothingAdded(t, d, u)
}

func TestUsenetAddByURLChecksTheCheapFieldsBeforeItFetchesAnything(t *testing.T) {
	t.Parallel()
	d, _ := startAdder(t)
	rel := smallNZB(t)
	host := startNZBHost(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(rel.NZB) }))

	requireErrorMessage(t, d.addURL(fmt.Sprintf(`{"url":%q,"priority":"urgent"}`, host.URL)), 400, "invalid_priority", `unknown priority "urgent": use force, high, normal or low`)
	requireErrorMessage(t, d.addURL(fmt.Sprintf(`{"url":%q,"category_id":"nope"}`, host.URL)), 400, "unknown_category", `category "nope" does not exist`)
	requireErrorMessage(t, d.addURL(fmt.Sprintf(`{"url":%q,"password":%q}`, host.URL, strings.Repeat("p", 1025))), 400, "invalid_password", "the password is longer than 1024 bytes")

	assert.Empty(t, host.requests(), "nothing was fetched")
}
