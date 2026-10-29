package auth

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jroosing/bitnews/internal/core"
)

// CSRF returns middleware that protects state-changing requests (anything but
// GET, HEAD and OPTIONS) from cross-site forgery with the standard library's
// http.CrossOriginProtection (CheckOrigin). A browser says where a request
// comes from (Sec-Fetch-Site, or else Origin against Host); one from another
// site is refused unless it comes from one of the allowed hosts. A request
// with neither header (a script, curl) passes: it carries no credential a
// hostile page could make the browser send. An API-key request is exempt: its
// credential is not ambient. Mount it after the authentication middleware.
//
// Failures are 403 csrf_origin_invalid, answered by fail (nil: JSON).
func CSRF(settings func() core.Settings, fail Failure) func(http.Handler) http.Handler {
	fail = orJSON(fail)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := FromContext(r.Context()); ok && !p.ambient() {
				next.ServeHTTP(w, r)
				return
			}
			if err := CheckOrigin(r, settings().Auth.AllowedHosts); err != nil {
				fail(w, r, http.StatusForbidden, "csrf_origin_invalid", "the request comes from another site")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CheckOrigin is the cross-origin rule of bitnews: a request from a page
// under one of the allowed hosts (any scheme of http and https, any port)
// passes; otherwise the standard library's http.CrossOriginProtection
// decides, which lets same-origin requests and requests without Origin and
// Sec-Fetch-Site through and refuses the rest of the unsafe ones.
func CheckOrigin(r *http.Request, allowedHosts []string) error {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && hostInList(u.Host, allowedHosts) {
			return nil
		}
	}
	return http.NewCrossOriginProtection().Check(r)
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
