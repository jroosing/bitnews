package finalize

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// MarkerName is the file written inside the final dir to make Finish
// restart-safe. It holds the Download ID on its first line.
const MarkerName = ".bitnews-done"

// Warning is a non-fatal condition the caller may turn into a Warning event.
type Warning struct {
	// Kind is a stable machine-readable code: "cross_device",
	// "category_missing" or "marker_conflict".
	Kind       string
	DownloadID core.ID
	Message    string
	// Path is the path the warning is about, if any.
	Path string
}

// Mover moves the directory src to the not yet existing dst.
type Mover func(src, dst string) (fsutil.MoveResult, error)

// Deps are the Finisher's collaborators.
type Deps struct {
	// Settings returns the current Settings. Required.
	Settings func(ctx context.Context) (core.Settings, error)
	// Categories looks a Category up by ID. Required.
	Categories func(ctx context.Context, id core.ID) (core.Category, error)
	// Warn receives warnings. The default logs them with slog.
	Warn func(ctx context.Context, w Warning)
	// Clock stamps the marker. The default is time.Now.
	Clock func() time.Time
	// Mover moves the directory. The default is fsutil.MoveDir.
	Mover Mover
}

// Option configures a Finisher.
type Option func(*Finisher)

// WithLogger sets the logger used by the default Warn sink.
func WithLogger(l *slog.Logger) Option {
	return func(f *Finisher) {
		if l != nil {
			f.log = l
		}
	}
}

// Finisher cleans up and moves finished Downloads to the complete tree.
type Finisher struct {
	deps Deps
	log  *slog.Logger
}

// New returns a Finisher using deps.
func New(deps Deps, opts ...Option) *Finisher {
	f := &Finisher{deps: deps, log: slog.Default()}
	for _, o := range opts {
		o(f)
	}
	if f.deps.Clock == nil {
		f.deps.Clock = time.Now
	}
	if f.deps.Mover == nil {
		f.deps.Mover = fsutil.MoveDir
	}
	if f.deps.Warn == nil {
		f.deps.Warn = func(ctx context.Context, w Warning) {
			f.log.WarnContext(ctx, "finalize: "+w.Message, "kind", w.Kind, "download", string(w.DownloadID), "path", w.Path)
		}
	}
	return f
}

// Finish cleans up dir and moves it to the destination of d, returning the
// final dir. unpackedDir is the dir Unpack produced, or "" if there were no
// archives; it is ignored for torrents.
func (f *Finisher) Finish(ctx context.Context, d core.Download, dir, unpackedDir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.deps.Settings == nil || f.deps.Categories == nil {
		return "", errors.New("finalize: Settings and Categories are required")
	}
	if dir == "" {
		return "", errors.New("finalize: empty download dir")
	}
	dir = filepath.Clean(dir)

	base, err := f.destination(ctx, d)
	if err != nil {
		return "", err
	}

	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A retry after a crash: dir may already have been moved.
		if found, ok := findMoved(base, d.ID); ok {
			return found, nil
		}
		return "", fmt.Errorf("finalize: download dir %q is gone and no finished copy of %s was found at %q", dir, d.ID, base)
	case err != nil:
		return "", fmt.Errorf("finalize: %w", err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return "", fmt.Errorf("%w: download dir %q", fsutil.ErrSymlink, dir)
	case !fi.IsDir():
		return "", fmt.Errorf("finalize: download dir %q is not a directory", dir)
	}

	if d.SourceKind == core.SourceNZB {
		if err := cleanUsenet(ctx, dir, unpackedDir); err != nil {
			return "", err
		}
	}

	// Already where it belongs (a torrent the engine relocated, or equal
	// roots): nothing to move and nothing may be added.
	if sameDir(dir, base) {
		return base, nil
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := f.writeMarker(ctx, d, dir); err != nil {
		return "", err
	}
	dst := fsutil.Unique(base)
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return "", fmt.Errorf("finalize: create %q: %w", filepath.Dir(dst), err)
	}
	res, err := f.deps.Mover(dir, dst)
	if res.CrossDevice {
		f.deps.Warn(ctx, Warning{
			Kind: "cross_device", DownloadID: d.ID, Path: dst,
			Message: fmt.Sprintf("moved %q to %q across filesystems; hardlinks will not work for this layout", dir, dst),
		})
	}
	if err != nil {
		return "", fmt.Errorf("finalize: %w", err)
	}
	return dst, nil
}

// Destination returns the directory Finish would move d to, before a name
// clash is resolved with fsutil.Unique: the Category DirOverride, else
// <complete root>/<category>, else <complete root>, then the sanitized
// Download name. The torrent runner uses it to relocate a torrent to the same
// place without a rehash.
func (f *Finisher) Destination(ctx context.Context, d core.Download) (string, error) {
	if f.deps.Settings == nil || f.deps.Categories == nil {
		return "", errors.New("finalize: Settings and Categories are required")
	}
	return f.destination(ctx, d)
}

