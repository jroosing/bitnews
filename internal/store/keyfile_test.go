package store_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/store"
)

// testKeyBytes is a recognizable key whose every textual rendering the tests
// look for in output that must never contain it.
var testKeyBytes = bytes.Repeat([]byte{0xAB, 0xCD}, 16)

func keyLeaks(out string) bool {
	return strings.Contains(out, base64.StdEncoding.EncodeToString(testKeyBytes)) ||
		strings.Contains(out, fmt.Sprintf("%x", testKeyBytes)) ||
		strings.Contains(out, fmt.Sprint(testKeyBytes)) ||
		strings.Contains(out, "171 205")
}

func testKey(t *testing.T) store.Key {
	t.Helper()
	k, err := store.ParseKey(base64.StdEncoding.EncodeToString(testKeyBytes))
	require.NoError(t, err)
	return k
}

func TestKeyIsRedactedByFmtJSONAndSlog(t *testing.T) {
	k := testKey(t)
	type holder struct{ Key store.Key }

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%b"} {
		for _, arg := range []any{k, &k, holder{k}, &holder{k}} {
			out := fmt.Sprintf(verb, arg)
			assert.False(t, keyLeaks(out), "%s of %T leaked: %s", verb, arg, out)
		}
	}
	assert.Contains(t, fmt.Sprint(k), "[redacted]")

	js, err := json.Marshal(holder{k})
	require.NoError(t, err)
	assert.False(t, keyLeaks(string(js)), string(js))
	assert.JSONEq(t, `{"Key":"[redacted]"}`, string(js))

	for _, h := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(h(&buf)).Info("loaded", "key", k, "ptr", &k)
		assert.False(t, keyLeaks(buf.String()), buf.String())
		assert.Contains(t, buf.String(), "[redacted]")
	}
}

func keyLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestLoadOrCreateKeyGeneratesAMissingKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "bitnews.key")
	logger, logs := keyLogger()

	key, err := store.LoadOrCreateKey(path, logger)
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, content, 45, "44 base64 characters and a newline")
	assert.Equal(t, byte('\n'), content[44])
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content)))
	require.NoError(t, err)
	assert.Len(t, raw, 32)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		dir, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	}

	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), path)
	assert.NotContains(t, logs.String(), strings.TrimSpace(string(content)))

	sealed, err := store.Seal(key, "x", "a/b/c")
	require.NoError(t, err)
	reloaded, err := store.ParseKey(strings.TrimSpace(string(content)))
	require.NoError(t, err)
	opened, err := store.Unseal(reloaded, sealed, "a/b/c")
	require.NoError(t, err)
	assert.Equal(t, "x", opened)
}

func writeKeyFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bitnews.key")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func sealedWith(t *testing.T, key store.Key) string {
	t.Helper()
	sealed, err := store.Seal(key, "probe", "a/b/c")
	require.NoError(t, err)
	return sealed
}

func TestLoadOrCreateKeyReturnsTheSameKeyOnTheNextStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bitnews.key")
	first, err := store.LoadOrCreateKey(path, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	logger, logs := keyLogger()
	second, err := store.LoadOrCreateKey(path, logger)
	require.NoError(t, err)

	opened, err := store.Unseal(second, sealedWith(t, first), "a/b/c")
	require.NoError(t, err)
	assert.Equal(t, "probe", opened)
	assert.Empty(t, logs.String(), "an existing 0600 key file is loaded silently")
}

func TestLoadOrCreateKeyDoesNotOverwriteAnExistingFile(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testKeyBytes)
	path := writeKeyFile(t, encoded+"\n", 0o600)

	key, err := store.LoadOrCreateKey(path, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Equal(t, testKey(t), key)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, encoded+"\n", string(content))
}

func TestLoadOrCreateKeyToleratesSurroundingWhitespace(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testKeyBytes)
	for name, content := range map[string]string{
		"no newline":     encoded,
		"newline":        encoded + "\n",
		"crlf":           encoded + "\r\n",
		"extra blank":    encoded + "\n\n  \n",
		"leading spaces": "  " + encoded + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			key, err := store.LoadOrCreateKey(writeKeyFile(t, content, 0o600), slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			assert.Equal(t, testKey(t), key)
		})
	}
}

