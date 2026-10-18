package store

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// keySize is the size of the master key in bytes (XChaCha20-Poly1305).
const keySize = 32

// Key is the master key that encrypts recoverable secrets.
type Key struct{ b [keySize]byte }

// GenerateKey returns a fresh random Key.
func GenerateKey() Key {
	var k Key
	_, _ = rand.Read(k.b[:]) // crypto/rand.Read never returns an error.
	return k
}

// ParseKey decodes a key in its key-file form: 32 bytes, base64-encoded.
func ParseKey(encoded string) (Key, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Key{}, errors.New("key is not valid base64")
	}
	if len(raw) != keySize {
		return Key{}, fmt.Errorf("key must be %d bytes, got %d", keySize, len(raw))
	}
	var k Key
	copy(k.b[:], raw)
	return k, nil
}

const keyRedacted = "[redacted]"

// Key never reveals itself: every fmt verb, encoder and slog handler shows
// "[redacted]". Format covers verbs such as %d and %x that bypass String.

// Format implements fmt.Formatter.
func (Key) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, keyRedacted) }

// String implements fmt.Stringer.
func (Key) String() string { return keyRedacted }

// GoString implements fmt.GoStringer.
func (Key) GoString() string { return keyRedacted }

// MarshalText implements encoding.TextMarshaler.
func (Key) MarshalText() ([]byte, error) { return []byte(keyRedacted), nil }

// MarshalJSON implements json.Marshaler.
func (Key) MarshalJSON() ([]byte, error) { return []byte(`"` + keyRedacted + `"`), nil }

// LogValue implements slog.LogValuer.
func (Key) LogValue() slog.Value { return slog.StringValue(keyRedacted) }

// LoadOrCreateKey returns the master key stored at path. The file holds 32
// random bytes, base64-encoded, followed by a newline.
//
// A missing file is generated with mode 0600 (and its directory with 0700 if
// that is missing too) and its location is logged at WARN level, because
// losing the file makes the encrypted secrets unrecoverable. An existing file
// whose permissions are broader than 0600 only produces a warning, since
// Docker secrets are often mode 0444.
func LoadOrCreateKey(path string, logger *slog.Logger) (Key, error) {
	if logger == nil {
		logger = slog.Default()
	}
	for {
		key, err := readKeyFile(path, logger)
		if !errors.Is(err, fs.ErrNotExist) {
			return key, err
		}
		key, err = createKeyFile(path, logger)
		if !errors.Is(err, fs.ErrExist) { // Lost a race with another process: read its file.
			return key, err
		}
	}
}

func createKeyFile(path string, logger *slog.Logger) (Key, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Key{}, fmt.Errorf("key file %q does not exist and its directory cannot be created: %w\n%s", path, err, keyFileHelp(path))
	}
	// Write the complete key to a private temp file first and publish it with
	// a hard link. Link fails with ErrExist when another process won the
	// race, and the published file is never visible half-written, so a loser
	// can never read an empty or truncated key.
	tmp, err := os.CreateTemp(dir, ".bitnews-key-*")
	if err != nil {
		return Key{}, fmt.Errorf("key file %q does not exist and cannot be created: %w\n%s", path, err, keyFileHelp(path))
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Key{}, fmt.Errorf("writing key file %q: %w", path, err)
	}
	key := GenerateKey()
	_, err = tmp.WriteString(base64.StdEncoding.EncodeToString(key.b[:]) + "\n")
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Key{}, fmt.Errorf("writing key file %q: %w", path, err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Key{}, err
		}
		return Key{}, fmt.Errorf("key file %q does not exist and cannot be created: %w\n%s", path, err, keyFileHelp(path))
	}
	logger.Warn("generated a new master key for secret encryption; back this file up, because without it stored secrets cannot be decrypted",
		"path", path)
	return key, nil
}

func readKeyFile(path string, logger *slog.Logger) (Key, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Key{}, err
		}
		return Key{}, fmt.Errorf("opening key file %q: %w\n%s", path, err, keyFileHelp(path))
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Key{}, fmt.Errorf("reading key file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Key{}, fmt.Errorf("key file %q is not a regular file", path)
	}
	content, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return Key{}, fmt.Errorf("reading key file %q: %w", path, err)
	}
	if len(content) > maxKeyFileSize {
		return Key{}, fmt.Errorf("key file %q is too large to be a key", path)
	}
	key, err := ParseKey(strings.TrimSpace(string(content)))
	if err != nil {
		return Key{}, fmt.Errorf("key file %q is invalid: %w\n%s", path, err, keyFileHelp(path))
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		logger.Warn("key file is accessible by other users; consider chmod 600",
			"path", path, "mode", fmt.Sprintf("%#o", perm))
	}
	return key, nil
}

// maxKeyFileSize bounds how much of a key file is read; a real one is 45 bytes.
const maxKeyFileSize = 1024

func keyFileHelp(path string) string {
	return fmt.Sprintf("Create the key file yourself, for example:\n  install -d -m 700 %q && head -c 32 /dev/urandom | base64 > %q && chmod 600 %q\n"+
		"or mount it as a secret, or point --key-file / BITNEWS_KEY_FILE at a writable location.\n"+
		"Do not replace the key of an existing installation: stored secrets could no longer be decrypted.",
		filepath.Dir(path), path, path)
}
