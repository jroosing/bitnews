package auth_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestNewTokenIs256BitLowercaseBase32WithoutPadding(t *testing.T) {
	plain, hash, err := auth.NewToken()

	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^[a-z2-7]{52}$`), plain, "32 bytes = 52 base32 chars")
	assert.Equal(t, auth.HashToken(plain), hash)
}

func TestNewTokenIsUnique(t *testing.T) {
	a, _, _ := auth.NewToken()
	b, _, _ := auth.NewToken()

	assert.NotEqual(t, a, b)
}

func TestHashTokenIsSHA256(t *testing.T) {
	// echo -n abc | sha256sum
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	got := auth.HashToken("abc")

	assert.Equal(t, want, hexOf(got))
}

func TestTokenHashEqual(t *testing.T) {
	a, b := auth.HashToken("a"), auth.HashToken("b")

	assert.True(t, auth.TokenHashEqual(a, a))
	assert.False(t, auth.TokenHashEqual(a, b))
}

func TestTokenKindsHaveDistinctPrefixes(t *testing.T) {
	prefixes := map[string]auth.TokenKind{}
	for _, k := range []auth.TokenKind{auth.KindSession, auth.KindAppKey} {
		p := k.Prefix()
		if other, dup := prefixes[p]; dup {
			t.Fatalf("kinds %q and %q share prefix %q", k, other, p)
		}
		prefixes[p] = k
	}
	assert.Equal(t, "bitnews_", auth.KindAppKey.Prefix())
	assert.Equal(t, "", auth.KindSession.Prefix())
}

func TestNewKindTokenCarriesKindPrefix(t *testing.T) {
	plain, hash, err := auth.NewKindToken(auth.KindAppKey)

	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^bitnews_[a-z2-7]{52}$`), plain)
	assert.Equal(t, auth.HashToken(plain), hash)
	k, ok := auth.KindOf(plain)
	assert.True(t, ok)
	assert.Equal(t, auth.KindAppKey, k)
}

func TestNewKindTokenRejectsUnknownKind(t *testing.T) {
	_, _, err := auth.NewKindToken("bogus")

	assert.Error(t, err)
}

func TestKindOfRecognisesEveryKindAndRejectsGarbage(t *testing.T) {
	for _, k := range []auth.TokenKind{auth.KindSession, auth.KindAppKey} {
		plain, _, err := auth.NewKindToken(k)
		require.NoError(t, err)
		got, ok := auth.KindOf(plain)
		assert.True(t, ok, k)
		assert.Equal(t, k, got)
	}
	_, ok := auth.KindOf("bitnews_short")
	assert.False(t, ok)
	_, ok = auth.KindOf("")
	assert.False(t, ok)
}

func hexOf(h core.TokenHash) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range h {
		out = append(out, digits[b>>4], digits[b&15])
	}
	return string(out)
}
