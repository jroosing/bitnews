package store_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/store"
)

func TestSealedSecretOpensToTheOriginalPlaintext(t *testing.T) {
	key := store.GenerateKey()
	aad := store.AAD("servers", "password", "srv1")

	sealed, err := store.Seal(key, "s3cret-pa55word", aad)
	require.NoError(t, err)
	assert.NotContains(t, sealed, "s3cret-pa55word")

	opened, err := store.Unseal(key, sealed, aad)
	require.NoError(t, err)
	assert.Equal(t, "s3cret-pa55word", opened)
}

func TestSealUsesAFreshNonceEveryTime(t *testing.T) {
	key := store.GenerateKey()
	aad := store.AAD("servers", "password", "srv1")

	a, err := store.Seal(key, "same", aad)
	require.NoError(t, err)
	b, err := store.Seal(key, "same", aad)
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

func TestSealedFormatIsVersionedBase64OfNonceAndCiphertext(t *testing.T) {
	sealed, err := store.Seal(store.GenerateKey(), "abc", "a/b/c")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sealed, "v1:"))
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	require.NoError(t, err)
	// 24-byte nonce + plaintext + 16-byte Poly1305 tag.
	assert.Len(t, raw, 24+len("abc")+16)
}

func TestEmptyPlaintextRoundTrips(t *testing.T) {
	key := store.GenerateKey()
	sealed, err := store.Seal(key, "", "a/b/c")
	require.NoError(t, err)
	opened, err := store.Unseal(key, sealed, "a/b/c")
	require.NoError(t, err)
	assert.Empty(t, opened)
}

func TestUnsealFailsWithErrDecrypt(t *testing.T) {
	key := store.GenerateKey()
	aad := store.AAD("servers", "password", "srv1")
	sealed, err := store.Seal(key, "s3cret", aad)
	require.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	require.NoError(t, err)
	flip := func(i int) string {
		c := bytes.Clone(raw)
		c[i] ^= 0x01
		return "v1:" + base64.StdEncoding.EncodeToString(c)
	}

	tests := []struct {
		name   string
		key    store.Key
		stored string
		aad    string
	}{
		{"wrong key", store.GenerateKey(), sealed, aad},
		{"wrong AAD", key, sealed, store.AAD("servers", "password", "srv2")},
		{"AAD from another column", key, sealed, store.AAD("servers", "username", "srv1")},
		{"tampered nonce", key, flip(0), aad},
		{"tampered ciphertext", key, flip(24), aad},
		{"tampered tag", key, flip(len(raw) - 1), aad},
		{"truncated ciphertext", key, "v1:" + base64.StdEncoding.EncodeToString(raw[:len(raw)-1]), aad},
		{"tag only", key, "v1:" + base64.StdEncoding.EncodeToString(raw[:24]), aad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.Unseal(tt.key, tt.stored, tt.aad)
			require.ErrorIs(t, err, store.ErrDecrypt)
			assert.Empty(t, got)
			assert.NotContains(t, err.Error(), "s3cret")
		})
	}
}

func TestCiphertextCannotBeSwappedBetweenRows(t *testing.T) {
	key := store.GenerateKey()
	sealedForRow1, err := store.Seal(key, "pw-of-row-1", store.AAD("servers", "password", "1"))
	require.NoError(t, err)

	_, err = store.Unseal(key, sealedForRow1, store.AAD("servers", "password", "2"))
	require.ErrorIs(t, err, store.ErrDecrypt)
}

func TestUnsealFailsWithErrUnsupportedFormat(t *testing.T) {
	key := store.GenerateKey()
	aad := store.AAD("servers", "password", "srv1")
	sealed, err := store.Seal(key, "s3cret", aad)
	require.NoError(t, err)
	body := strings.TrimPrefix(sealed, "v1:")

	for name, stored := range map[string]string{
		"empty":                 "",
		"no version":            body,
		"unknown version":       "v2:" + body,
		"version only":          "v1:",
		"bad base64":            "v1:!!!not-base64!!!",
		"shorter than a nonce":  "v1:" + base64.StdEncoding.EncodeToString([]byte("short")),
		"raw plaintext":         "s3cret",
		"uppercase version":     "V1:" + body,
		"url-safe base64 alien": "v1:" + base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 48)),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := store.Unseal(key, stored, aad)
			require.ErrorIs(t, err, store.ErrUnsupportedFormat)
			assert.NotErrorIs(t, err, store.ErrDecrypt)
			assert.Empty(t, got)
		})
	}
}

