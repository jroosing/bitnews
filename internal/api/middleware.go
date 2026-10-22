package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/redact"
)

type ctxKey int

const loggerKey ctxKey = iota

// logFrom returns the request-scoped logger, which carries the request ID.
func logFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

const requestIDHeader = "X-Request-ID"

// validRequestID reports whether a client-supplied request ID is safe to
// echo and log: 1 to 64 characters of [A-Za-z0-9._-].
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := range len(id) {
		switch c := id[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error.
	return hex.EncodeToString(b[:])
}

// statusWriter records what a handler wrote, for the access log and the
// panic recovery. It forwards Flush and exposes the wrapped writer through
// Unwrap, so http.ResponseController keeps working through it.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack lets protocol upgrades work through the wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// wrapRequest is the outermost middleware. It gives the request an ID, echoes
// it and puts a logger carrying it into the context, and wraps the writer in
// a statusWriter for the middlewares inside it (recoverPanics, accessLog).
func (s *Server) wrapRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), loggerKey, s.log.With("request_id", id))
		next.ServeHTTP(&statusWriter{ResponseWriter: w}, r.WithContext(ctx))
	})
}

// recoverPanics turns a panic in a handler into a generic 500 and logs the
// panic value and stack on the server only.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			logFrom(r.Context()).Error("panic in handler", "panic", v, "stack", string(debug.Stack()))
			if sw, ok := w.(*statusWriter); !ok || sw.status == 0 {
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog logs one line per request. URLs go through redact.URL, because a
// query can carry an API key.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw, _ := w.(*statusWriter)
		completed := false
		defer func() {
			status, size := http.StatusInternalServerError, int64(0)
			if completed && sw != nil {
				status, size = max(sw.status, http.StatusOK), sw.bytes
			}
			level := slog.LevelInfo
			if r.URL.Path == "/api/v1/health" && status == http.StatusOK {
				level = slog.LevelDebug // health checks poll; keep them out of the default log
			}
			logFrom(r.Context()).LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("url", loggedURL(r)),
				slog.Int("status", status),
				slog.Int64("bytes", size),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		}()
		next.ServeHTTP(w, r)
		completed = true
	})
}

// loggedURL is the request target as it may be logged. URLs go through
// redact.URL, because a query can carry an API key.
//
// The SABnzbd adapter is the exception: its query holds the adapter key and,
// for mode=addurl, a second URL of an indexer with its own key in an encoded
// parameter value, which redact.URL does not look into. Only the path and
// the mode are logged for it. The qBittorrent adapter (/api/v2/) logs the
// path only: its queries hold hashes and its form bodies, never logged,
// hold the magnet links and URLs with passkeys.
func loggedURL(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/api/v2/") {
		return r.URL.EscapedPath()
	}
	if r.URL.Path != "/sabnzbd/api" && !strings.HasPrefix(r.URL.Path, "/sabnzbd/api/") {
		return redact.URL(r.URL.RequestURI())
	}
	mode := strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			return c
		}
		return -1
	}, r.URL.Query().Get("mode"))
	if len(mode) > 32 {
		mode = mode[:32]
	}
	if mode == "" {
		return r.URL.EscapedPath()
	}
	return r.URL.EscapedPath() + "?mode=" + mode
}

// securityHeaders sets the headers every response carries.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
