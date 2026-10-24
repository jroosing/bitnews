package redact_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jroosing/bitnews/internal/redact"
)

// marker prefixes every injected secret. Inputs that already contain it
// are skipped, so any marker in the output is a leak.
const marker = "s3cr3t"

var secretNames = []string{
	"apikey", "api_key", "key", "token", "password", "pass", "nzbkey",
	"r", "passkey", "authkey", "torrent_pass", "rsskey", "apitoken", "access_token",
}

// FuzzURL checks that URL never panics and that a secret placed in a
// secret-bearing query or fragment parameter, in the userinfo password or in
// an announce/passkey path segment never survives.
func FuzzURL(f *testing.F) {
	for _, seed := range []string{
		"",
		"https://api.nzbgeek.info/api?t=search&q=ubuntu",
		"http://joey:pw@sabnzbd:8080/sabnzbd/api?mode=queue&output=json",
		"http://prowlarr:9696/3/download?link=aHR0cHM6Ly9leGFtcGxl&file=x",
		"mailto:joey@example.com",
		"//host/path?a=1;b=2#frag",
		"http://[::1]:9494/?x",
		"%zz",
	} {
		f.Add(seed, "0123456789abcdef", uint8(0), uint8(0))
	}
	f.Fuzz(func(t *testing.T, raw, suffix string, nameIdx, caseMask uint8) {
		_ = redact.URL(raw) // must not panic

		if containsMarker(raw) {
			t.Skip("input already contains the marker")
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		secret := strings.ToUpper(marker[:3]) + marker[3:] + suffix

		withParam := *u
		if withParam.RawQuery != "" {
			withParam.RawQuery += "&"
		}
		withParam.RawQuery += mixCase(secretNames[int(nameIdx)%len(secretNames)], caseMask) + "=" + url.QueryEscape(secret)
		assertNoLeak(t, withParam.String())

		withFragment := *u
		withFragment.Fragment, withFragment.RawFragment = "", ""
		withFragment.Opaque = ""
		if withFragment.Host == "" {
			withFragment.Host = "h"
		}
		withFragment.Fragment = mixCase(secretNames[int(nameIdx)%len(secretNames)], caseMask) + "=" + secret
		assertNoLeak(t, withFragment.String())

		// Passkey in the path: a hex segment anywhere, a token next to announce.
		hexKey := strings.Repeat("a1B2c3D4", 4+int(caseMask)%3)
		tokenKey := marker + "_" + strings.Map(tokenOnly, suffix) + strings.Repeat("Z", 20)
		for _, p := range []string{
			"/announce/" + tokenKey, "/" + tokenKey + "/announce",
			"/passkey/" + tokenKey, "/x/" + hexKey + "/y",
		} {
			withPath := url.URL{Scheme: "http", Host: "t.example", Path: p}
			assertNoLeakIn(t, withPath.String(), tokenKey, hexKey)
		}

		withPassword := *u
		if withPassword.Host == "" {
			withPassword.Host = "h" // userinfo needs an authority
			withPassword.Opaque = ""
		}
		withPassword.User = url.UserPassword(withPassword.User.Username(), secret)
		assertNoLeak(t, withPassword.String())
	})
}

// assertNoLeakIn fails if any of secrets survives in the redacted in.
func assertNoLeakIn(t *testing.T, in string, secrets ...string) {
	t.Helper()
	out := redact.URL(in)
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked:\n in: %q\nout: %q", secret, in, out)
		}
	}
}

// tokenOnly keeps only characters that are valid in a passkey token.
func tokenOnly(r rune) rune {
	if r < 0x80 && isTokenRune(byte(r)) {
		return r
	}
	return -1
}

func isTokenRune(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-'
}

func assertNoLeak(t *testing.T, in string) {
	t.Helper()
	if out := redact.URL(in); containsMarker(out) {
		t.Fatalf("secret leaked:\n in: %q\nout: %q", in, out)
	}
}

// containsMarker reports whether s holds the marker in any case, also after
// percent-decoding.
func containsMarker(s string) bool {
	for _, form := range []string{s, unescapeLenient(s)} {
		if strings.Contains(strings.ToLower(form), marker) {
			return true
		}
	}
	return false
}

func unescapeLenient(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// mixCase upper-cases the letters of s selected by the bits of mask.
func mixCase(s string, mask uint8) string {
	b := []byte(s)
	for i := range b {
		if mask&(1<<(i%8)) != 0 && b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
