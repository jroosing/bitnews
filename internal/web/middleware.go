package web

import (
	"net/http"
	"net/url"
	"path"
	"strings"
)

// contentSecurityPolicy is the strict CSP of every UI response: only the
// daemon's own origin, no inline script or style, no eval.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// maxFormBody caps request bodies of the UI's form posts.
const maxFormBody = 64 << 10

// secure sets the UI's security headers on every response. It replaces the
// API's looser CSP for UI paths only (the API keeps its own header), and
// makes responses uncacheable by default: the static handler overrides that
// for assets.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			limit := int64(maxFormBody)
			if n, ok := uploadLimits[r.URL.Path]; ok {
				limit = n
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// htmx reports whether the request came from htmx.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// authFailure answers a failed authentication or CSRF check.
func (s *Server) authFailure(w http.ResponseWriter, r *http.Request, status int, code string) {
	switch {
	case status == http.StatusUnauthorized && strings.HasPrefix(r.Header.Get("Accept"), "text/event-stream"):
		// An event stream cannot follow a redirect to the login page.
		w.WriteHeader(status)
	case status == http.StatusUnauthorized:
		target := "/login"
		if r.Method == http.MethodGet && !isHTMX(r) {
			if next := safeNext(r.URL.RequestURI(), s.knownPath); next != "" && next != "/" {
				target += "?next=" + url.QueryEscape(next)
			}
		}
		s.redirect(w, r, target)
	case status == http.StatusForbidden && strings.HasPrefix(code, "csrf_"):
		s.renderError(w, r, status, "Please reload the page",
			"bitnews could not verify that this request came from its own page. Reload the page and try again.")
	case status == http.StatusMisdirectedRequest:
		s.renderError(w, r, status, "Address not allowed",
			"This address is not in bitnews's list of allowed hosts. Open bitnews at an address you have allowed in Settings, or from the server itself.")
	case status == http.StatusInternalServerError:
		s.renderError(w, r, status, "Something went wrong", "bitnews could not complete the request. Details are in the server log.")
	default:
		s.renderError(w, r, status, "Request refused", "bitnews refused this request.")
	}
}

// redirect sends the browser to target: HX-Redirect for htmx (which ignores
// ordinary redirects of fetches), 303 otherwise.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeNext validates a post-login redirect target. It returns "" unless the
// value is a same-origin absolute path (no scheme, host, backslash, control
// character or "//" prefix) that known accepts, so it can never be an open
// redirect.
func safeNext(next string, known func(path string) bool) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\x00\r\n\t") {
		return ""
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || path.Clean(u.Path) != u.Path || !known(u.Path) {
		return ""
	}
	return next
}
