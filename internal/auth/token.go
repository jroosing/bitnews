package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"slices"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// TokenKind says what a generated token authenticates. Every kind is the same
// mechanism: 256 random bits, only the SHA-256 hash stored, constant-time
// compare. The kind decides the text prefix. The app key (ADR-0006) is the one
// kind that is not stored as a hash: it is sealed, so Settings can show it.
type TokenKind string

// The token kinds.
const (
	KindSession TokenKind = "session"
	KindAppKey  TokenKind = "app_key"
)

var allKinds = []TokenKind{KindSession, KindAppKey}

// Prefix is the fixed text a token of this kind starts with, so a credential
// pasted in the wrong place is recognisable. Session tokens have none. It is
// empty for an unknown kind.
func (k TokenKind) Prefix() string {
	switch k {
	case KindAppKey:
		return "bitnews_"
	}
	return ""
}

func (k TokenKind) valid() bool {
	return slices.Contains(allKinds, k)
}

// tokenBytes is the entropy of every token: 256 bits.
const tokenBytes = 32

var lowerBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// tokenBodyLen is the length of the encoded random part: 32 bytes in base32.
var tokenBodyLen = lowerBase32.EncodedLen(tokenBytes)

// NewToken returns a fresh session token and its hash: 256 bits from
// crypto/rand, encoded as lowercase base32 without padding.
func NewToken() (plaintext string, hash core.TokenHash, err error) {
	return NewKindToken(KindSession)
}

// NewKindToken is NewToken for any kind: the kind's prefix followed by the
// encoded random bytes. The plaintext is shown to its owner once; only the
// hash is stored.
func NewKindToken(kind TokenKind) (plaintext string, hash core.TokenHash, err error) {
	if !kind.valid() {
		return "", core.TokenHash{}, fmt.Errorf("auth: unknown token kind %q", kind)
	}
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", core.TokenHash{}, fmt.Errorf("auth: generate token: %w", err)
	}
	plaintext = kind.Prefix() + strings.ToLower(lowerBase32.EncodeToString(raw[:]))
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the SHA-256 hash under which a token is stored and looked
// up. A fast hash is right here because tokens are high-entropy; passwords go
// through argon2id instead.
func HashToken(plaintext string) core.TokenHash {
	return sha256.Sum256([]byte(plaintext))
}

// TokenHashEqual compares two hashes in constant time.
func TokenHashEqual(a, b core.TokenHash) bool {
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// KindOf reports which kind of token s looks like, by prefix and shape. It
// only checks the format: a "true" says nothing about whether the token is
// valid. It lets a credential check reject garbage before hashing it.
func KindOf(s string) (TokenKind, bool) {
	// Longest prefix first is unnecessary: no prefix is a prefix of another
	// (each ends in "_", which the encoded body never contains).
	for _, k := range allKinds {
		body, ok := strings.CutPrefix(s, k.Prefix())
		if !ok || len(body) != tokenBodyLen {
			continue
		}
		if isLowerBase32(body) {
			return k, true
		}
	}
	return "", false
}

func isLowerBase32(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
