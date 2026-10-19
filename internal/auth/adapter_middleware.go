package auth

import (
	"errors"
	"net/http"
)

// PrincipalAdapter is a media-manager adapter credential. Principal.KeyID is
// the credential's ID. Like an API key it is sent deliberately by the caller,
// so CSRF protection does not apply.
const PrincipalAdapter PrincipalKind = "adapter"

// AdapterRouteOption configures RequireAdapterCredential.
type AdapterRouteOption func(*adapterRoute)

type adapterRoute struct {
	key    func(*http.Request) string
	reject func(http.ResponseWriter, *http.Request, error)
}

// WithAdapterKey replaces where the key is read from. The default is
// "Authorization: Bearer", "X-Api-Key" or the "apikey" query parameter.
func WithAdapterKey(f func(*http.Request) string) AdapterRouteOption {
	return func(a *adapterRoute) { a.key = f }
}

// WithAdapterRejection replaces the answer to a request without a valid
// credential, for adapters that must speak their own error dialect. It gets
// ErrInvalidAdapterCredential, or an internal error from the lookup. The
// default answers 401 in the own API's error shape.
func WithAdapterRejection(f func(http.ResponseWriter, *http.Request, error)) AdapterRouteOption {
	return func(a *adapterRoute) { a.reject = f }
}

// RequireAdapterCredential returns middleware for the routes of one adapter
// (kind is KindAdapterSAB or KindAdapterQBit). It accepts that adapter's
// credential only: own-API keys, session tokens, the other adapter's
// credentials and the UI principals (session, LAN bypass, disable-auth) are
// all refused, and the disable-auth and LAN bypass settings are never
// consulted. On success the request carries a PrincipalAdapter.
//
// The default key lookup is "Authorization: Bearer <key>", "X-Api-Key" or the
// "apikey" query parameter, in that order. It never reads the request body.
func (s *AdapterCredentialService) RequireAdapterCredential(kind TokenKind, opts ...AdapterRouteOption) func(http.Handler) http.Handler {
	route := &adapterRoute{key: defaultAdapterKey, reject: rejectAdapter}
	for _, o := range opts {
		o(route)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info, err := s.Authenticate(r.Context(), kind, route.key(r))
			if err != nil {
				if !errors.Is(err, ErrInvalidAdapterCredential) {
					s.log.Error("adapter authentication failed", "err", err)
				}
				route.reject(w, r, err)
				return
			}
			p := Principal{Kind: PrincipalAdapter, KeyID: info.ID}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
		})
	}
}

func defaultAdapterKey(r *http.Request) string {
	if key, present := credential(r); present {
		return key
	}
	return r.URL.Query().Get("apikey")
}

func rejectAdapter(w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, ErrInvalidAdapterCredential) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid adapter credential is required")
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// UIPrincipalOnly guards the routes that manage adapter credentials. It lets
// through the three UI principals (a session, and also the LAN bypass and
// disable-auth principals) and nothing else: API keys and adapter
// credentials cannot mint credentials. 401 without any principal, 403
// session_required otherwise.
//
// This differs from SessionOnly on purpose. Media managers must be
// configurable, and keep working, when the UI runs with disabled auth or LAN
// bypass; adapters never honour those settings, so the credentials made
// under them are the only thing that authenticates a media manager. They
// outlive the bypass setting. The UI must say so.
//
// Mount it behind KeyOr(write, ui) so that a request carrying an API key is
// judged as an API key request and refused here, instead of falling back to
// the bypass principal.
func UIPrincipalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		switch {
		case !ok:
			writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		case !p.ambient():
			writeError(w, http.StatusForbidden, "session_required", "use the UI to do this: API keys and adapter credentials are refused")
		default:
			next.ServeHTTP(w, r)
		}
	})
}
