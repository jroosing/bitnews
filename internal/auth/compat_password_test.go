package auth_test

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
)

// The Zig version (M15, ADR-0014) must verify every password hash and token
// hash Go wrote and the other way round. crypto.json is written by Go (run
// `go test ./internal/auth -run CompatCrypto -update`) and read by the Zig tests;
// zig_written.json is written by the Zig tests (BITNEWS_UPDATE_GOLDEN=1) and
// read here. Both files carry more sections than this test owns
// ("master_key" and "sealed" belong to internal/store), so an update rewrites
// only "passwords" and "tokens".
var updateCompat = flag.Bool("update", false, "regenerate the golden files in zig/testdata/compat")

const (
	compatDir      = "../../zig/testdata/compat"
	compatGoFile   = "crypto.json"
	compatZigFile  = "zig_written.json"
	compatPassword = "correct horse battery staple"
)

type compatPasswordEntry struct {
	Password string `json:"password"`
	PHC      string `json:"phc"`
}

type compatTokenEntry struct {
	// Kind is "session", "app_key" or "" for a plain string that is only
	// hashed.
	Kind      string `json:"kind"`
	Plaintext string `json:"plaintext"`
	HashHex   string `json:"hash_hex"`
}

type compatAuthFile struct {
	Passwords []compatPasswordEntry `json:"passwords"`
	Tokens    []compatTokenEntry    `json:"tokens"`
}

func TestCompatCryptoPasswordsAndTokensGoldenIsReadableAndCurrent(t *testing.T) {
	if *updateCompat {
		writeCompatAuthGolden(t)
	}
	checkCompatAuth(t, compatGoFile)
}

func TestCompatCryptoPasswordsAndTokensWrittenByZigVerifyInGo(t *testing.T) {
	checkCompatAuth(t, compatZigFile)
}

func checkCompatAuth(t *testing.T, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(compatDir, name))
	require.NoError(t, err, "generate the golden files first")
	var f compatAuthFile
	require.NoError(t, json.Unmarshal(raw, &f))
	require.NotEmpty(t, f.Passwords)
	require.NotEmpty(t, f.Tokens)

	for _, p := range f.Passwords {
		ok, err := auth.VerifyPassword([]byte(p.PHC), core.Secret(p.Password))
		require.NoError(t, err, p.PHC)
		assert.True(t, ok, p.PHC)
		ok, err = auth.VerifyPassword([]byte(p.PHC), core.Secret(p.Password+"x"))
		require.NoError(t, err)
		assert.False(t, ok, "a wrong password must not verify")
	}
	for _, tok := range f.Tokens {
		want, err := hex.DecodeString(tok.HashHex)
		require.NoError(t, err)
		h := auth.HashToken(tok.Plaintext)
		assert.Equal(t, want, h[:], tok.Plaintext)
		if tok.Kind != "" {
			kind, ok := auth.KindOf(tok.Plaintext)
			assert.True(t, ok, tok.Plaintext)
			assert.Equal(t, auth.TokenKind(tok.Kind), kind)
		}
	}
}

func writeCompatAuthGolden(t *testing.T) {
	t.Helper()
	// A modest cost: golden files are verified on every test run.
	auth.SetParamsForTest(t, auth.Params{Memory: 64, Time: 2, Threads: 2, SaltLen: 16, KeyLen: 32})

	var passwords []compatPasswordEntry
	for _, pw := range []string{compatPassword, "pässwörd-日本語-🔑-längd"} {
		phc, err := auth.HashPassword(core.Secret(pw))
		require.NoError(t, err)
		passwords = append(passwords, compatPasswordEntry{Password: pw, PHC: string(phc)})
	}

	var tokens []compatTokenEntry
	for _, kind := range []auth.TokenKind{auth.KindSession, auth.KindAppKey} {
		plain, hash, err := auth.NewKindToken(kind)
		require.NoError(t, err)
		tokens = append(tokens, compatTokenEntry{Kind: string(kind), Plaintext: plain, HashHex: hex.EncodeToString(hash[:])})
	}
	abc := auth.HashToken("abc")
	tokens = append(tokens, compatTokenEntry{Plaintext: "abc", HashHex: hex.EncodeToString(abc[:])})

	sections := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(filepath.Join(compatDir, compatGoFile)); err == nil {
		require.NoError(t, json.Unmarshal(raw, &sections))
	}
	for k, val := range map[string]any{"passwords": passwords, "tokens": tokens} {
		b, err := json.Marshal(val)
		require.NoError(t, err)
		sections[k] = b
	}
	out, err := json.MarshalIndent(sections, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(compatDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(compatDir, compatGoFile), append(out, '\n'), 0o644))
}
