package store_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/store"
)

// The Zig version (M15, ADR-0014) must open every secret sealed by Go and the
// other way round. crypto.json is written by Go (run
// `go test ./internal/store -run CompatCrypto -update`; the flag is declared in
// compat_db_test.go) and read by the Zig tests; zig_written.json is written
// by the Zig tests (BITNEWS_UPDATE_GOLDEN=1) and read here. Both files carry
// more sections than this test owns ("passwords" and "tokens" belong to
// internal/auth), so an update rewrites only "master_key" and "sealed".
const (
	compatCryptoDir  = "../../zig/testdata/compat"
	compatCryptoFile = "crypto.json"
	compatZigFile    = "zig_written.json"
)

type compatSealed struct {
	Table     string `json:"table"`
	Column    string `json:"column"`
	RowID     string `json:"row_id"`
	Plaintext string `json:"plaintext"`
	Stored    string `json:"stored"`
}

type compatSealedFile struct {
	MasterKey string `json:"master_key"`
	// KeyFile is the complete content of a key file, newline included.
	KeyFile string         `json:"key_file"`
	Sealed  []compatSealed `json:"sealed"`
}

func readCompatSections(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(compatCryptoDir, name))
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}
	}
	require.NoError(t, err)
	sections := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(raw, &sections))
	return sections
}

func TestCompatCryptoGoldenIsReadableAndCurrent(t *testing.T) {
	if *update {
		writeCompatCryptoGolden(t)
	}
	openCompatSealed(t, compatCryptoFile)
}

func TestCompatCryptoSealedByZigOpensInGo(t *testing.T) {
	openCompatSealed(t, compatZigFile)
}

// openCompatSealed opens every sealed value of a golden file with the Go code.
func openCompatSealed(t *testing.T, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(compatCryptoDir, name))
	require.NoError(t, err, "generate the golden files first")
	var f compatSealedFile
	require.NoError(t, json.Unmarshal(raw, &f))
	require.NotEmpty(t, f.Sealed)

	key, err := store.ParseKey(f.MasterKey)
	require.NoError(t, err)

	// The key file is read through the real loader and is the same key.
	path := filepath.Join(t.TempDir(), "bitnews.key")
	require.NoError(t, os.WriteFile(path, []byte(f.KeyFile), 0o600))
	fromFile, err := store.LoadOrCreateKey(path, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Equal(t, key, fromFile)

	for _, s := range f.Sealed {
		got, err := store.Unseal(key, s.Stored, store.AAD(s.Table, s.Column, s.RowID))
		require.NoError(t, err, "%s/%s/%s", s.Table, s.Column, s.RowID)
		assert.Equal(t, s.Plaintext, got)

		_, err = store.Unseal(key, s.Stored, store.AAD(s.Table, s.Column, s.RowID+"x"))
		require.ErrorIs(t, err, store.ErrDecrypt, "the AAD binds the row")
	}
}

func writeCompatCryptoGolden(t *testing.T) {
	t.Helper()
	// A key file written by the Go code itself, as a real installation has it.
	keyPath := filepath.Join(t.TempDir(), "keys", "bitnews.key")
	key, err := store.LoadOrCreateKey(keyPath, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	keyFile, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	masterKey := strings.TrimSpace(string(keyFile))

	entries := []compatSealed{
		{Table: "servers", Column: "password", RowID: "srv1", Plaintext: "s3cret-pa55word"},
		{Table: "servers", Column: "username", RowID: "srv1", Plaintext: ""},
		{Table: "servers", Column: "password", RowID: "srv-é", Plaintext: "pässwörd-日本語-🔑"},
		{Table: "app_key", Column: "key", RowID: "1", Plaintext: "bitnews_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqr"},
		{Table: "t", Column: "c", RowID: "", Plaintext: "x"},
		{Table: "settings", Column: "value", RowID: "long", Plaintext: string(bytes.Repeat([]byte("0123456789"), 40))},
		{Table: "t", Column: "c", RowID: "r", Plaintext: "line1\nline2\t\"quoted\" \\ <&>"},
	}
	for i := range entries {
		e := &entries[i]
		e.Stored, err = store.Seal(key, e.Plaintext, store.AAD(e.Table, e.Column, e.RowID))
		require.NoError(t, err)
	}

	sections := readCompatSections(t, compatCryptoFile)
	for k, val := range map[string]any{"master_key": masterKey, "key_file": string(keyFile), "sealed": entries} {
		b, err := json.Marshal(val)
		require.NoError(t, err)
		sections[k] = b
	}
	out, err := json.MarshalIndent(sections, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(compatCryptoDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(compatCryptoDir, compatCryptoFile), append(out, '\n'), 0o644))
}
