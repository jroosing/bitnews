package core

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TorrentEngine is the port to a BitTorrent client. The adapter in
// internal/torrent implements it over anacrolix/torrent; internal/core/coretest
// has a fake. Domain types never carry adapter types.
//
// Methods that take an InfoHash return an error wrapping ErrNotFound when the
// engine does not know the torrent. All methods are safe for concurrent use.
type TorrentEngine interface {
	// Add starts (or, with spec.Paused, registers) a torrent from a metainfo
	// or a magnet link and returns its InfoHash. Adding an InfoHash that is
	// already present returns that InfoHash and an error wrapping ErrConflict.
	Add(ctx context.Context, spec TorrentSpec) (InfoHash, error)
	// Remove forgets the torrent. Its files stay on disk.
	Remove(ctx context.Context, h InfoHash) error
	Pause(h InfoHash) error
	Resume(h InfoHash) error
	// Relocate moves the torrent's data to newDir without rehashing and keeps
	// seeding. On failure the torrent stays at its old location.
	Relocate(ctx context.Context, h InfoHash, newDir string) error
	// SetRateLimits sets the global download and upload limits in bytes per
	// second. A value of zero or less means unlimited.
	SetRateLimits(down, up int64) error
	Stats(h InfoHash) (TorrentStats, error)
	// Metainfo returns the bytes of the torrent's .torrent file. For a
	// magnet link it fails until the metadata has arrived (the
	// TorrentMetadataReceived event); the bytes then let the caller store a
	// .torrent file and add the torrent from it after a restart.
	Metainfo(h InfoHash) ([]byte, error)
	// Events returns the engine's event channel. It is buffered; the engine
	// drops events rather than block when the consumer falls behind.
	Events() <-chan TorrentEvent
}

// Limits on hostile torrent input.
const (
	// MaxMetainfoSize is the largest .torrent file the engine accepts.
	MaxMetainfoSize = 10 << 20
	// MaxTorrentFiles is the most files a torrent may contain.
	MaxTorrentFiles = 100_000
)

// ErrInvalidInfoHash is returned for text that is not a hex info hash.
var ErrInvalidInfoHash = errors.New("info hash must be 40 hex characters")

// ErrInvalidTorrentSpec is returned for a TorrentSpec that cannot be added.
var ErrInvalidTorrentSpec = errors.New("invalid torrent spec")

// InfoHash identifies a torrent. It holds the 20-byte v1 info hash; for a
// v2-only torrent it holds the first 20 bytes of the SHA-256 info hash, which
// is what BEP 52 and qBittorrent use as the short identity.
type InfoHash [20]byte

// ParseInfoHash parses a 40-character hex info hash, in either case. A
// 64-character hex string (a v2 info hash) is truncated to its first 20 bytes.
func ParseInfoHash(s string) (InfoHash, error) {
	var h InfoHash
	if len(s) != 2*len(h) && len(s) != 64 {
		return h, fmt.Errorf("%w: got %d characters", ErrInvalidInfoHash, len(s))
	}
	raw, err := hex.DecodeString(s) // accepts both cases
	if err != nil {
		return h, ErrInvalidInfoHash
	}
	copy(h[:], raw)
	return h, nil
}

// String returns the lowercase hex form.
func (h InfoHash) String() string { return hex.EncodeToString(h[:]) }

// IsZero reports whether h is the zero value.
func (h InfoHash) IsZero() bool { return h == InfoHash{} }

// TorrentSpec says what to add. Exactly one of Metainfo and Magnet is set.
type TorrentSpec struct {
	Metainfo   []byte // the bytes of a .torrent file, at most MaxMetainfoSize
	Magnet     string // a magnet: URI
	StorageDir string // absolute directory the torrent's files are stored under
	Paused     bool   // add without starting
}

