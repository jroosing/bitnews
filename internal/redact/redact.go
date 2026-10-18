// Package redact masks secrets in values that end up in logs.
package redact

import (
	"net/url"
	"strings"
)

const (
	// Mask replaces every redacted value.
	Mask = "REDACTED"
	// Invalid is returned in place of input that cannot be parsed, since
	// secrets in it cannot be located reliably.
	Invalid = "[invalid URL redacted]"
)

// URL returns raw with the userinfo password and the values of
// secret-bearing query and fragment parameters masked.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Invalid
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), Mask)
	}
	if u.Opaque == "" {
		escaped := redactPath(u.EscapedPath())
		path, err := url.PathUnescape(escaped)
		if err != nil {
			return Invalid
		}
		u.Path, u.RawPath = path, escaped
	}
	u.RawQuery = redactQuery(u.RawQuery, "&;")

	// Single-page apps and OAuth redirects put parameters in the fragment,
	// sometimes behind a client-side route ("#/route?apikey=..."), so '?'
	// separates there too. The escaped form is kept to preserve encoding.
	fragment := u.EscapedFragment()
	u.Fragment, u.RawFragment = "", ""
	out := u.String()
	if fragment != "" {
		out += "#" + redactQuery(fragment, "&;?")
	}
	return out
}

// redactPath masks tracker passkeys, which some trackers embed in the
// announce URL path ("/announce/<passkey>" or "/<passkey>/announce"). A
// segment is masked when it is a hex string of 32 or more characters, or when
// it is a token-looking string (20 or more of [A-Za-z0-9_-]) next to an
// "announce" or "passkey" segment. Ordinary long segments such as release
// names contain other characters (dots, spaces) and are left alone.
func redactPath(escaped string) string {
	segments := strings.Split(escaped, "/")
	out := make([]string, len(segments))
	for i, seg := range segments {
		out[i] = seg
		switch {
		case len(seg) >= 32 && isAll(seg, isHex):
			out[i] = Mask
		case len(seg) >= 20 && isAll(seg, isTokenChar) &&
			(isPasskeyMarker(segments, i-1) || isPasskeyMarker(segments, i+1)):
			out[i] = Mask
		}
	}
	return strings.Join(out, "/")
}

func isPasskeyMarker(segments []string, i int) bool {
	if i < 0 || i >= len(segments) {
		return false
	}
	switch strings.ToLower(segments[i]) {
	case "announce", "passkey":
		return true
	}
	return false
}

func isAll(s string, f func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !f(s[i]) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isTokenChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-'
}

// redactQuery masks secret values in a raw query-style string. Any byte of
// seps separates parameters ('&' and ';' are both accepted by servers), and
// separators are preserved so the result stays recognisable.
func redactQuery(raw, seps string) string {
	var b strings.Builder
	for raw != "" {
		end := strings.IndexAny(raw, seps)
		if end < 0 {
			end = len(raw)
		}
		part := raw[:end]
		if name, _, hasValue := strings.Cut(part, "="); hasValue && isSecretParam(name) {
			part = name + "=" + Mask
		}
		b.WriteString(part)
		if end < len(raw) {
			b.WriteByte(raw[end])
			end++
		}
		raw = raw[end:]
	}
	return b.String()
}

// secretParams are the lower-cased query parameter names whose values are
// masked.
var secretParams = map[string]bool{
	"apikey":   true,
	"api_key":  true,
	"key":      true,
	"token":    true,
	"password": true,
	"pass":     true,
	"nzbkey":   true,
	// Newznab RSS and getnzb links carry the key in r; trackers use
	// passkey-style names. Over-masking is the safe side.
	"r":            true,
	"passkey":      true,
	"authkey":      true,
	"torrent_pass": true,
	"rsskey":       true,
	"apitoken":     true,
	"access_token": true,
}

// isSecretParam reports whether the raw (possibly percent-encoded) query
// parameter name is one of secretParams, ignoring case.
func isSecretParam(rawName string) bool {
	name, err := url.QueryUnescape(rawName)
	if err != nil {
		name = rawName
	}
	return secretParams[strings.ToLower(name)]
}
