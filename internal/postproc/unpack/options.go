package unpack

import (
	"io"
	"log/slog"
)

// Default limits.
const (
	// DefaultMaxEntries caps the number of entries in one archive.
	DefaultMaxEntries = 1_000_000
	// DefaultMaxDictionary caps the RAR decoder dictionary, which is allocated
	// from a number the archive controls.
	DefaultMaxDictionary = 1 << 30

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
		freeSpace:  freeSpace,
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

// WithMaxDictionary caps the RAR decoder dictionary size in bytes. The default
// is DefaultMaxDictionary.
func WithMaxDictionary(n int64) Option { return func(c *config) { c.maxDict = n } }

// WithReplace lets Unpack replace an existing destination instead of failing
// with ErrExists.
func WithReplace() Option { return func(c *config) { c.replace = true } }

// WithLogger sets the logger for skipped entries and notices. A nil logger
// discards them.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithFreeSpace replaces the free disk space probe, which defaults to statfs on
// Unix. A probe that fails disables the disk check with a warning.
func WithFreeSpace(fn func(path string) (uint64, error)) Option {
	return func(c *config) { c.freeSpace = fn }
}
