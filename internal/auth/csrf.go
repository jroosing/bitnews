package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// CSRFHeader carries the synchronizer token on state-changing requests.
const CSRFHeader = "X-CSRF-Token"

const csrfContext = "bitnews-csrf-v1"

// CSRFToken derives the synchronizer token of a session:
// hex(HMAC-SHA256(key = session token, message = "bitnews-csrf-v1")).
// Deriving it needs no storage, only the session token, which the server
// sees in the cookie. A page on another origin can neither read it (same
// origin policy) nor compute it (it never sees the cookie).
func CSRFToken(sessionToken string) string {
	mac := hmac.New(sha256.New, []byte(sessionToken))
	mac.Write([]byte(csrfContext))
	return hex.EncodeToString(mac.Sum(nil))
}

// CSRF returns middleware that protects state-changing requests (anything but
// GET, HEAD and OPTIONS) from cross-site forgery. Mount it after the
// authentication middleware.
//
//   - The Origin header, if present, must name the request's own Host or one
//     of the allowed_hosts. "null" and malformed values are refused.
//   - A request authenticated by cookie, LAN bypass or disable-auth must also
//     send the principal's token in X-CSRF-Token.
//   - An API-key request is exempt from both: its credential is not ambient.
//   - A request with no principal (login) gets only the Origin check.
//
// Failures are 403.
func CSRF(settings func() core.Settings) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if safeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			p, hasPrincipal := FromContext(r.Context())
			if hasPrincipal && !p.ambient() {
				next.ServeHTTP(w, r)
				return
			}
			if !originAllowed(r.Header.Values("Origin"), r.Host, settings().Auth.AllowedHosts) {
				writeError(w, http.StatusForbidden, "csrf_origin_invalid", "the Origin header does not match this server")
				return
			}
			if hasPrincipal && !tokenMatches(r.Header.Get(CSRFHeader), p.csrf) {
				writeError(w, http.StatusForbidden, "csrf_token_invalid", "the X-CSRF-Token header is missing or wrong")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// tokenMatches compares in constant time. An empty expected token never
// matches, so a principal built without one cannot be satisfied by an
// empty header.
func tokenMatches(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// originAllowed applies the Origin rule: absent is fine (non-browser or
// same-origin GET-like request); present must be one origin whose host is the
// request's Host or an allowed host.
func originAllowed(origins []string, requestHost string, allowedHosts []string) bool {
	switch len(origins) {
	case 0:
		return true
	case 1:
	default:
		return false
	}
	u, err := url.Parse(origins[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return false
	}
	if strings.EqualFold(u.Host, requestHost) {
		return true
	}
	return hostInList(u.Host, allowedHosts)
}

// hostInList reports whether host (a Host header value, port ignored) is in
// the list. Entries may carry a port; it is ignored too.
func hostInList(host string, list []string) bool {
	h := hostOnly(host)
	if h == "" {
		return false
	}
	for _, e := range list {
		if hostOnly(e) == h {
			return true
		}
	}
	return false
}

// hostOnly lowercases a host[:port] value and strips the port, the brackets
// of an IPv6 literal and a trailing dot.
func hostOnly(hostport string) string {
	h := strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	} else {
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// HostAllowed reports whether host (a Host header value) is one of the
// allowed hosts. Ports, case, the brackets of an IPv6 literal and a trailing
// dot do not matter. It is the check UIAuth makes while disable auth or LAN
// bypass is on, exported so the settings page can warn before an edit would
// lock the user out.
func HostAllowed(host string, allowed []string) bool { return hostInList(host, allowed) }
