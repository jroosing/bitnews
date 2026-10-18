package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/auth"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestProductionParamsMatchSpec(t *testing.T) {
	p := auth.DefaultParams

	assert.Equal(t, uint32(64*1024), p.Memory, "64 MiB in KiB")
	assert.Equal(t, uint32(3), p.Time)
	assert.Equal(t, uint8(2), p.Threads)
	assert.Equal(t, 16, p.SaltLen)
	assert.Equal(t, 32, p.KeyLen)
}

func TestHashPasswordProducesPHCStringThatVerifies(t *testing.T) {
	auth.UseFastParams(t)

	hash, err := auth.HashPassword(core.Secret("correct horse battery"))
	require.NoError(t, err)
	ok, err := auth.VerifyPassword(hash, core.Secret("correct horse battery"))

	require.NoError(t, err)
	assert.True(t, ok)
	assert.True(t, strings.HasPrefix(string(hash), "$argon2id$v=19$m=8,t=1,p=1$"), string(hash))
	assert.Equal(t, 6, len(strings.Split(string(hash), "$")))
}

func TestVerifyPasswordWrongPasswordIsFalseWithoutError(t *testing.T) {
	auth.UseFastParams(t)
	hash, err := auth.HashPassword(core.Secret("correct horse battery"))
	require.NoError(t, err)

	ok, err := auth.VerifyPassword(hash, core.Secret("incorrect horse battery"))

	require.NoError(t, err)
	assert.False(t, ok)
}

func TestHashPasswordUsesFreshSaltEachTime(t *testing.T) {
	auth.UseFastParams(t)

	a, err := auth.HashPassword(core.Secret("correct horse battery"))
	require.NoError(t, err)
	b, err := auth.HashPassword(core.Secret("correct horse battery"))
	require.NoError(t, err)

	assert.NotEqual(t, string(a), string(b))
}

func TestVerifyPasswordMalformedHashIsErrorNotPanic(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"garbage":          "not a hash",
		"too few parts":    "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHQ",
		"too many parts":   "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA$x",
		"wrong algorithm":  "$argon2i$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"wrong version":    "$argon2id$v=16$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"bad version":      "$argon2id$v=x$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"bad params":       "$argon2id$v=19$m=x,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"missing param":    "$argon2id$v=19$m=8,t=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"extra param":      "$argon2id$v=19$m=8,t=1,p=1,x=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"trailing junk":    "$argon2id$v=19$m=8,t=1,p=1x$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"negative":         "$argon2id$v=19$m=-8,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"bad salt base64":  "$argon2id$v=19$m=8,t=1,p=1$!!!$aGFzaGhhc2hoYXNoaGFzaA",
		"bad hash base64":  "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$!!!",
		"padded base64":    "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA==",
		"empty salt":       "$argon2id$v=19$m=8,t=1,p=1$$aGFzaGhhc2hoYXNoaGFzaA",
		"empty key":        "$argon2id$v=19$m=8,t=1,p=1$c2FsdHNhbHRzYWx0$",
		"zero threads":     "$argon2id$v=19$m=8,t=1,p=0$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"zero time":        "$argon2id$v=19$m=8,t=0,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"memory below 8*p": "$argon2id$v=19$m=7,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := auth.VerifyPassword([]byte(h), core.Secret("whatever password"))

			require.ErrorIs(t, err, auth.ErrInvalidHash)
			assert.False(t, ok)
		})
	}
}

