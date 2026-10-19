package torrent_test

import (
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

// FuzzTorrentPaths feeds arbitrary bytes as a .torrent file: parsing may fail,
// but whatever it accepts must yield relative, contained, unique paths.
func FuzzTorrentPaths(f *testing.F) {
	f.Add([]byte("d4:infod4:name1:a12:piece lengthi16384e6:lengthi0e6:pieces0:ee"))
	f.Add([]byte("d4:infod5:filesld6:lengthi0e4:pathl2:..1:xeee4:name4:root12:piece lengthi16384e6:pieces0:ee"))
	f.Add([]byte("d4:infod5:filesld6:lengthi1e4:pathl3:a/bee" + "d6:lengthi1e4:pathl3:a:bee" + "e4:name1:r12:piece lengthi16384e6:pieces20:01234567890123456789ee"))
	f.Add([]byte("not bencode"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := filepath.Join(t.TempDir(), "storage")
		paths, err := torrent.TorrentPaths(dir, data)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, p := range paths {
			if p == "" || filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\\") {
				t.Fatalf("unsafe path %q", p)
			}
			for c := range strings.SplitSeq(p, "/") {
				if c == "" || c == "." || c == ".." {
					t.Fatalf("bad component in %q", p)
				}
			}
			if seen[strings.ToLower(p)] {
				t.Fatalf("duplicate path %q", p)
			}
			seen[strings.ToLower(p)] = true
			if _, err := fsutil.SafeJoin(dir, p); err != nil {
				t.Fatalf("SafeJoin rejects accepted path %q: %v", p, err)
			}
		}
	})
}

// FuzzParseSpec feeds arbitrary magnet strings and metainfo to the parsing
// done by Add; it must never panic.
func FuzzParseSpec(f *testing.F) {
	f.Add("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x&xs=http://example.com/a.torrent&ws=http://a/b")
	f.Add("magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32))
	f.Add("magnet:?xt=urn:btih:")
	e, err := torrent.New(torrent.Config{BindAddr: "127.0.0.1", PieceStore: newMemStore()})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = e.Close() })
	f.Fuzz(func(t *testing.T, magnet string) {
		_ = e.ParseSpec(core.TorrentSpec{Magnet: magnet, StorageDir: "/data"})
		_ = e.ParseSpec(core.TorrentSpec{Metainfo: []byte(magnet), StorageDir: "/data"})
	})
}
