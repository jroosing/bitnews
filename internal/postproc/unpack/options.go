package unpack

import (
	"io"
	"log/slog"

	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// Default limits.
const (
	// DefaultMaxEntries caps the number of entries in one archive.
	DefaultMaxEntries = 1_000_000
	// DefaultMaxDictionary caps the decoder dictionary of RAR (LZ) and 7z
	// (LZMA, LZMA2, PPMd), which is allocated from a number the archive
	// controls.
	DefaultMaxDictionary = 1 << 30
	// DefaultMaxHeaderMemory caps the memory the 7z reader may allocate for the
	// archive header, estimated from the counts the header declares.
	DefaultMaxHeaderMemory = 256 << 20

	// maxHeaderBytes caps a 7z header, as stored and as decoded.
	maxHeaderBytes = 64 << 20

	// bombMinBytes and bombRatio define a compression bomb: more than
	// bombMinBytes of output at more than bombRatio times the archive size.
	bombMinBytes = 1 << 30
	bombRatio    = 100

	copyChunk = 1 << 20
)

// config holds the settings behind the Options.
type config struct {
	password   string
	minFree    uint64
	maxBytes   int64 // 0: unlimited
	maxEntries int
	maxDict    int64
	maxHeadMem int64 // estimated memory of the 7z header
	maxHeader  int64 // bytes of a 7z header, stored and decoded
	replace    bool
	freeSpace  func(path string) (uint64, error)
	logger     *slog.Logger

	// Bomb thresholds; tests lower them to avoid writing a gigabyte.
	bombMin   int64
	bombRatio int64
}

func newConfig(opts []Option) *config {
	c := &config{
		maxEntries: DefaultMaxEntries,
		maxDict:    DefaultMaxDictionary,
		maxHeadMem: DefaultMaxHeaderMemory,
		maxHeader:  maxHeaderBytes,
		freeSpace:  fsutil.FreeBytes,
		logger:     slog.Default(),
		bombMin:    bombMinBytes,
		bombRatio:  bombRatio,
	}
	for _, o := range opts {
		o(c)
	}
	if c.logger == nil {
		c.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return c
}

// Option changes how Unpack behaves.
type Option func(*config)

// WithPassword sets the archive password. It accepts core.Secret as well as a
// plain string, so a Secret never has to be converted by the caller.
func WithPassword[T ~string](password T) Option {
	return func(c *config) { c.password = string(password) }
}

// WithMinFree keeps n bytes of the disk free: the declared output size must fit
// into the free space minus n.
func WithMinFree(n uint64) Option { return func(c *config) { c.minFree = n } }

// WithMaxBytes caps the total bytes written, nested archives included. Zero
// means no cap.
func WithMaxBytes(n int64) Option { return func(c *config) { c.maxBytes = n } }

// WithMaxEntries caps the number of archive entries. The default is
// DefaultMaxEntries.
func WithMaxEntries(n int) Option { return func(c *config) { c.maxEntries = n } }

// WithMaxDictionary caps the decoder dictionary size in bytes: the RAR
// dictionary, and the 7z LZMA/LZMA2 dictionary (as far as the stream needs it)
// and PPMd memory. The default is DefaultMaxDictionary.
func WithMaxDictionary(n int64) Option { return func(c *config) { c.maxDict = n } }

// WithMaxHeaderMemory caps the memory a 7z archive header may make the reader
// allocate, in bytes. The memory is estimated from the file, folder and stream
// counts the header declares, before the 7z library sees the archive; a header
// over the cap fails with ErrTooLarge. The default is DefaultMaxHeaderMemory,
// which allows about 500,000 files per archive.
func WithMaxHeaderMemory(n int64) Option { return func(c *config) { c.maxHeadMem = n } }

// WithReplace lets Unpack replace an existing destination instead of failing
// with ErrExists.
func WithReplace() Option { return func(c *config) { c.replace = true } }

// WithLogger sets the logger for skipped entries and notices. A nil logger
// discards them.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithFreeSpace replaces the free disk space probe, which defaults to fsutil.FreeBytes. A probe that fails disables the disk check with a warning.
func WithFreeSpace(fn func(path string) (uint64, error)) Option {
	return func(c *config) { c.freeSpace = fn }
}
