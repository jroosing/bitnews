package adaptertest_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/adaptertest"
)

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(s), &v))
	return v
}

func TestCompareMatchersAndReadableDiffs(t *testing.T) {
	tests := []struct {
		name, want, got string
		diffs           []string
	}{
		{"exact match", `{"status":"Completed","n":1}`, `{"status":"Completed","n":1,"extra":true}`, nil},
		{"wrong string shows want and got", `{"queue":{"slots":[{"status":"Downloading"}]}}`, `{"queue":{"slots":[{"status":"Queued"}]}}`,
			[]string{`$.queue.slots[0].status: want "Downloading", got "Queued"`}},
		{"missing key", `{"a":"x"}`, `{}`, []string{`$.a: missing (want "x")`}},
		{"any", `{"a":"$any"}`, `{"a":[1,2]}`, nil},
		{"type ok", `{"a":"$type:string","b":"$type:number","c":"$type:bool","d":"$type:array","e":"$type:object","f":"$type:null"}`,
			`{"a":"x","b":1.5,"c":false,"d":[],"e":{},"f":null}`, nil},
		{"type mismatch", `{"a":"$type:number"}`, `{"a":"1"}`, []string{`$.a: want type number, got string ("1")`}},
		{"regexp", `{"a":"$re:^SABnzbd_nzo_[a-z0-9]+$"}`, `{"a":"SABnzbd_nzo_abc1"}`, nil},
		{"regexp mismatch", `{"a":"$re:^x$"}`, `{"a":"y"}`, []string{`$.a: want a string matching /^x$/, got "y"`}},
		{"regexp on a non string", `{"a":"$re:1"}`, `{"a":1}`, []string{`$.a: want a string matching /1/, got 1`}},
		{"absent ok", `{"a":"$absent"}`, `{}`, nil},
		{"absent violated", `{"password":"$absent"}`, `{"password":""}`, []string{`$.password: want it absent, got ""`}},
		{"escaped dollar", `{"a":"$$any"}`, `{"a":"$any"}`, nil},
		{"number mismatch", `{"a":1}`, `{"a":2}`, []string{`$.a: want 1, got 2`}},
		{"bool vs string", `{"status":true}`, `{"status":"true"}`, []string{`$.status: want true, got "true"`}},
		{"array length", `{"a":[1,2]}`, `{"a":[1]}`, []string{`$.a: want 2 array elements, got 1`}},
		{"object vs array", `{"a":{}}`, `{"a":[]}`, []string{`$.a: want an object, got []`}},
		{"exact rejects extras", `{"$exact":true,"a":1}`, `{"a":1,"b":2}`, []string{`$.b: unexpected key, got 2`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			diffs := adaptertest.Compare(decodeJSON(t, tc.want), decodeJSON(t, tc.got))

			assert.Equal(t, tc.diffs, diffs)
		})
	}
}

func writeFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadReadsFixturesInNameOrderAndInsistsOnProvenance(t *testing.T) {
	dir := t.TempDir()
	step := `"steps":[{"request":{"method":"GET","target":"/x"},"response":{"status":200}}]`
	writeFixture(t, dir, "b.json", `{"name":"b","provenance":"hand-written, NOT recorded",`+step+`}`)
	writeFixture(t, dir, "a.json", `{"name":"a","provenance":"hand-written, NOT recorded",`+step+`}`)

	fxs, err := adaptertest.Load(dir)

	require.NoError(t, err)
	require.Len(t, fxs, 2)
	assert.Equal(t, "a", fxs[0].Name)
	assert.Equal(t, "b", fxs[1].Name)

	writeFixture(t, dir, "c.json", `{"name":"c",`+step+`}`)
	_, err = adaptertest.Load(dir)
	assert.ErrorContains(t, err, "no provenance")

	writeFixture(t, dir, "c.json", `{"name":"c","provenance":"p","bogus":1,`+step+`}`)
	_, err = adaptertest.Load(dir)
	assert.ErrorContains(t, err, "unknown field")
}

func echo() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Seen", r.Method+" "+r.URL.RequestURI())
		out, _ := json.Marshal(map[string]any{
			"ids": []string{"id1"}, "body": string(body), "ct": r.Header.Get("Content-Type"), "ua": r.Header.Get("User-Agent"),
		})
		_, _ = w.Write(out)
	})
}