func TestLoadOrCreateKeyRejectsAnInvalidKeyFileWithoutPrintingIt(t *testing.T) {
	for name, content := range map[string]string{
		"empty":            "",
		"too short":        base64.StdEncoding.EncodeToString(testKeyBytes[:31]) + "\n",
		"too long":         base64.StdEncoding.EncodeToString(append(bytes.Clone(testKeyBytes), 0x42)) + "\n",
		"not base64":       "this is not base64 at all!!\n",
		"two keys":         base64.StdEncoding.EncodeToString(testKeyBytes) + "\n" + base64.StdEncoding.EncodeToString(testKeyBytes) + "\n",
		"binary":           string(testKeyBytes),
		"huge":             strings.Repeat("A", 1<<20),
		"hex instead":      fmt.Sprintf("%x\n", testKeyBytes),
		"url-safe variant": base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeKeyFile(t, content, 0o600)
			logger, logs := keyLogger()

			key, err := store.LoadOrCreateKey(path, logger)

			require.Error(t, err)
			assert.Equal(t, store.Key{}, key)
			assert.Contains(t, err.Error(), path)
			for _, out := range []string{err.Error(), logs.String()} {
				assert.False(t, keyLeaks(out))
				if len(content) > 0 && len(content) < 100 {
					assert.NotContains(t, out, strings.TrimSpace(content))
				}
			}
			after, rerr := os.ReadFile(path)
			require.NoError(t, rerr)
			assert.Equal(t, content, string(after), "an invalid key file is never overwritten")
		})
	}
}

func TestLoadOrCreateKeyWarnsButWorksWhenPermissionsAreBroad(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	encoded := base64.StdEncoding.EncodeToString(testKeyBytes)
	for _, mode := range []os.FileMode{0o644, 0o444, 0o640, 0o604} {
		t.Run(mode.String(), func(t *testing.T) {
			path := writeKeyFile(t, encoded+"\n", mode)
			logger, logs := keyLogger()

			key, err := store.LoadOrCreateKey(path, logger)

			require.NoError(t, err)
			assert.Equal(t, testKey(t), key)
			assert.Contains(t, logs.String(), "level=WARN")
			assert.Contains(t, logs.String(), path)
			assert.False(t, keyLeaks(logs.String()))
			assert.NotContains(t, logs.String(), encoded)
		})
	}
}

func TestLoadOrCreateKeyAcceptsOwnerOnlyPermissionsSilently(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(testKeyBytes)
	for _, mode := range []os.FileMode{0o600, 0o400} {
		t.Run(mode.String(), func(t *testing.T) {
			logger, logs := keyLogger()
			_, err := store.LoadOrCreateKey(writeKeyFile(t, encoded+"\n", mode), logger)
			require.NoError(t, err)
			assert.Empty(t, logs.String())
		})
	}
}

func TestLoadOrCreateKeyFailsWithInstructionsWhenTheDirectoryIsNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced on a non-root user")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "bitnews.key")

	key, err := store.LoadOrCreateKey(path, slog.New(slog.DiscardHandler))

	require.Error(t, err)
	assert.Equal(t, store.Key{}, key)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "BITNEWS_KEY_FILE")
	assert.Contains(t, err.Error(), "chmod 600")
	assert.NoFileExists(t, path)
}

func TestLoadOrCreateKeyFailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	blocker := writeKeyFile(t, "i am a file", 0o600)
	path := filepath.Join(blocker, "sub", "bitnews.key")

	_, err := store.LoadOrCreateKey(path, slog.New(slog.DiscardHandler))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "BITNEWS_KEY_FILE")
}

func TestLoadOrCreateKeyRejectsADirectoryAtThePath(t *testing.T) {
	// Docker creates a directory when a bind-mounted secret file is missing.
	_, err := store.LoadOrCreateKey(t.TempDir(), slog.New(slog.DiscardHandler))
	require.Error(t, err)
}

func TestLoadOrCreateKeyIsSafeToCallConcurrentlyOnAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bitnews.key")
	const n = 8
	keys := make([]store.Key, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { keys[i], errs[i] = store.LoadOrCreateKey(path, slog.New(slog.DiscardHandler)) })
	}
	wg.Wait()
	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, keys[0], keys[i], "every caller ends up with the one key that was written")
	}
}
