package torrent_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleRoot finds the directory with go.mod above the test's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found")
		dir = parent
	}
}

// internal/torrent is the only package that may import anacrolix/torrent or
// anything else from the anacrolix module family, so that the port in core
// stays free of it and the adapter can be swapped.
func TestOnlyInternalTorrentImportsAnacrolix(t *testing.T) {
	root := moduleRoot(t)
	allowed := filepath.Join(root, "internal", "torrent")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip what is not the module's own source: hidden directories
			// (.git, and in CI the Go module cache .go and build cache
			// .cache, which sit inside the checkout and hold anacrolix's own
			// source) and build output.
			if name := d.Name(); path != root && strings.HasPrefix(name, ".") || name == "node_modules" || name == "bin" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasPrefix(path, allowed+string(filepath.Separator)) {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil // another package's broken file is not this test's business
		}
		checked++
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			assert.False(t, strings.HasPrefix(p, "github.com/anacrolix/"), "%s imports %s; only internal/torrent may", path, p)
		}
		return nil
	})
	require.NoError(t, err)
	require.Positive(t, checked, "found no Go files to check")
}
