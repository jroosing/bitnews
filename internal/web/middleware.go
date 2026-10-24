package web

import (
	"bytes"
	"encoding/json"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/jroosing/bitnews/internal/auth"
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

// formCSRF lets a plain (non-JavaScript) form post carry its CSRF token in
// the hidden "_csrf" field. It copies the field into the X-CSRF-Token header
// when the header is absent, before auth.CSRF looks. htmx requests send the
// header themselves (app.js), so this changes nothing for them.
func formCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
			r.Header.Get(auth.CSRFHeader) == "" && isFormContent(r.Header.Get("Content-Type")) {
			if err := r.ParseForm(); err == nil {
				if tok := r.PostForm.Get("_csrf"); tok != "" {
					r.Header.Set(auth.CSRFHeader, tok)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isFormContent(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/x-www-form-urlencoded"
}

// htmx reports whether the request came from htmx.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// htmlErrors turns the JSON errors that the auth middleware writes (401, 403
// CSRF, 421 Host, 500) into UI responses: a redirect to the login page, an
// error page, or, for htmx, a redirect header or a toast. Anything the inner
// handler writes that is not such an error passes through untouched.
func (s *Server) htmlErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iw := &interceptWriter{dst: w, header: http.Header{}}
		next.ServeHTTP(iw, r)
		if !iw.intercepted {
			return
		}
		s.authFailure(w, r, iw.status, iw.errorCode())
	})
}

// interceptWriter holds back a JSON error response so htmlErrors can replace
// it. Its header map is private until the first WriteHeader decides.
type interceptWriter struct {
	dst         http.ResponseWriter
	header      http.Header
	decided     bool
	intercepted bool
	status      int
	body        bytes.Buffer
}

func (w *interceptWriter) Header() http.Header {
	if w.decided && !w.intercepted {
		return w.dst.Header()
	}
	return w.header
}

func (w *interceptWriter) WriteHeader(code int) {
	if w.decided {
		return
	}
	w.decided = true
	if code >= 400 && strings.HasPrefix(w.header.Get("Content-Type"), "application/json") {
		w.intercepted, w.status = true, code
		return
	}
	maps.Copy(w.dst.Header(), w.header)
	w.dst.WriteHeader(code)
}

func (w *interceptWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercepted {
		if w.body.Len() < 4096 {
			w.body.Write(p)
		}
		return len(p), nil
	}
	return w.dst.Write(p)
}

func (w *interceptWriter) Flush() {
	if w.decided && !w.intercepted {
		_ = http.NewResponseController(w.dst).Flush()
	}
}

func (w *interceptWriter) Unwrap() http.ResponseWriter { return w.dst }

// errorCode is the "code" of the intercepted JSON error body.
func (w *interceptWriter) errorCode() string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.body.Bytes(), &body)
	return body.Error.Code
}

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