func TestVerifyPasswordRejectsAbsurdParametersBeforeHashing(t *testing.T) {
	const salt, key = "c2FsdHNhbHRzYWx0", "aGFzaGhhc2hoYXNoaGFzaA"
	cases := map[string]string{
		"memory over 1 GiB": "m=1048577,t=1,p=1",
		"huge memory":       "m=4294967295,t=1,p=1",
		"time over 10":      "m=65536,t=11,p=1",
		"huge time":         "m=8,t=4294967295,p=1",
		"threads over 16":   "m=65536,t=1,p=17",
		"huge threads":      "m=65536,t=1,p=255",
		"overflowing":       "m=99999999999999999999,t=1,p=1",
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			h := "$argon2id$v=19$" + params + "$" + salt + "$" + key

			start := time.Now()
			ok, err := auth.VerifyPassword([]byte(h), core.Secret("whatever password"))

			require.ErrorIs(t, err, auth.ErrInvalidHash)
			assert.False(t, ok)
			assert.Less(t, time.Since(start), 500*time.Millisecond, "must not run argon2")
		})
	}
}

func TestVerifyPasswordRejectsOversizedHashString(t *testing.T) {
	h := "$argon2id$v=19$m=8,t=1,p=1$" + strings.Repeat("A", 4000) + "$" + strings.Repeat("A", 40)

	_, err := auth.VerifyPassword([]byte(h), core.Secret("whatever password"))

	assert.ErrorIs(t, err, auth.ErrInvalidHash)
}

func TestVerifyPasswordUsesParametersStoredInTheHash(t *testing.T) {
	auth.UseFastParams(t)
	hash, err := auth.HashPassword(core.Secret("correct horse battery"))
	require.NoError(t, err)
	auth.SetParamsForTest(t, auth.Params{Memory: 16, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32})

	ok, err := auth.VerifyPassword(hash, core.Secret("correct horse battery"))

	require.NoError(t, err)
	assert.True(t, ok)
}

func TestNeedsRehash(t *testing.T) {
	auth.UseFastParams(t)
	hash, err := auth.HashPassword(core.Secret("correct horse battery"))
	require.NoError(t, err)

	assert.False(t, auth.NeedsRehash(hash), "same params")

	auth.SetParamsForTest(t, auth.Params{Memory: 16, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32})
	assert.True(t, auth.NeedsRehash(hash), "memory differs")
	auth.SetParamsForTest(t, auth.Params{Memory: 8, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32})
	assert.True(t, auth.NeedsRehash(hash), "time differs")
	auth.SetParamsForTest(t, auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 64})
	assert.True(t, auth.NeedsRehash(hash), "key length differs")
	assert.True(t, auth.NeedsRehash([]byte("garbage")), "unparseable hashes need replacing")
}

func TestValidatePasswordCountsRunesForMinimumLength(t *testing.T) {
	assert.NoError(t, auth.ValidatePassword(core.Secret("twelve chars")))
	assert.NoError(t, auth.ValidatePassword(core.Secret("日本語日本語日本語日本語")), "12 runes, 36 bytes")

	err := auth.ValidatePassword(core.Secret("eleven char"))
	require.ErrorIs(t, err, auth.ErrPasswordTooShort)
	err = auth.ValidatePassword(core.Secret("日本語日本語日本語日"))
	assert.ErrorIs(t, err, auth.ErrPasswordTooShort, "11 runes")
	assert.ErrorIs(t, auth.ValidatePassword(""), auth.ErrPasswordTooShort)
}

func TestValidatePasswordCapsLengthAt1024Bytes(t *testing.T) {
	assert.NoError(t, auth.ValidatePassword(core.Secret(strings.Repeat("a", 1024))))

	err := auth.ValidatePassword(core.Secret(strings.Repeat("a", 1025)))

	assert.ErrorIs(t, err, auth.ErrPasswordTooLong)
}

func TestValidatePasswordRejectsInvalidUTF8(t *testing.T) {
	err := auth.ValidatePassword(core.Secret("valid prefix \xff\xfe"))

	assert.ErrorIs(t, err, auth.ErrPasswordInvalid)
}

func TestPasswordErrorsNeverContainThePassword(t *testing.T) {
	err := auth.ValidatePassword(core.Secret("hunter2"))

	assert.NotContains(t, err.Error(), "hunter2")
}
