package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

var (
	// ErrDecrypt means a stored secret could not be decrypted: the key or the
	// AAD is wrong, or the value was tampered with. The causes are
	// deliberately indistinguishable.
	ErrDecrypt = errors.New("store: cannot decrypt secret (wrong key, wrong row, or corrupted data)")
	// ErrUnsupportedFormat means a stored value is not in a known sealed format.
	ErrUnsupportedFormat = errors.New("store: unsupported secret format")
)

const formatV1 = "v1:"

// AAD builds the additional authenticated data that binds a ciphertext to the
// table, column and row it is stored in, so ciphertexts cannot be swapped
// between rows.
func AAD(table, column, rowID string) string {
	return table + "/" + column + "/" + rowID
}

// Seal encrypts plaintext with XChaCha20-Poly1305 under key and a fresh
// random nonce. The result is "v1:" followed by base64(nonce || ciphertext).
func Seal(key Key, plaintext, aad string) (string, error) {
	aead, err := chacha20poly1305.NewX(key.b[:])
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return formatV1 + base64.StdEncoding.EncodeToString(sealed), nil
}

// Unseal decrypts a value produced by Seal with the same key and aad. (It is not
// called Open because that name belongs to opening the database.)
func Unseal(key Key, stored, aad string) (string, error) {
	body, ok := strings.CutPrefix(stored, formatV1)
	if !ok {
		return "", ErrUnsupportedFormat
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	// Accept only the canonical spelling: the decoder ignores line breaks and
	// spare padding bits, which would give one secret many valid encodings.
	if err != nil || len(raw) < chacha20poly1305.NonceSizeX || base64.StdEncoding.EncodeToString(raw) != body {
		return "", ErrUnsupportedFormat
	}
	aead, err := chacha20poly1305.NewX(key.b[:])
	if err != nil {
		return "", err
	}
	nonce, ciphertext := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}
