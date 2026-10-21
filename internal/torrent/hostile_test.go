package torrent_test

import (
	"context"
	"crypto/sha1"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

type hostileFile struct {
	path []string
	data []byte
}

// hostileTorrent builds a valid torrent (correct piece hashes) whose names
// are whatever the test wants.
func hostileTorrent(t *testing.T, name string, files []hostileFile) []byte {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: pieceLength}
	var all []byte
	for _, f := range files {
		info.Files = append(info.Files, metainfo.FileInfo{Length: int64(len(f.data)), Path: f.path})
		all = append(all, f.data...)
	}
	for off := 0; off < len(all); off += pieceLength {
		sum := sha1.Sum(all[off:min(off+pieceLength, len(all))])
		info.Pieces = append(info.Pieces, sum[:]...)
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	out, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: infoBytes})
	require.NoError(t, err)
	return out
}

// listAll returns every path below root, relative and slash-separated.
func listAll(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	}))
	return out
}

func TestHostileNamesNeverEscapeTheStorageDir(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		name  string
		paths [][]string
	}{
		"slashes inside a component": {"root", [][]string{{"a/../../escape"}}},
		"absolute component":         {"root", [][]string{{"/etc/passwd"}}},
		"backslashes":                {"root", [][]string{{`..\..\escape`}}},
		"windows drive":              {"root", [][]string{{`C:\Windows\x`}}},
		"nul byte":                   {"root", [][]string{{"a\x00b"}}},
		"windows reserved":           {"root", [][]string{{"CON"}, {"nul.txt"}, {"COM1"}}},
		"control characters":         {"root", [][]string{{"a\nb\x1b[0m"}}},
		"hostile torrent name":       {"../../outside", [][]string{{"f"}}},
		"absolute torrent name":      {"/tmp/outside", [][]string{{"f"}}},
		"very long name":             {"root", [][]string{{strings.Repeat("x", 600)}}},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "storage")
			var files []hostileFile
			for _, p := range tc.paths {
				files = append(files, hostileFile{path: p}) // zero-length: created at once
			}
			data := hostileTorrent(t, tc.name, files)
			e := newEngine(t)

			h, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: data, StorageDir: dir})

			if err != nil {
				// Refusing is fine, as long as nothing was written anywhere.
				assert.ErrorIs(t, err, torrent.ErrInvalidMetainfo)
				assert.Empty(t, listAll(t, parent), "a refused torrent must leave nothing behind")
				return
			}
			st, err := e.Stats(h)
			require.NoError(t, err)
			for _, f := range st.Files {
				assert.NotContains(t, strings.Split(f.Path, "/"), "..", "reported path %q", f.Path)
				assert.False(t, filepath.IsAbs(f.Path), "reported path %q", f.Path)
				assert.NotContains(t, f.Path, "\x00")
			}
			for _, p := range listAll(t, parent) {
				assert.True(t, p == "storage" || strings.HasPrefix(p, "storage/"), "%q is outside the storage dir", p)
			}
		})
	}
}

func TestHostileTorrentsAreRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		name  string
		files []hostileFile
	}{
		"dot dot component":     {"root", []hostileFile{{path: []string{"..", "x"}}}},
		"dot component":         {"root", []hostileFile{{path: []string{".", "x"}}}},
		"empty component":       {"root", []hostileFile{{path: []string{"a", "", "x"}}}},
		"dot dot torrent name":  {"..", []hostileFile{{path: []string{"x"}}}},
		"names that collide":    {"root", []hostileFile{{path: []string{"a:b"}}, {path: []string{"a_b"}}}},
		"case-only collision":   {"root", []hostileFile{{path: []string{"Movie.mkv"}}, {path: []string{"movie.MKV"}}}},
		"file also a directory": {"root", []hostileFile{{path: []string{"a"}}, {path: []string{"a", "b"}}}},
		"directory then file":   {"root", []hostileFile{{path: []string{"a", "b"}}, {path: []string{"a"}}}},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "storage")
			data := hostileTorrent(t, tc.name, tc.files)

			_, err := newEngine(t).Add(context.Background(), core.TorrentSpec{Metainfo: data, StorageDir: dir})

			assert.ErrorIs(t, err, torrent.ErrInvalidMetainfo)
			assert.Empty(t, listAll(t, parent))
		})
	}
}

