package unpack

import (
	"io"
	"log/slog"

	"github.com/jroosing/bitnews/internal/fsutil"
)

// Default limits.
const (
	// DefaultMaxEntries caps the number of entries in one archive.
	DefaultMaxEntries = 1_000_000

	// bombMinBytes and bombRatio define a compression bomb: more than
	// bombMinBytes of output at more than bombRatio times the archive size.
	bombMinBytes = 1 << 30
	bombRatio    = 100

	// listBytesPerEntry sizes the cap on the listing output of 7zz: the
	// listing of an archive with maxEntries entries is expected to fit.
	listBytesPerEntry = 600
	listBytesFloor    = 4 << 20
	listBytesCeiling  = 512 << 20
)

// config holds the settings behind the Options.
type config struct {
	password   string
	minFree    uint64
	maxBytes   int64 // 0: unlimited
	maxEntries int
	replace    bool
	freeSpace  func(path string) (uint64, error)
	logger     *slog.Logger
	sevenZip   string // path of 7zz; empty: look it up on PATH

	// Bomb thresholds; tests lower them to avoid writing a gigabyte.
	bombMin   int64
	bombRatio int64
}

func newConfig(opts []Option) *config {
	c := &config{
		maxEntries: DefaultMaxEntries,
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
// plain string, so a Secret never has to be converted by the caller. The
// password is handed to 7zz on its standard input, never on the command line,
// and it never appears in an error or a log line. A password with a line
// break cannot be passed that way and fails with ErrPassword.
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

// WithReplace lets Unpack replace an existing destination instead of failing
// with ErrExists.
func WithReplace() Option { return func(c *config) { c.replace = true } }

// WithLogger sets the logger for notices. A nil logger discards them.
func WithLogger(l *slog.Logger) Option { return func(c *config) { c.logger = l } }

// WithFreeSpace replaces the free disk space probe, which defaults to fsutil.FreeBytes. A probe that fails disables the disk check with a warning.
func WithFreeSpace(fn func(path string) (uint64, error)) Option {
	return func(c *config) { c.freeSpace = fn }
}

// WithSevenZip sets the path of the 7zz program, as found by tool.Locate. The
// default is to look 7zz up on PATH for each Unpack.
func WithSevenZip(path string) Option { return func(c *config) { c.sevenZip = path } }
