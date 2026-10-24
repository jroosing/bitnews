package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/fsutil"
	"github.com/jroosing/bitnews/internal/postproc/tidy"
	"github.com/jroosing/bitnews/internal/usenet/nzb"
)

// DefaultMaxNZBBytes is the size cap of an NZB, uploaded or fetched.
const DefaultMaxNZBBytes = 64 << 20

const (
	maxDisplayNameBytes = 255
	maxPasswordBytes    = 1024
	// stagingName is the temporary name of the NZB while it is being
	// received; it gets its final name once the Download's name is known.
	stagingName = ".incoming.nzb"
)

// SettingsReader gives the AddService the Settings in effect.
type SettingsReader interface {
	Current() core.Settings
}

// AddService creates Downloads, from NZBs and torrents. Callers use two
// calls: Receive stages an uploaded file as it arrives (a multipart upload
// may send the file before the fields that describe it), and Add takes the
// whole request, the received file or a link, and does the rest: checking,
// fetching a link, the Category (found or created), naming, storing and
// queueing, the torrent's Share Limit, and cleaning up on any error.
//
// Inside, an NZB is staged into the new Download's directory
// <usenet incomplete>/<download id>/ while it is parsed, and a .torrent into
// <torrent incomplete>/<download id>/; nothing is left behind when it is not
// valid.
type AddService struct {
	ds       *DownloadService
	sources  core.DownloadSourceRepo
	settings SettingsReader
	opts     options
	client   *http.Client
	// anyClient fetches a URL that may turn out to be an NZB, a .torrent or
	// a redirect to a magnet link: its redirect policy lets a redirect to a
	// "magnet:" URI end the fetch instead of failing it.
	anyClient *http.Client
}

// NewAddService returns an AddService. Downloads are created through ds, so
// they share its locking and its repositories.
func NewAddService(ds *DownloadService, sources core.DownloadSourceRepo, settings SettingsReader, opts ...Option) *AddService {
	o := newOptions(opts)
	c := newFetchClient(o)
	anyClient := *c
	anyClient.CheckRedirect = checkRedirectOrMagnet
	return &AddService{ds: ds, sources: sources, settings: settings, opts: o, client: c, anyClient: &anyClient}
}

// AddRequest describes the Download to create.
type AddRequest struct {
	// Name is the display name. Empty takes the NZB's own name, then
	// FileName, then the first file's name, then "nzb-<id>".
	Name string
	// FileName is the name of the uploaded file, if any. A trailing ".nzb"
	// is ignored.
	FileName string
	// CategoryID must name an existing Category, or be empty to use the
	// NZB's category (matched by name; an unknown one is ignored).
	CategoryID core.ID
	// Priority defaults to normal.
	Priority core.DownloadPriority
	// Password of the release's archives. Empty takes the NZB's, then the
	// "{{password}}" suffix of the name.
	Password core.Secret
	Paused   bool

	// Upload is the received file (Receive); URL is a magnet link or the
	// http(s) URL of an NZB or a .torrent. Add takes exactly one of them.
	Upload *Upload
	URL    string
	// Only, when set, accepts that kind of Source alone.
	Only core.SourceKind
	// CategoryName names a Category to use, created when it does not
	// exist. CategoryID wins when both are set.
	CategoryName string
	// ShareLimit is the torrent's own Share Limit; nil uses the default.
	ShareLimit *core.ShareLimit
}

// StagedNZB is an NZB that was received and parsed, waiting for Add.
type StagedNZB struct {
	id      core.ID
	dir     string
	tmpPath string
	nzb     *nzb.NZB
	done    bool
}

// Discard removes the staged files. It does nothing after a successful Add,
// so it is safe to defer.
func (st *StagedNZB) Discard() {
	if st == nil || st.done {
		return
	}
	st.done = true
	_ = os.RemoveAll(st.dir)
}

