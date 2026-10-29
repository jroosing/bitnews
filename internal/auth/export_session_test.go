package auth

import "github.com/jroosing/bitnews/internal/core"

// WithVerifier replaces the password verifier, so tests can observe which
// hash a login checks against without timing anything.
func WithVerifier(fn func(hash []byte, pw core.Secret) (bool, error)) SessionOption {
	return func(s *SessionService) { s.verify = fn }
}

// ContextWithPrincipal puts p into ctx, as the authentication middleware does.
var ContextWithPrincipal = withPrincipal
