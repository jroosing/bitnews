package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// Params are the argon2id cost parameters. Memory is in KiB.
type Params struct {
	Memory  uint32
	Time    uint32
	Threads uint8
	SaltLen int
	KeyLen  int
}

// DefaultParams are the production parameters from the design:
// m=64 MiB, t=3, p=2, 16-byte salt, 32-byte key.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// currentParams is what HashPassword uses; tests lower it via export_test.go.
var currentParams = DefaultParams

var b64 = base64.RawStdEncoding

// HashPassword hashes pw with argon2id and returns a PHC string.
func HashPassword(pw core.Secret) ([]byte, error) {
	p := currentParams
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw.Reveal()), salt, p.Time, p.Memory, p.Threads, uint32(p.KeyLen))
	return []byte(fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))), nil
}

// ErrInvalidHash means a stored hash is malformed, uses an unsupported
// variant, or carries parameters outside the accepted range.
var ErrInvalidHash = errors.New("auth: invalid password hash")

// Bounds on parameters read from a stored hash. They stop a poisoned
// database row from making a login attempt allocate gigabytes or burn CPU.
const (
	maxHashLen   = 512
	maxMemoryKiB = 1 << 20 // 1 GiB
	maxTime      = 10
	maxThreads   = 16
	minSaltLen   = 8
	maxSaltLen   = 64
	minKeyLen    = 16
	maxKeyLen    = 64
)

type parsedHash struct {
	Params
	salt, key []byte
}

// parseHash validates a PHC string without running argon2.
func parseHash(hash []byte) (parsedHash, error) {
	var ph parsedHash
	if len(hash) > maxHashLen {
		return ph, ErrInvalidHash
	}
	parts := strings.Split(string(hash), "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return ph, ErrInvalidHash
	}
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return ph, ErrInvalidHash
	}
	m, ok1 := paramValue(kv[0], "m=")
	t, ok2 := paramValue(kv[1], "t=")
	p, ok3 := paramValue(kv[2], "p=")
	if !ok1 || !ok2 || !ok3 {
		return ph, ErrInvalidHash
	}
	if t < 1 || t > maxTime || p < 1 || p > maxThreads || m < 8*p || m > maxMemoryKiB {
		return ph, ErrInvalidHash
	}
	enc := b64.Strict()
	salt, err1 := enc.DecodeString(parts[4])
	key, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil ||
		len(salt) < minSaltLen || len(salt) > maxSaltLen ||
		len(key) < minKeyLen || len(key) > maxKeyLen {
		return ph, ErrInvalidHash
	}
	ph.Params = Params{Memory: uint32(m), Time: uint32(t), Threads: uint8(p), SaltLen: len(salt), KeyLen: len(key)}
	ph.salt, ph.key = salt, key
	return ph, nil
}

func paramValue(s, prefix string) (uint64, bool) {
	v, ok := strings.CutPrefix(s, prefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 32)
	return n, err == nil
}

// VerifyPassword reports whether pw matches the PHC string hash. It reads the
// parameters from hash, so hashes made with older settings keep working. A
// wrong password is (false, nil); a bad hash is (false, ErrInvalidHash).
func VerifyPassword(hash []byte, pw core.Secret) (bool, error) {
	ph, err := parseHash(hash)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw.Reveal()), ph.salt, ph.Time, ph.Memory, ph.Threads, uint32(ph.KeyLen))
	return subtle.ConstantTimeCompare(got, ph.key) == 1, nil
}

// NeedsRehash reports whether hash should be replaced after a successful
// login: it is unparseable or was made with other parameters than the
// current ones.
func NeedsRehash(hash []byte) bool {
	ph, err := parseHash(hash)
	return err != nil || ph.Params != currentParams
}

// Password length limits. The minimum counts runes; the maximum counts bytes
// and exists so a huge body cannot make argon2 hash megabytes.
const (
	MinPasswordRunes = 12
	MaxPasswordBytes = 1024
)

// Password validation errors.
var (
	ErrPasswordTooShort = fmt.Errorf("auth: password must be at least %d characters", MinPasswordRunes)
	ErrPasswordTooLong  = fmt.Errorf("auth: password must be at most %d bytes", MaxPasswordBytes)
	ErrPasswordInvalid  = errors.New("auth: password is not valid UTF-8")
)

// ValidatePassword checks the length rules. The returned errors never
// contain the password.
func ValidatePassword(pw core.Secret) error {
	s := pw.Reveal()
	switch {
	case len(s) > MaxPasswordBytes:
		return ErrPasswordTooLong
	case !utf8.ValidString(s):
		return ErrPasswordInvalid
	case utf8.RuneCountInString(s) < MinPasswordRunes:
		return ErrPasswordTooShort
	}
	return nil
}