// Stage reads an NZB from r, which the caller must bound if it is hostile
// (the NZB limit applies in any case). The raw bytes are kept in the new
// Download's directory (mode 0600). An NZB that is too large is an error of
// kind ErrTooLarge (code "nzb_too_large"); one that does not parse is
// invalid (code "invalid_nzb"). Read errors of r are returned wrapped.
func (s *AddService) stage(ctx context.Context, r io.Reader) (st *StagedNZB, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := s.settings.Current().Paths.Usenet.Incomplete
	id := core.NewID()
	dir := filepath.Join(root, string(id))
	// 0o777 before the umask, like every directory the daemon creates.
	if err := fsutil.MkdirAll(dir, 0o777); err != nil {
		return nil, createDirError(err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	tmp := filepath.Join(dir, stagingName)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("app: create nzb file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// The parser sees the bytes through a tee that writes them to disk, so
	// the NZB is never held in memory as a whole.
	w := bufio.NewWriter(f)
	lim := nzb.DefaultLimits()
	lim.MaxBytes = s.opts.maxNZBBytes
	parsed, perr := nzb.Parse(io.TeeReader(r, w), lim)
	if perr != nil {
		return nil, s.parseError(perr)
	}
	if err := w.Flush(); err != nil {
		return nil, fmt.Errorf("app: write nzb: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("app: write nzb: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("app: write nzb: %w", err)
	}
	return &StagedNZB{id: id, dir: dir, tmpPath: tmp, nzb: parsed}, nil
}

func (s *AddService) parseError(err error) error {
	switch {
	case errors.Is(err, nzb.ErrTooLarge):
		return tooLarge("nzb_too_large", "the NZB is larger than %d MiB", s.opts.maxNZBBytes>>20)
	case errors.Is(err, nzb.ErrMalformed), errors.Is(err, nzb.ErrNoFiles),
		errors.Is(err, nzb.ErrTooManyFiles), errors.Is(err, nzb.ErrTooManySegments):
		return invalid("invalid_nzb", "not a valid NZB: %v", err)
	}
	return fmt.Errorf("app: read nzb: %w", err)
}

// Check validates the fields of req that do not depend on the NZB: priority,
// password length and the requested Category. Add does it too; calling it
// first saves fetching an NZB that would be refused anyway.
func (s *AddService) check(ctx context.Context, req AddRequest) error {
	if req.Priority != "" && !slices.Contains(knownPriorities, req.Priority) {
		return invalid("invalid_priority", "unknown priority %q: use force, high, normal or low", req.Priority)
	}
	if len(req.Password) > maxPasswordBytes {
		return invalid("invalid_password", "the password is longer than %d bytes", maxPasswordBytes)
	}
	if req.CategoryID != "" {
		if _, err := s.ds.cats.Get(ctx, req.CategoryID); errors.Is(err, core.ErrNotFound) {
			return invalid("unknown_category", "category %q does not exist", req.CategoryID)
		} else if err != nil {
			return fmt.Errorf("app: add download: %w", err)
		}
	}
	return nil
}

// Add creates the Download for a StagedNZB in state Queued at the end of the
// NZB Queue, stores its source and publishes core.DownloadAdded. On any
// error the staged files are removed. On success st must not be used again.
func (s *AddService) addNZB(ctx context.Context, st *StagedNZB, req AddRequest) (d core.Download, err error) {
	defer func() {
		if err != nil {
			st.Discard()
		}
	}()
	if req.Priority == "" {
		req.Priority = core.DownloadPriorityNormal
	}
	if err := s.check(ctx, req); err != nil {
		return core.Download{}, err
	}
	categoryID, err := s.resolveCategory(ctx, req.CategoryID, st.nzb.Meta.Category)
	if err != nil {
		return core.Download{}, err
	}

	name, suffixPassword := resolveName(req, st.nzb, st.id)
	if s.settings.Current().PostProcess.CleanNames {
		// Clean.py as a built-in option (ADR-0009): read per add, applied
		// to the final name, which feeds the Download, the stored NZB file
		// and the output folder. Torrents never come through here.
		name = tidy.Clean(name)
	}
	password := firstNonEmpty(req.Password.Reveal(), strings.TrimSpace(st.nzb.Meta.Password), suffixPassword)

	finalPath := filepath.Join(st.dir, nzbFileName(name))
	if err := os.Rename(st.tmpPath, finalPath); err != nil {
		return core.Download{}, fmt.Errorf("app: store nzb: %w", err)
	}

	now := s.opts.now()
	d = core.Download{
		ID: st.id, Name: name, SourceKind: core.SourceNZB, CategoryID: categoryID,
		State: core.StateQueued, Priority: req.Priority, Paused: req.Paused,
		TotalBytes: nzb.UpFrontBytes(st.nzb), AddedAt: now, UpdatedAt: now,
	}
	if err := s.insert(ctx, &d, finalPath, core.Secret(password)); err != nil {
		return core.Download{}, err
	}
	st.done = true
	s.ds.events.Publish(core.DownloadAdded{Download: d})
	return d, nil
}

// insert stores the Download at the end of the NZB Queue together with its
// source, so that the runner, which only looks at Queued Downloads, never
// sees one without its source:
//
//  1. the Download is created in state Added (with its Queue position);
//  2. its source row is stored;
//  3. the Download moves to Queued.
//
// A failure after step 1 deletes the Download again (the source row goes
// with it by cascade). On success d is the stored, Queued Download.
func (s *AddService) insert(ctx context.Context, d *core.Download, nzbPath string, password core.Secret) error {
	s.ds.mu.Lock()
	defer s.ds.mu.Unlock()
	queue, err := s.ds.queueOf(ctx, core.SourceNZB)
	if err != nil {
		return err
	}
	for _, q := range queue {
		d.QueuePosition = max(d.QueuePosition, q.QueuePosition+1)
	}
	d.State = core.StateAdded
	if err := s.ds.downloads.Create(ctx, *d); err != nil {
		return fmt.Errorf("app: add download: %w", err)
	}
	queued, err := s.enqueue(ctx, d.ID, nzbPath, password)
	if err != nil {
		if derr := s.ds.downloads.Delete(context.WithoutCancel(ctx), d.ID); derr != nil {
			err = errors.Join(err, derr)
		}
		return err
	}
	*d = queued
	return nil
}

// enqueue is steps 2 and 3 of insert.
func (s *AddService) enqueue(ctx context.Context, id core.ID, nzbPath string, password core.Secret) (core.Download, error) {
	if err := s.sources.Create(ctx, id, nzbPath, password); err != nil {
		return core.Download{}, fmt.Errorf("app: add download source: %w", err)
	}
	queued, err := s.ds.downloads.UpdateState(ctx, id, core.StateChange{To: core.StateQueued, At: s.opts.now()})
	if err != nil {
		return core.Download{}, fmt.Errorf("app: queue download: %w", err)
	}
	return queued, nil
}

// resolveCategory validates the requested Category, or else looks the NZB's
// category up by name. A name no Category has is ignored: indexers put free
// text there, and the Download is better added than refused.
func (s *AddService) resolveCategory(ctx context.Context, id core.ID, metaName string) (core.ID, error) {
	if id != "" {
		if _, err := s.ds.cats.Get(ctx, id); errors.Is(err, core.ErrNotFound) {
			return "", invalid("unknown_category", "category %q does not exist", id)
		} else if err != nil {
			return "", fmt.Errorf("app: add download: %w", err)
		}
		return id, nil
	}
	if metaName == "" {
		return "", nil
	}
	cats, err := s.ds.cats.List(ctx)
	if err != nil {
		return "", fmt.Errorf("app: add download: %w", err)
	}
	for _, c := range cats {
		if strings.EqualFold(c.Name, metaName) {
			return c.ID, nil
		}
	}
	return "", nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolveName picks the display name: the request's, the NZB's, the upload's
// file name, the first file's guessed name, else "nzb-<id>". A "{{password}}"
// suffix is cut off the chosen name and returned.
func resolveName(req AddRequest, n *nzb.NZB, id core.ID) (name, suffixPassword string) {
	candidates := []string{req.Name, n.Meta.Name, trimNZBExt(req.FileName), firstFileName(n)}
	for _, c := range candidates {
		base, pw := splitPasswordSuffix(c)
		if clean := cleanDisplayName(base); clean != "" {
			return clean, pw
		}
	}
	return "nzb-" + string(id), ""
}

func firstFileName(n *nzb.NZB) string {
	for _, f := range n.Files {
		if f.Kind == nzb.KindData {
			return f.Name
		}
	}
	if len(n.Files) > 0 {
		return n.Files[0].Name
	}
	return ""
}

func trimNZBExt(s string) string {
	if len(s) >= 4 && strings.EqualFold(s[len(s)-4:], ".nzb") {
		return s[:len(s)-4]
	}
	return s
}

var passwordSuffix = regexp.MustCompile(`^(.*)\{\{(.+)\}\}\s*$`)

// splitPasswordSuffix cuts a trailing "{{password}}" off s.
func splitPasswordSuffix(s string) (name, password string) {
	if m := passwordSuffix.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1], m[2]
	}
	return s, ""
}

// cleanDisplayName makes a name safe to show: invalid UTF-8, control and
// format characters (such as bidi overrides) and line separators are
// removed, surrounding space is trimmed and the result is capped at 255
// bytes. It is a display name only: files are named with fsutil.SafeName.
func cleanDisplayName(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > maxDisplayNameBytes {
		s = s[:maxDisplayNameBytes]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1] // cut a rune in half: drop the stub
		}
	}
	return strings.TrimSpace(s)
}

// nzbFileName is the name of the stored NZB: the display name made safe
// plus ".nzb".
func nzbFileName(displayName string) string {
	if safe, err := fsutil.SafeName(displayName + ".nzb"); err == nil {
		return safe
	}
	return "download.nzb"
}