// destination returns the wanted final dir, before Unique: the Category
// DirOverride, else <complete root>/<category>, else <complete root>, then
// the sanitized Download name. The result is always inside the chosen root.
func (f *Finisher) destination(ctx context.Context, d core.Download) (string, error) {
	s, err := f.deps.Settings(ctx)
	if err != nil {
		return "", fmt.Errorf("finalize: settings: %w", err)
	}
	var root string
	switch d.SourceKind {
	case core.SourceNZB:
		root = s.Paths.Usenet.Complete
	case core.SourceTorrent:
		root = s.Paths.Torrent.Complete
	default:
		return "", fmt.Errorf("finalize: unknown source kind %q", d.SourceKind)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("finalize: complete root %q is not an absolute path", root)
	}
	root = filepath.Clean(root)

	if d.CategoryID != "" {
		cat, err := f.deps.Categories(ctx, d.CategoryID)
		switch {
		case errors.Is(err, core.ErrNotFound):
			f.deps.Warn(ctx, Warning{
				Kind: "category_missing", DownloadID: d.ID,
				Message: fmt.Sprintf("category %q no longer exists; using the complete root", d.CategoryID),
			})
		case err != nil:
			return "", fmt.Errorf("finalize: category: %w", err)
		default:
			if root, err = categoryRoot(root, cat); err != nil {
				return "", err
			}
		}
	}
	dst, err := fsutil.SafeJoin(root, dirName(d))
	if err != nil {
		return "", fmt.Errorf("finalize: destination: %w", err)
	}
	return dst, nil
}

// categoryRoot returns the dir the Download dir goes into for cat. Names
// and overrides were validated in core; this defends anyway.
func categoryRoot(completeRoot string, cat core.Category) (string, error) {
	if err := cat.Validate(); err != nil {
		return "", fmt.Errorf("finalize: category %q: %w", cat.ID, err)
	}
	if cat.DirOverride != "" {
		if slices.Contains(strings.FieldsFunc(cat.DirOverride, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
			return "", fmt.Errorf("%w: dir override %q has a .. element", fsutil.ErrUnsafePath, cat.DirOverride)
		}
		return filepath.Clean(cat.DirOverride), nil
	}
	dir, err := fsutil.SafeJoin(completeRoot, cat.Name)
	if err != nil {
		return "", fmt.Errorf("finalize: category dir: %w", err)
	}
	return dir, nil
}

func dirName(d core.Download) string {
	if n, err := fsutil.SafeName(d.Name); err == nil {
		return n
	}
	if n, err := fsutil.SafeName("download-" + string(d.ID)); err == nil {
		return n
	}
	return "download"
}

func (f *Finisher) writeMarker(ctx context.Context, d core.Download, dir string) error {
	p := filepath.Join(dir, MarkerName)
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		// Left by an earlier attempt of ours, or one of the Download's own
		// files. Never overwrite it.
		if !markerHolds(dir, d.ID) {
			f.deps.Warn(ctx, Warning{
				Kind: "marker_conflict", DownloadID: d.ID, Path: p,
				Message: fmt.Sprintf("%q already exists and is not ours; a retry after a crash cannot be detected", p),
			})
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("finalize: marker: %w", err)
	}
	_, werr := fmt.Fprintf(fh, "%s\n%s\n", d.ID, f.deps.Clock().UTC().Format(time.RFC3339))
	if cerr := fh.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("finalize: marker: %w", werr)
	}
	return nil
}

// sameDir reports whether a and b are the same existing dir.
func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// findMoved looks for the dir a previous Finish moved to: base, or a
// Unique'd variant of it (base.1, base.2, ...; the name may be shortened to
// fit). Only a dir whose marker holds id counts, so a dir that merely has the
// same name is never mistaken for ours.
func findMoved(base string, id core.ID) (string, bool) {
	if markerHolds(base, id) {
		return base, true
	}
	parent, name := filepath.Split(base)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		stem := variantRE.ReplaceAllString(e.Name(), "")
		if !e.IsDir() || stem == e.Name() || !strings.HasPrefix(name, stem) {
			continue
		}
		if p := filepath.Join(parent, e.Name()); markerHolds(p, id) {
			return p, true
		}
	}
	return "", false
}

var variantRE = regexp.MustCompile(`\.[0-9a-f]+$`)

// markerHolds reports whether dir is a real directory whose regular marker
// file starts with id.
func markerHolds(dir string, id core.ID) bool {
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return false
	}
	p := filepath.Join(dir, MarkerName)
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || fi.Size() > 1024 {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(b), "\n")
	return first == string(id)
}