func TestTorrentWithTooManyFilesIsRefused(t *testing.T) {
	t.Parallel()
	files := make([]hostileFile, core.MaxTorrentFiles+1)
	for i := range files {
		files[i] = hostileFile{path: []string{fmt.Sprint(i)}}
	}
	data := hostileTorrent(t, "many", files)
	require.Less(t, len(data), core.MaxMetainfoSize)

	_, err := newEngine(t).Add(context.Background(), core.TorrentSpec{Metainfo: data, StorageDir: filepath.Join(t.TempDir(), "s")})

	assert.ErrorIs(t, err, torrent.ErrInvalidMetainfo)
}

func TestTorrentPathsSanitizesEachComponent(t *testing.T) {
	data := hostileTorrent(t, "Release", []hostileFile{
		{path: []string{"dir", "CON.txt"}, data: []byte("x")},
		{path: []string{"a:b?"}, data: []byte("y")},
	})

	paths, err := torrent.TorrentPaths(t.TempDir(), data)

	require.NoError(t, err)
	assert.Equal(t, []string{"Release/dir/_CON.txt", "Release/a_b_"}, paths)
}

func TestHostileNamesDownloadIntoTheSanitizedPathsOnly(t *testing.T) {
	t.Parallel()
	body := []byte(strings.Repeat("payload-", 4000))
	data := hostileTorrent(t, "rel", []hostileFile{{path: []string{`..\..\evil`}, data: body}, {path: []string{"/etc/cron.d/x"}, data: body}})
	seedParent := t.TempDir()
	seedDir := filepath.Join(seedParent, "seed")
	paths, err := torrent.TorrentPaths(seedDir, data)
	require.NoError(t, err)
	for _, p := range paths { // lay the data down where a safe engine will look
		full := filepath.Join(seedDir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, body, 0o644))
	}
	mi, err := metainfo.Load(strings.NewReader(string(data)))
	require.NoError(t, err)
	h := core.InfoHash(mi.HashInfoBytes())
	seeder := newEngine(t)
	_, err = seeder.Add(context.Background(), core.TorrentSpec{Metainfo: data, StorageDir: seedDir})
	require.NoError(t, err)
	waitEvent(t, seeder, core.TorrentFetched, h)

	parent := t.TempDir()
	dir := filepath.Join(parent, "storage")
	leech := newEngine(t)
	_, err = leech.Add(context.Background(), core.TorrentSpec{Metainfo: data, StorageDir: dir})
	require.NoError(t, err)
	require.NoError(t, leech.AddPeer(h, seeder.ListenAddr()))
	waitEvent(t, leech, core.TorrentFetched, h)

	for _, p := range listAll(t, parent) {
		assert.True(t, p == "storage" || strings.HasPrefix(p, "storage/"), "%q is outside the storage dir", p)
	}
	for _, p := range paths {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		require.NoError(t, err)
		assert.Equal(t, body, got)
	}
}

func TestInvalidMetainfoMatchesTheCoreInvalidSpecError(t *testing.T) {
	t.Parallel()
	e := newEngine(t)

	_, err := e.Add(context.Background(), core.TorrentSpec{Metainfo: []byte("not bencode"), StorageDir: t.TempDir()})

	require.Error(t, err)
	assert.ErrorIs(t, err, torrent.ErrInvalidMetainfo)
	assert.ErrorIs(t, err, core.ErrInvalidTorrentSpec, "callers outside this package classify it without importing the adapter")
}
