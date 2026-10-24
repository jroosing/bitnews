package finalize_test

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/fsutil"
	"github.com/jroosing/bitnews/internal/postproc/finalize"
)

// env is a temp tree with incomplete and complete roots for both protocols.
type env struct {
	t        *testing.T
	base     string
	settings core.Settings
	cats     map[core.ID]core.Category

	mu       sync.Mutex
	warnings []finalize.Warning
	mover    func(src, dst string) (fsutil.MoveResult, error)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	e := &env{t: t, base: base, cats: map[core.ID]core.Category{}}
	e.settings = core.DefaultSettings()
	e.settings.Paths = core.Paths{
		Usenet:  core.Roots{Incomplete: filepath.Join(base, "usenet", "incomplete"), Complete: filepath.Join(base, "usenet", "complete")},
		Torrent: core.Roots{Incomplete: filepath.Join(base, "torrents", "incomplete"), Complete: filepath.Join(base, "torrents", "complete")},
	}
	require.NoError(t, os.MkdirAll(e.settings.Paths.Usenet.Incomplete, 0o755))
	require.NoError(t, os.MkdirAll(e.settings.Paths.Torrent.Incomplete, 0o755))
	return e
}

func (e *env) finisher(opts ...finalize.Option) *finalize.Finisher {
	deps := finalize.Deps{
		Settings: func(context.Context) (core.Settings, error) { return e.settings, nil },
		Categories: func(_ context.Context, id core.ID) (core.Category, error) {
			c, ok := e.cats[id]
			if !ok {
				return core.Category{}, core.ErrNotFound
			}
			return c, nil
		},
		Warn: func(_ context.Context, w finalize.Warning) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.warnings = append(e.warnings, w)
		},
	}
	if e.mover != nil {
		deps.Mover = e.mover
	}
	return finalize.New(deps, opts...)
}

func (e *env) warns() []finalize.Warning {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]finalize.Warning(nil), e.warnings...)
}

func (e *env) addCategory(name, override string) core.ID {
	id := core.NewID()
	e.cats[id] = core.Category{ID: id, Name: name, DirOverride: override}
	return id
}

func usenetDownload(name string) core.Download {
	return core.Download{ID: core.NewID(), Name: name, SourceKind: core.SourceNZB}
}

func torrentDownload(name string) core.Download {
	return core.Download{ID: core.NewID(), Name: name, SourceKind: core.SourceTorrent}
}

// writeFiles creates files (name -> content) under dir, making parents.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
}

// listing returns every path under root (slash-separated, relative) mapped
// to its content, directories as "<dir>". Symlinks are not followed.
func listing(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "<symlink:" + target + ">"
		case d.IsDir():
			out[rel] = "<dir>"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = string(b)
		}
		return nil
	}))
	return out
}

// hashTree returns a digest of names, modes, mtimes and contents.
func hashTree(t *testing.T, root string) [32]byte {
	t.Helper()
	h := sha256.New()
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		h.Write([]byte(rel))
		h.Write([]byte(info.Mode().String()))
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write([]byte(info.ModTime().String()))
			h.Write(b)
		}
		return nil
	}))
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func (e *env) usenetDir(name string) string {
	dir := filepath.Join(e.settings.Paths.Usenet.Incomplete, name)
	require.NoError(e.t, os.MkdirAll(dir, 0o755))
	return dir
}

func (e *env) torrentDir(name string) string {
	dir := filepath.Join(e.settings.Paths.Torrent.Incomplete, name)
	require.NoError(e.t, os.MkdirAll(dir, 0o755))
	return dir
}

func osMkdirAll(p string) error           { return os.MkdirAll(p, 0o755) }
func osSymlink(target, link string) error { return os.Symlink(target, link) }

const MarkerName = finalize.MarkerName

func removeMarker(dir string) error { return os.Remove(filepath.Join(dir, MarkerName)) }
