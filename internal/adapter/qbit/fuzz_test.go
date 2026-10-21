package qbit_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/adapter/qbit"
)

// FuzzParseHashes: hostile hashes parameters never panic, and everything
// that comes out is a 40 character lowercase hex hash, at most maxHashes of
// them.
func FuzzParseHashes(f *testing.F) {
	for _, s := range []string{"", "all", "ALL", strings.Repeat("a", 40), strings.Repeat("A", 64), "a|b|c", "|||", "../..", "\x00", strings.Repeat("0", 41),
		strings.Repeat("ab|", 2000)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		l, ok := qbit.ParseHashes(s)
		if !ok {
			return
		}
		if len(l.Hashes) > 1000 {
			t.Fatalf("%d hashes", len(l.Hashes))
		}
		for _, h := range l.Hashes {
			if len(h) != 40 || strings.Trim(h, "0123456789abcdef") != "" {
				t.Fatalf("bad hash %q from %q", h, s)
			}
		}
	})
}

// FuzzRequest sends arbitrary paths, methods, queries and form bodies through
// a logged-in handler: it must never panic and never answer 5xx.
func FuzzRequest(f *testing.F) {
	f.Add("GET", "torrents/info", "hashes=all&filter=completed&sort=name&limit=-5&offset=junk", "")
	f.Add("POST", "torrents/add", "", "urls=magnet%3A%3Fxt%3Dabc%0Afile%3A%2F%2F%2Fetc%2Fpasswd&savepath=..%2F..&category=%00")
	f.Add("POST", "torrents/setShareLimits", "", "hashes=all&ratioLimit=NaN&seedingTimeLimit=99999999999999999999")
	f.Add("POST", "torrents/delete", "", "hashes=all|x&deleteFiles=1")
	f.Add("POST", "torrents/setCategory", "", "hashes=all&category=%E2%82%AC")
	f.Add("GET", "../../etc/passwd", "", "")
	s := newStack(f)
	s.seed("one")
	s.seed("two", ready("/d/two"))
	sid := s.login()
	f.Fuzz(func(t *testing.T, method, path, query, body string) {
		if !validMethod(method) || strings.ContainsAny(path+query, " \r\n\x00") {
			return
		}
		target := "/api/v2/" + url.PathEscape(path)
		if query != "" {
			target += "?" + query
		}
		r, err := http.NewRequest(method, target, strings.NewReader(body))
		if err != nil {
			return
		}
		r.RemoteAddr = "192.0.2.1:1"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(sid)
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, r)
		if rec.Code >= 500 {
			t.Fatalf("%s %s %q -> %d %s", method, target, body, rec.Code, rec.Body.String())
		}
	})
}

func validMethod(m string) bool {
	switch m {
	case "GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "PATCH":
		return true
	}
	return false
}
