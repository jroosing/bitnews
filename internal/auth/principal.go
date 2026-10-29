package auth

import (
	"context"

	"github.com/jroosing/bitnews/internal/core"
)

// PrincipalKind says how a request was authenticated.
type PrincipalKind string

// The principal kinds. The first three are ambient: the browser attaches the
// credential (cookie) or the network position does, so CSRF protection
// applies. An API key is sent deliberately by the caller, so it does not.
const (
	// PrincipalSession is a logged-in UI session (cookie).
	PrincipalSession PrincipalKind = "session"
	// PrincipalLANBypass is an unauthenticated UI request from a
	// configured LAN bypass network.
	PrincipalLANBypass PrincipalKind = "bypass"
	// PrincipalAuthDisabled is an unauthenticated UI request while the
	// disable_auth setting is on.
	PrincipalAuthDisabled PrincipalKind = "disabled"
	// PrincipalAPIKey is the app key on the own API (Authorization: Bearer
	// or X-Api-Key). It has full access.
	PrincipalAPIKey PrincipalKind = "api_key"
)

// Principal is who a request acts as, put into the request context by the
// authentication middleware. Read it with FromContext.
type Principal struct {
	Kind PrincipalKind
	// UserID and Username are set for PrincipalSession only.
	UserID   core.ID
	Username string
}

// ambient reports whether the credential travels without the caller choosing
// to send it, which is when CSRF protection is needed.
func (p Principal) ambient() bool {
	switch p.Kind {
	case PrincipalSession, PrincipalLANBypass, PrincipalAuthDisabled:
		return true
	}
	return false
}

// AuthMode is the value of auth_mode in the session info: "session",
// "bypass" or "disabled". It is empty for an API key.
func (p Principal) AuthMode() string {
	if p.ambient() {
		return string(p.Kind)
	}
	return ""
}

type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the Principal the authentication middleware stored, if
// the request was authenticated.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