// Validate checks the spec's shape. It does not parse the metainfo.
func (s TorrentSpec) Validate() error {
	switch {
	case len(s.Metainfo) == 0 && s.Magnet == "":
		return fmt.Errorf("%w: metainfo or magnet required", ErrInvalidTorrentSpec)
	case len(s.Metainfo) != 0 && s.Magnet != "":
		return fmt.Errorf("%w: metainfo and magnet are exclusive", ErrInvalidTorrentSpec)
	case len(s.Metainfo) > MaxMetainfoSize:
		return fmt.Errorf("%w: metainfo larger than %d bytes", ErrInvalidTorrentSpec, MaxMetainfoSize)
	case s.Magnet != "" && !strings.HasPrefix(strings.ToLower(s.Magnet), "magnet:?"):
		return fmt.Errorf("%w: not a magnet URI", ErrInvalidTorrentSpec)
	case s.StorageDir == "":
		return fmt.Errorf("%w: storage dir required", ErrInvalidTorrentSpec)
	}
	return nil
}

// ShareLimit is the ratio or seeding time after which Seeding stops. A
// negative value means unlimited; the zero ShareLimit therefore means
// "stop at once", so use UnlimitedShareLimit for no limit.
type ShareLimit struct {
	Ratio       float64       // uploaded bytes divided by the torrent size
	SeedingTime time.Duration // time spent seeding
}

// UnlimitedShareLimit returns a ShareLimit that is never reached.
func UnlimitedShareLimit() ShareLimit { return ShareLimit{Ratio: -1, SeedingTime: -1} }

// Reached reports whether the given ratio or seeding time has reached either
// limit.
func (l ShareLimit) Reached(ratio float64, seeding time.Duration) bool {
	return (l.Ratio >= 0 && ratio >= l.Ratio) ||
		(l.SeedingTime >= 0 && seeding >= l.SeedingTime)
}

// TorrentState is the engine's view of one torrent.
type TorrentState string

// The torrent states.
const (
	TorrentMetadataPending TorrentState = "metadata_pending"
	TorrentDownloading     TorrentState = "downloading"
	TorrentSeeding         TorrentState = "seeding"
	TorrentPaused          TorrentState = "paused"
	TorrentError           TorrentState = "error"
)

// TorrentFile is one file of a torrent: a slash-separated path relative to
// the torrent's root and its size.
type TorrentFile struct {
	Path string
	Size int64
}

// TorrentStats is a snapshot of one torrent. Sizes are zero until the
// metadata is known.
type TorrentStats struct {
	Name        string
	State       TorrentState
	Private     bool // BEP 27: no DHT, PEX or LSD
	Size        int64
	DoneBytes   int64
	DownRate    int64 // bytes per second
	UpRate      int64 // bytes per second
	Peers       int
	Seeds       int
	Ratio       float64 // uploaded bytes divided by Size
	SeedingTime time.Duration
	Uploaded    int64
	Files       []TorrentFile
}

// TorrentEventKind says what happened to a torrent.
type TorrentEventKind string

// The torrent event kinds.
const (
	// TorrentMetadataReceived: a magnet's metadata arrived and was accepted.
	TorrentMetadataReceived TorrentEventKind = "metadata_received"
	// TorrentFetched: all pieces are hash-verified.
	TorrentFetched TorrentEventKind = "fetched"
	// TorrentErrored: the torrent failed; Err says why.
	TorrentErrored TorrentEventKind = "error"
)

// TorrentEvent is something the engine reports about one torrent.
type TorrentEvent struct {
	Kind TorrentEventKind
	Hash InfoHash
	Err  error // set for TorrentErrored
}

// ListenPortStatus is what the torrent engine knows about whether peers can
// reach its listen port. Port forwarding (by the router, or a VPN such as
// gluetun) is only needed for incoming connections: downloading works
// without it, but Seeding to peers that cannot accept connections does not.
//
// Nothing can prove from the inside that a port is closed, so the status is
// "open" once a peer has connected in on the current port, and unconfirmed
// until then.
type ListenPortStatus struct {
	// Port is the TCP and uTP port the engine listens on.
	Port int `json:"port"`
	// Since is when the engine started listening on Port.
	Since time.Time `json:"since"`
	// LastIncoming is the last time a peer connected in on Port, zero if
	// none has yet.
	LastIncoming time.Time `json:"last_incoming"`
}

// Open reports whether a peer has connected in on the current port.
func (s ListenPortStatus) Open() bool { return !s.LastIncoming.IsZero() }
