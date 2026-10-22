package torrent

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent/metainfo"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/store"
)

// ErrInvalidMetainfo is returned for torrents that are malformed or exceed
// the engine's limits (file count, piece count, piece length).
var ErrInvalidMetainfo = errors.New("torrent: invalid metainfo")

// maxPieceLength bounds a torrent's piece length. Real torrents stay well
// below 64 MiB.
const maxPieceLength = 256 << 20

// parseMetainfo decodes a .torrent file and checks it against the engine's
// limits. It does not look at file names; see torrentPaths.
func parseMetainfo(data []byte) (*metainfo.MetaInfo, *metainfo.Info, error) {
	if len(data) > core.MaxMetainfoSize {
		return nil, nil, fmt.Errorf("%w: larger than %d bytes", ErrInvalidMetainfo, core.MaxMetainfoSize)
	}
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidMetainfo, err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: info: %v", ErrInvalidMetainfo, err)
	}
	if err := checkInfo(&info); err != nil {
		return nil, nil, err
	}
	return mi, &info, nil
}

// checkInfo applies the engine's limits to an info dictionary, whether it
// came from a .torrent file or from a magnet link's metadata.
func checkInfo(info *metainfo.Info) error {
	if !info.HasV1() {
		return fmt.Errorf("%w: v2-only torrents are not supported", ErrInvalidMetainfo)
	}
	if info.PieceLength <= 0 || info.PieceLength > maxPieceLength {
		return fmt.Errorf("%w: piece length %d", ErrInvalidMetainfo, info.PieceLength)
	}
	if len(info.Pieces)%20 != 0 {
		return fmt.Errorf("%w: pieces has invalid length", ErrInvalidMetainfo)
	}
	if n := len(info.Pieces) / 20; n > store.MaxTorrentPieces {
		return fmt.Errorf("%w: %d pieces, at most %d", ErrInvalidMetainfo, n, store.MaxTorrentPieces)
	}
	if len(info.Files) > core.MaxTorrentFiles {
		return fmt.Errorf("%w: %d files, at most %d", ErrInvalidMetainfo, len(info.Files), core.MaxTorrentFiles)
	}
	var total int64
	for f := range info.UpvertedV1Files() {
		if f.Length < 0 || total > (1<<62)-f.Length {
			return fmt.Errorf("%w: file length out of range", ErrInvalidMetainfo)
		}
		total += f.Length
	}
	return nil
}

// relPath is the on-disk path of one torrent file relative to the torrent's
// storage directory: the torrent name and the file's path, each component
// made safe with fsutil.SafeName.
func relPath(info *metainfo.Info, f *metainfo.FileInfo) (string, error) {
	var parts []string
	if name := info.BestName(); name != metainfo.NoName {
		parts = append(parts, name)
	}
	parts = append(parts, f.BestPath()...)
	if len(parts) == 0 {
		return "", fmt.Errorf("%w: file without a name", ErrInvalidMetainfo)
	}
	safe := make([]string, len(parts))
	for i, p := range parts {
		s, err := fsutil.SafeName(p)
		if err != nil {
			return "", fmt.Errorf("%w: file name component %q: %v", ErrInvalidMetainfo, truncate(p, 64), err)
		}
		safe[i] = s
	}
	return filepath.Join(safe...), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "?") + "..."
}

// torrentPaths returns the path of every file of info (in torrent order),
// relative to dir and slash-separated. It fails if any file name cannot be
// made safe, if two files would share a path (including paths that differ
// only in case, which collide on case-insensitive file systems) or if a file
// would also have to be a directory. Every path is also checked with
// fsutil.SafeJoin against dir.
func torrentPaths(dir string, info *metainfo.Info) ([]string, error) {
	if err := checkInfo(info); err != nil {
		return nil, err
	}
	files := info.UpvertedFiles()
	out := make([]string, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	dirs := make(map[string]struct{})
	for i := range files {
		rel, err := relPath(info, &files[i])
		if err != nil {
			return nil, err
		}
		if _, err := fsutil.SafeJoin(dir, rel); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidMetainfo, err)
		}
		slash := filepath.ToSlash(rel)
		key := strings.ToLower(slash)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("%w: two files map to %q", ErrInvalidMetainfo, slash)
		}
		if _, isDir := dirs[key]; isDir {
			return nil, fmt.Errorf("%w: %q is both a file and a directory", ErrInvalidMetainfo, slash)
		}
		seen[key] = struct{}{}
		for d := key; ; {
			i := strings.LastIndexByte(d, '/')
			if i < 0 {
				break
			}
			d = d[:i]
			if _, isFile := seen[d]; isFile {
				return nil, fmt.Errorf("%w: %q is both a file and a directory", ErrInvalidMetainfo, d)
			}
			dirs[d] = struct{}{}
		}
		out = append(out, slash)
	}
	return out, nil
}