// Every sealed secret has exactly one valid spelling, so a stored value cannot
// be altered in a way that still opens (padding bits, line breaks).
func TestUnsealRejectsNonCanonicalBase64(t *testing.T) {
	key := store.GenerateKey()
	// Plaintext of length 1 makes the base64 body end in "=" padding with
	// spare bits in the last character that a lenient decoder ignores.
	sealed, err := store.Seal(key, "x", "a/b/c")
	require.NoError(t, err)
	body := strings.TrimPrefix(sealed, "v1:")
	require.True(t, strings.HasSuffix(body, "="), "test needs padding: %s", body)

	last := body[len(body)-2]
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	idx := strings.IndexByte(alphabet, last)
	variant := body[:len(body)-2] + string(alphabet[idx^0x01]) + "="

	for name, stored := range map[string]string{
		"trailing newline": sealed + "\n",
		"trailing CRLF":    sealed + "\r\n",
		"embedded newline": "v1:" + body[:10] + "\n" + body[10:],
		"spare bits set":   "v1:" + variant,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := store.Unseal(key, stored, "a/b/c")
			require.ErrorIs(t, err, store.ErrUnsupportedFormat)
		})
	}
}

// goldenKey and goldenAAD produced goldenSealed with Seal. The vectors pin the
// v1 format (a stored value must stay readable after upgrades) and give the
// fuzzer valid seeds, since Seal's random nonce cannot be replayed inside the
// fuzzing worker processes.
var (
	goldenKey    = bytes.Repeat([]byte{7}, 32)
	goldenAAD    = "servers/password/srv1"
	goldenSealed = map[string]string{ // stored -> plaintext
		"v1:DnxZtI7UwsmrHEICstYnHoH/WTzRhgoBzAvama/frmzffYEFabpmkw==":                     "",
		"v1:4+2w0atsQsGSkU/7N/MdXWq5Ep4DIWg1zkJadqh7TbPB+eZrwvH/tEQ=":                     "x",
		"v1:i4aH7d4+0/s9XhgzCrJFd18/dsZaJGZN3ozg+R6JDFf9eu4Cn44JPCUCChmlMzeYECm2ZglpkQ==": "s3cret-pa55word",
		"v1:HgXd3WfoTCnWfaglAqLHWVLF1R4IY/OEiv78/JMh3TWXbmdoz77ACKuDt0JMZHR1mYzcGNhkX9859gq74DRa5GpyBA35tCp3FlLP6w4kkrbRP0AL+sQJ7wXKEPgXzxvyxE/c0kxnD2PaIQ8cNtfl3/EJ4v4qs+ql": strings.Repeat("long", 20),
	}
)

func goldenStoreKey(tb testing.TB) store.Key {
	tb.Helper()
	k, err := store.ParseKey(base64.StdEncoding.EncodeToString(goldenKey))
	require.NoError(tb, err)
	return k
}

func TestUnsealReadsValuesSealedByEarlierVersions(t *testing.T) {
	key := goldenStoreKey(t)
	for stored, want := range goldenSealed {
		got, err := store.Unseal(key, stored, goldenAAD)
		require.NoError(t, err, stored)
		assert.Equal(t, want, got)
	}
}

// FuzzOpen feeds arbitrary stored values to Unseal (the "open" half of the
// secret box). It must never panic, must fail with one of the two sentinels,
// and may only succeed for a value that Seal produced.
func FuzzOpen(f *testing.F) {
	key := goldenStoreKey(f)
	for stored := range goldenSealed {
		f.Add(stored)
	}
	for _, seed := range []string{"", "v1:", "v1:AAAA", "v2:AAAA", "v1:!!!!", "v1:" + strings.Repeat("A", 31) + "=", "\x00", "v1:\xff\xfe"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, stored string) {
		got, err := store.Unseal(key, stored, goldenAAD)
		if err != nil {
			assert.True(t, errors.Is(err, store.ErrDecrypt) || errors.Is(err, store.ErrUnsupportedFormat), "unexpected error %v", err)
			assert.Empty(t, got)
			return
		}
		want, ok := goldenSealed[stored]
		require.True(t, ok, "Unseal accepted a value Seal never produced: %q", stored)
		assert.Equal(t, want, got)
	})
}
