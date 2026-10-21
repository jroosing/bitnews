package adaptertest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Run replays the fixture's steps against h and fails t with one line per
// difference. vars are the variables {{name}} refers to; the caller seeds
// the state the fixture expects and returns them (see the package comment).
func Run(t testing.TB, h http.Handler, fx *Fixture, vars map[string]string) {
	t.Helper()
	diffs, err := Check(h, fx, vars)
	if err != nil {
		t.Fatalf("fixture %s: %v", fx.Path, err)
	}
	if len(diffs) > 0 {
		t.Errorf("fixture %q (%s) does not match:\n  %s", fx.Name, filepath.Base(fx.Path), strings.Join(diffs, "\n  "))
	}
}

// Check is Run without a testing.T: it returns the differences, or an error
// for a fixture that cannot be run (an unknown variable, a missing file, a
// bad capture path).
func Check(h http.Handler, fx *Fixture, vars map[string]string) (diffs []string, err error) {
	vars = cloneVars(vars)
	for i, step := range fx.Steps {
		label := fmt.Sprintf("step %d", i+1)
		if step.Name != "" {
			label += " (" + step.Name + ")"
		}
		req, err := buildRequest(fx, step.Request, vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		stepDiffs, got, err := checkResponse(step.Response, rec, vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		for _, d := range stepDiffs {
			diffs = append(diffs, label+" "+d)
		}
		for _, name := range sortedStrings(step.Capture) {
			v, err := capture(got, rec, step.Capture[name])
			if err != nil {
				return nil, fmt.Errorf("%s: capture %s: %w", label, name, err)
			}
			vars[name] = v
		}
	}
	return diffs, nil
}

func sortedStrings(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func cloneVars(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

var varRef = regexp.MustCompile(`\{\{([A-Za-z0-9_.-]+)\}\}`)

// subst replaces {{name}} with the variable.
func subst(s string, vars map[string]string) (string, error) {
	var missing []string
	out := varRef.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-2]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unknown variable %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func buildRequest(fx *Fixture, r Request, vars map[string]string) (*http.Request, error) {
	target, err := subst(r.Target, vars)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	contentType := ""
	switch {
	case len(r.Multipart) > 0:
		b, ct, err := buildMultipart(fx, r.Multipart, vars)
		if err != nil {
			return nil, err
		}
		body, contentType = b, ct
	case r.Body != "":
		s, err := subst(r.Body, vars)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(s)
	}
	req := httptest.NewRequest(r.Method, target, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range r.Headers {
		if v, err = subst(v, vars); err != nil {
			return nil, err
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

func buildMultipart(fx *Fixture, parts []Part, vars map[string]string) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		content := p.Text
		if p.File != "" {
			raw, err := os.ReadFile(filepath.Join(fx.Dir, filepath.Clean("/"+p.File)))
			if err != nil {
				return nil, "", err
			}
			content = string(raw)
		}
		content, err := subst(content, vars)
		if err != nil {
			return nil, "", err
		}
		h := textproto.MIMEHeader{}
		disp := fmt.Sprintf(`form-data; name=%q`, p.Name)
		if p.Filename != "" {
			disp += fmt.Sprintf(`; filename=%q`, p.Filename)
		}
		h.Set("Content-Disposition", disp)
		if p.ContentType != "" {
			h.Set("Content-Type", p.ContentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := io.WriteString(w, content); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

// checkResponse compares the reply with the expectation and returns the
// decoded JSON body, if it has one, for captures.
func checkResponse(want Response, rec *httptest.ResponseRecorder, vars map[string]string) (diffs []string, got any, err error) {
	if rec.Code != want.Status {
		diffs = append(diffs, fmt.Sprintf("status: want %d, got %d", want.Status, rec.Code))
	}
	for _, name := range sortedStrings(want.Headers) {
		exp, err := subst(want.Headers[name], vars)
		if err != nil {
			return nil, nil, err
		}
		for _, d := range Compare(exp, rec.Header().Get(name)) {
			diffs = append(diffs, "header "+name+d[1:])
		}
	}
	body := rec.Body.Bytes()
	if want.Body != nil {
		exp, err := subst(*want.Body, vars)
		if err != nil {
			return nil, nil, err
		}
		for _, d := range Compare(exp, string(body)) {
			diffs = append(diffs, "body"+d[1:])
		}
	}
	if len(body) > 0 && (len(want.JSON) > 0 || json.Valid(body)) {
		dec := json.NewDecoder(bytes.NewReader(body))
		if derr := dec.Decode(&got); derr != nil && len(want.JSON) > 0 {
			diffs = append(diffs, fmt.Sprintf("body: not JSON (%v): %.80q", derr, body))
			return diffs, nil, nil
		}
	} else if len(want.JSON) > 0 {
		diffs = append(diffs, fmt.Sprintf("body: want JSON, got %.80q", body))
		return diffs, nil, nil
	}
	if len(want.JSON) > 0 {
		var exp any
		if err := json.Unmarshal(want.JSON, &exp); err != nil {
			return nil, nil, fmt.Errorf("expected json: %w", err)
		}
		if exp, err = substJSON(exp, vars); err != nil {
			return nil, nil, err
		}
		diffs = append(diffs, Compare(exp, got)...)
	}
	return diffs, got, nil
}

// substJSON applies subst to every string of a decoded JSON value.
func substJSON(v any, vars map[string]string) (any, error) {
	switch x := v.(type) {
	case string:
		return subst(x, vars)
	case []any:
		for i := range x {
			var err error
			if x[i], err = substJSON(x[i], vars); err != nil {
				return nil, err
			}
		}
	case map[string]any:
		// Keys are substituted too, so an object keyed by an id ({{hash.a}})
		// can be matched.
		out := make(map[string]any, len(x))
		for k, val := range x {
			nk, err := subst(k, vars)
			if err != nil {
				return nil, err
			}
			if out[nk], err = substJSON(val, vars); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}

// capture reads a captured value from the response: a JSON path like
// "$.nzo_ids[0]", or "header:Name:REGEX" for a response header (the first
// capture group of the first match over all values of the header, or the
// whole match without a group), such as "header:Set-Cookie:SID=([^;]+)".
func capture(got any, rec *httptest.ResponseRecorder, spec string) (string, error) {
	rest, ok := strings.CutPrefix(spec, "header:")
	if !ok {
		return lookup(got, spec)
	}
	name, pattern, ok := strings.Cut(rest, ":")
	if !ok {
		return "", fmt.Errorf("capture %q: want header:Name:REGEX", spec)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("capture %q: %w", spec, err)
	}
	for _, v := range rec.Header().Values(name) {
		if m := re.FindStringSubmatch(v); m != nil {
			return m[len(m)-1], nil
		}
	}
	return "", fmt.Errorf("capture %q: no header %s matches", spec, name)
}