func loadOne(t *testing.T, body string) *adaptertest.Fixture {
	t.Helper()
	dir := t.TempDir()
	return mustLoad(t, writeFixture(t, dir, "f.json", body))
}

func mustLoad(t *testing.T, path string) *adaptertest.Fixture {
	t.Helper()
	fx, err := adaptertest.LoadFile(path)
	require.NoError(t, err)
	return fx
}

func TestCheckSubstitutesVariablesCapturesAndComparesHeadersAndBodies(t *testing.T) {
	fx := loadOne(t, `{"name":"n","provenance":"p","steps":[
	  {"request":{"method":"POST","target":"/api?k={{key}}","headers":{"User-Agent":"Sonarr/{{ver}}"},"body":"hello {{key}}"},
	   "response":{"status":200,"headers":{"X-Seen":"POST /api?k=secret","Content-Type":"$re:^application/json"},
	               "json":{"ids":["id1"],"ua":"Sonarr/4","body":"hello secret"}},
	   "capture":{"first":"$.ids[0]"}},
	  {"request":{"method":"GET","target":"/again/{{first}}"},
	   "response":{"status":200,"headers":{"X-Seen":"GET /again/id1"}}}]}`)

	diffs, err := adaptertest.Check(echo(), fx, map[string]string{"key": "secret", "ver": "4"})

	require.NoError(t, err)
	assert.Empty(t, diffs)
}

func TestCheckReportsAWrongStatusAndABrokenField(t *testing.T) {
	fx := loadOne(t, `{"name":"n","provenance":"p","steps":[
	  {"request":{"method":"GET","target":"/x"},"response":{"status":201,"json":{"ua":"Sonarr"}}}]}`)

	diffs, err := adaptertest.Check(echo(), fx, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{`step 1 status: want 201, got 200`, `step 1 $.ua: want "Sonarr", got ""`}, diffs)
}

func TestCheckSendsMultipartPartsFromTextAndFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "files"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files", "x.nzb"), []byte("<nzb/>"), 0o600))
	fx := mustLoad(t, writeFixture(t, dir, "f.json", `{"name":"n","provenance":"p","steps":[
	  {"request":{"method":"POST","target":"/up","multipart":[
	     {"name":"name","filename":"My Show.nzb","content_type":"application/x-nzb","file":"x.nzb"},
	     {"name":"cat","text":"{{c}}"}]},
	   "response":{"status":200,"json":{"ct":"$re:^multipart/form-data; boundary=","body":"$re:(?s)filename=\"My Show.nzb\".*<nzb/>.*name=\"cat\".*\\r\\n\\r\\ntv\\r\\n"}}}]}`))

	diffs, err := adaptertest.Check(echo(), fx, map[string]string{"c": "tv"})

	require.NoError(t, err)
	assert.Empty(t, diffs)
}

func TestCheckFailsOnAnUnknownVariableOrAMissingFileOrBadCapture(t *testing.T) {
	for name, body := range map[string]string{
		"unknown variable": `{"request":{"method":"GET","target":"/{{nope}}"},"response":{"status":200}}`,
		"missing file":     `{"request":{"method":"POST","target":"/","multipart":[{"name":"n","filename":"f","file":"nope.nzb"}]},"response":{"status":200}}`,
		"bad capture":      `{"request":{"method":"GET","target":"/"},"response":{"status":200},"capture":{"x":"$.nope"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			fx := loadOne(t, `{"name":"n","provenance":"p","steps":[`+body+`]}`)

			_, err := adaptertest.Check(echo(), fx, nil)

			assert.Error(t, err)
		})
	}
}

func TestCheckPlainTextBodies(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Ok.")) })
	fx := loadOne(t, `{"name":"n","provenance":"p","steps":[{"request":{"method":"GET","target":"/"},"response":{"status":200,"body":"Ok."}}]}`)
	bad := loadOne(t, `{"name":"n","provenance":"p","steps":[{"request":{"method":"GET","target":"/"},"response":{"status":200,"body":"Fails."}}]}`)

	diffs, err := adaptertest.Check(h, fx, nil)
	require.NoError(t, err)
	assert.Empty(t, diffs)
	diffs, err = adaptertest.Check(h, bad, nil)
	require.NoError(t, err)
	assert.True(t, strings.Contains(strings.Join(diffs, "\n"), `body: want "Fails.", got "Ok."`), diffs)
}
