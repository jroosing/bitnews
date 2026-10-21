package torrent_test

import (
	"path/filepath"
	"strings"
	"testing"

	anacrolix "github.com/anacrolix/torrent"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

// FuzzParseMetainfo feeds arbitrary bytes as a .torrent file: parsing may
// fail, but whatever it accepts must be within the caps, with safe unique
// paths that agree with the Engine's own check.
func FuzzParseMetainfo(f *testing.F) {
	f.Add([]byte("d4:infod4:name1:a12:piece lengthi16384e6:lengthi0e6:pieces0:ee"))
	f.Add([]byte("d4:infod5:filesld6:lengthi0e4:pathl2:..1:xeee4:name4:root12:piece lengthi16384e6:pieces0:ee"))
	f.Add([]byte("d8:announce3:foo13:announce-listll3:fooee4:infod4:name1:a12:piece lengthi16384e6:lengthi1e6:pieces20:01234567890123456789ee"))
	f.Add([]byte("d4:infod9:file treed1:ad0:d6:lengthi1e11:pieces root32:" + strings.Repeat("r", 32) + "eee12:meta versioni2e4:name1:n12:piece lengthi16384eee"))
	f.Add([]byte("not bencode"))
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := torrent.ParseMetainfo(data)
		if err != nil {
			return
		}
		if info.Size < 0 || len(info.Files) > core.MaxTorrentFiles || len(info.Trackers) > torrent.MaxTrackers {
			t.Fatalf("out of caps: size %d, %d files, %d tiers", info.Size, len(info.Files), len(info.Trackers))
		}
		if info.InfoHash.IsZero() {
			t.Fatal("accepted a torrent without an info hash")
		}
		dir := filepath.Join(t.TempDir(), "storage")
		paths, err := torrent.TorrentPaths(dir, data)
		if err != nil || len(paths) != len(info.Files) {
			t.Fatalf("engine check disagrees: %v (%d paths, %d files)", err, len(paths), len(info.Files))
		}
		var total int64
		seen := map[string]bool{}
		for i, file := range info.Files {
			p := file.Path
			if p != paths[i] || p == "" || filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\\") || file.Size < 0 {
				t.Fatalf("unsafe or diverging file %+v (engine: %q)", file, paths[i])
			}
			if seen[strings.ToLower(p)] {
				t.Fatalf("duplicate path %q", p)
			}
			seen[strings.ToLower(p)] = true
			if _, err := fsutil.SafeJoin(dir, p); err != nil {
				t.Fatalf("SafeJoin rejects accepted path %q: %v", p, err)
			}
			total += file.Size
		}
		if total != info.Size {
			t.Fatalf("size %d != sum of files %d", info.Size, total)
		}
	})
}

// FuzzParseMagnet feeds arbitrary strings as a magnet link: it must never
// panic, and a link it accepts must give the same info hash as anacrolix.
func FuzzParseMagnet(f *testing.F) {
	f.Add("magnet:?xt=urn:btih:" + hexHash + "&dn=x&tr=udp://t.example:80&xs=http://example.com/a.torrent")
	f.Add("magnet:?xt=urn:btih:" + b32Hash)
	f.Add("magnet:?xt=urn:btih:" + hexHash + "&xt=urn:btmh:1220" + strings.Repeat("ab", 32))
	f.Add("magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32))
	f.Add("magnet:?xt=urn:btih:")
	f.Add("magnet:?xt=urn:btih:" + b32Hash[:30] + "\n\n")
	f.Fuzz(func(t *testing.T, uri string) {
		m, err := torrent.ParseMagnet(uri)
		if err != nil {
			return
		}
		if len(m.Trackers) > torrent.MaxTrackers || len(m.DisplayName) > 1024 || len(uri) > torrent.MaxMagnetLength {
			t.Fatalf("out of caps: %d trackers, %d byte name", len(m.Trackers), len(m.DisplayName))
		}
		spec, err := anacrolix.TorrentSpecFromMagnetUri(uri)
		if err != nil {
			t.Fatalf("anacrolix refuses a link ParseMagnet accepted: %v", err)
		}
		if core.InfoHash(spec.InfoHash) != m.InfoHash {
			t.Fatalf("hash %s != anacrolix %s", m.InfoHash, spec.InfoHash)
		}
	})
}
