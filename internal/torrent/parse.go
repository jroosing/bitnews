package torrent

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
	infohash_v2 "github.com/anacrolix/torrent/types/infohash-v2"

	"github.com/jroosing/bitnews/internal/core"
)

// ErrInvalidMagnet is returned for magnet links that are malformed, too
// large, or that the engine would refuse.
var ErrInvalidMagnet = errors.New("torrent: invalid magnet link")

// Limits on a magnet link. A real one with a dozen trackers is well below
// these.
const (
	// MaxMagnetLength is the longest magnet URI accepted, in bytes.
	MaxMagnetLength = 8 << 10
	// MaxTrackers is the most tracker URLs kept from a magnet link or from the
	// announce list of a .torrent file.
	MaxTrackers = 128
	// MaxTrackerLength is the longest tracker URL kept, in bytes.
	MaxTrackerLength = 2048
	maxNameLength    = 1024
)

// Info is what ParseMetainfo learned about a .torrent file, without starting
// a client.
type Info struct {
	// Name is the torrent's own name (the advisory "name" of the info
	// dictionary, which is hostile input); empty if it has none.
	Name string
	// InfoHash is the v1 info hash.
	InfoHash core.InfoHash
	// InfoHashV2 is the first 20 bytes of the SHA-256 info hash of a hybrid
	// torrent; zero for a v1 torrent. (v2-only torrents are refused.)
	InfoHashV2 core.InfoHash
	// Size is the sum of the file sizes.
	Size int64
	// Files lists the files in torrent order with the safe, slash-separated
	// paths they get on disk below the torrent's storage directory (the same
	// ones Engine.Stats reports).
	Files []core.TorrentFile
	// Private is the BEP 27 flag.
	Private bool
	// Trackers is the announce list in tiers, capped at MaxTrackers URLs.
	Trackers [][]string
}

// parseRoot is the directory the paths of a parsed torrent are checked
// against; nothing is created there.
var parseRoot = filepath.Join(string(filepath.Separator), "bitnews-parse-root")

// ParseMetainfo decodes a .torrent file and applies exactly the checks the
// Engine applies when it adds one (size, file, piece and piece length caps,
// v1 data, safe and unique file paths), plus a consistency check between the
// total size and the number of pieces. It starts nothing and touches no file
// system. Errors wrap ErrInvalidMetainfo.
func ParseMetainfo(data []byte) (Info, error) {
	mi, info, err := parseMetainfo(data)
	if err != nil {
		return Info{}, err
	}
	rels, err := torrentPaths(parseRoot, info)
	if err != nil {
		return Info{}, err
	}
	out := Info{InfoHash: core.InfoHash(mi.HashInfoBytes()), Files: make([]core.TorrentFile, len(rels))}
	if name := info.BestName(); name != metainfo.NoName {
		out.Name = name
	}
	for i, f := range info.UpvertedFiles() {
		out.Files[i] = core.TorrentFile{Path: rels[i], Size: f.Length}
		out.Size += f.Length
	}
	if want := (out.Size + info.PieceLength - 1) / info.PieceLength; int64(len(info.Pieces)/20) != want {
		return Info{}, fmt.Errorf("%w: %d pieces for %d bytes of %d-byte pieces", ErrInvalidMetainfo, len(info.Pieces)/20, out.Size, info.PieceLength)
	}
	if info.HasV2() {
		h := infohash_v2.HashBytes(mi.InfoBytes)
		copy(out.InfoHashV2[:], h[:])
	}
	out.Private = info.Private != nil && *info.Private
	out.Trackers = capTrackers(mi.UpvertedAnnounceList())
	return out, nil
}

// capTrackers keeps at most MaxTrackers URLs of at most MaxTrackerLength
// bytes, dropping empty tiers.
func capTrackers(tiers [][]string) [][]string {
	var out [][]string
	n := 0
	for _, tier := range tiers {
		var kept []string
		for _, u := range tier {
			if u == "" || len(u) > MaxTrackerLength || n >= MaxTrackers {
				continue
			}
			kept = append(kept, u)
			n++
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

// MagnetInfo is what ParseMagnet learned about a magnet link.
type MagnetInfo struct {
	// InfoHash is the v1 info hash.
	InfoHash core.InfoHash
	// InfoHashV2 is the first 20 bytes of the v2 (btmh) info hash of a hybrid
	// link; zero if the link has none.
	InfoHashV2 core.InfoHash
	// DisplayName is the "dn" parameter, at most 1024 bytes; hostile input.
	DisplayName string
	// Trackers are the "tr" parameters, capped like Info.Trackers.
	Trackers []string
}

// ParseMagnet decodes a magnet link the way the Engine does, but strictly: it
// needs a v1 info hash (xt=urn:btih: with 40 hex characters or 32 base32
// ones in upper case) and accepts a v2 one (urn:btmh:) next to it. A link
// with only a v2 hash is refused, as the Engine refuses v2-only torrents.
// The "xs", "as", "ws" and "x.pe" parameters are ignored: the Engine never
// fetches from them. Errors wrap ErrInvalidMagnet.
func ParseMagnet(uri string) (m MagnetInfo, err error) {
	if len(uri) > MaxMagnetLength {
		return m, fmt.Errorf("%w: longer than %d bytes", ErrInvalidMagnet, MaxMagnetLength)
	}
	if !strings.HasPrefix(strings.ToLower(uri), "magnet:?") {
		return m, fmt.Errorf("%w: not a magnet URI", ErrInvalidMagnet)
	}
	// The library's own hash decoder panics when its length assumptions are
	// wrong (and base32 decoding skips line breaks), so check the shape of
	// the hashes first and recover below as a second line of defence.
	if err := checkBTIH(uri); err != nil {
		return m, err
	}
	defer func() {
		if r := recover(); r != nil {
			m, err = MagnetInfo{}, fmt.Errorf("%w: unparseable", ErrInvalidMagnet)
		}
	}()
	pm, err := metainfo.ParseMagnetV2Uri(uri)
	if err != nil {
		return m, fmt.Errorf("%w: %v", ErrInvalidMagnet, err)
	}
	if !pm.InfoHash.Ok {
		return m, fmt.Errorf("%w: no v1 info hash (btih); v2-only is not supported", ErrInvalidMagnet)
	}
	m.InfoHash = core.InfoHash(pm.InfoHash.Value)
	if pm.V2InfoHash.Ok {
		copy(m.InfoHashV2[:], pm.V2InfoHash.Value[:])
	}
	m.DisplayName = strings.ToValidUTF8(pm.DisplayName, "")
	if len(m.DisplayName) > maxNameLength {
		m.DisplayName = strings.ToValidUTF8(m.DisplayName[:maxNameLength], "")
	}
	if tiers := capTrackers([][]string{pm.Trackers}); len(tiers) > 0 {
		m.Trackers = tiers[0]
	}
	return m, nil
}

// checkBTIH refuses a btih value that is not exactly 40 hex or 32 base32
// characters.
func checkBTIH(uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMagnet, err)
	}
	for _, xt := range u.Query()["xt"] {
		v, ok := strings.CutPrefix(xt, "urn:btih:")
		if !ok {
			continue
		}
		switch len(v) {
		case 40:
			for _, c := range []byte(v) {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
					return fmt.Errorf("%w: btih is not hex", ErrInvalidMagnet)
				}
			}
		case 32:
			for _, c := range []byte(v) {
				if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
					return fmt.Errorf("%w: btih is not base32", ErrInvalidMagnet)
				}
			}
		default:
			return fmt.Errorf("%w: btih has %d characters, want 40 hex or 32 base32", ErrInvalidMagnet, len(v))
		}
	}
	return nil
}
