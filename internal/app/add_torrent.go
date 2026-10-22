package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/redact"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

// stagingTorrentName is the temporary name of a .torrent file while it is
// being received.
const stagingTorrentName = ".incoming.torrent"

// reservedTorrentName is the fallback name of a stored .torrent file when
// the natural one would collide with the torrent's own data.
const reservedTorrentName = ".bitnews-metainfo.torrent"

// StagedTorrent is a torrent that was received and checked, waiting for
// AddTorrent: either a .torrent file, kept in the new Download's directory
// <torrent incomplete>/<download id>/, or a magnet link, which has no files.
// One that is not passed to AddTorrent must be discarded.
type StagedTorrent struct {
	id      core.ID
	dir     string // the Download directory; empty for a magnet link
	tmpPath string
	info    *torrent.Info       // a .torrent file
	magnet  *torrent.MagnetInfo // a magnet link
	uri     core.Secret         // the magnet link as received
	done    bool
}

// IsMagnet reports whether the torrent is a magnet link, whose metadata is
// still to be fetched.
func (st *StagedTorrent) IsMagnet() bool { return st.magnet != nil }

// InfoHash returns the torrent's (v1) info hash.
func (st *StagedTorrent) InfoHash() core.InfoHash {
	if st.magnet != nil {
		return st.magnet.InfoHash
	}
	return st.info.InfoHash
}

// Discard removes the staged files. It does nothing after a successful
// AddTorrent, so it is safe to defer.
func (st *StagedTorrent) Discard() {
	if st == nil || st.done {
		return
	}
	st.done = true
	if st.dir != "" {
		_ = os.RemoveAll(st.dir)
	}
}

func (s *AddService) torrentsEnabled() error {
	if s.opts.torrentSources == nil {
		return unavailable("torrents_unavailable", "this instance is not set up for torrent downloads")
	}
	return nil
}

// StageTorrent reads a .torrent file from r (at most core.MaxMetainfoSize
// bytes; a larger one is an error of kind ErrTooLarge, code
// "torrent_too_large"), parses it with the torrent engine's own checks and
// keeps the raw bytes in the new Download's directory with mode 0600. A file
// that does not parse, or has hostile names, is invalid (code
// "invalid_torrent") and nothing is left on disk. Read errors of r are
// returned wrapped.
func (s *AddService) StageTorrent(ctx context.Context, r io.Reader) (st *StagedTorrent, err error) {
	if err := s.torrentsEnabled(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := s.settings.Current().Paths.Torrent.Incomplete
	if root == "" {
		return nil, fmt.Errorf("app: the torrent incomplete directory is not set")
	}
	data, err := io.ReadAll(io.LimitReader(r, core.MaxMetainfoSize+1))
	if err != nil {
		return nil, fmt.Errorf("app: read torrent: %w", err)
	}
	if len(data) > core.MaxMetainfoSize {
		return nil, tooLarge("torrent_too_large", "the torrent file is larger than %d MiB", core.MaxMetainfoSize>>20)
	}
	info, err := torrent.ParseMetainfo(data)
	if err != nil {
		return nil, invalid("invalid_torrent", "not a valid torrent: %v", strings.TrimPrefix(err.Error(), "torrent: invalid metainfo: "))
	}

	id := core.NewID()
	dir := filepath.Join(root, string(id))
	if err := fsutil.MkdirAll(dir, 0o777); err != nil {
		return nil, createDirError(err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	tmp := filepath.Join(dir, stagingTorrentName)
	if err := writeNewFile(tmp, data); err != nil {
		return nil, fmt.Errorf("app: store torrent: %w", err)
	}
	return &StagedTorrent{id: id, dir: dir, tmpPath: tmp, info: &info}, nil
}

// writeNewFile creates path (which must not exist) with mode 0600, writes
// data and syncs it.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// StageMagnet checks a magnet link (code "invalid_magnet" if it is not
// acceptable; see torrent.ParseMagnet). It writes nothing: the metadata is
// fetched by the torrent runner. The link is never logged or put in an error
// message, because its tracker URLs may carry a passkey.
func (s *AddService) StageMagnet(ctx context.Context, uri string) (*StagedTorrent, error) {
	if err := s.torrentsEnabled(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	uri = strings.TrimSpace(uri)
	m, err := torrent.ParseMagnet(uri)
	if err != nil {
		return nil, invalid("invalid_magnet", "not a usable magnet link: %v", strings.TrimPrefix(err.Error(), "torrent: invalid magnet link: "))
	}
	return &StagedTorrent{id: core.NewID(), magnet: &m, uri: core.Secret(uri)}, nil
}

// FetchedSource is what Fetch downloaded: exactly one field is set.
type FetchedSource struct {
	NZB     *StagedNZB
	Torrent *StagedTorrent
}

// Discard removes whatever was staged.
func (f FetchedSource) Discard() {
	f.NZB.Discard()
	f.Torrent.Discard()
}

// Fetch downloads rawURL, which may be an NZB or a .torrent file, and stages
// it as what it is: a body that starts with 'd' (bencode) is a torrent,
// anything else is taken to be an NZB. A redirect to a magnet link ends the
// fetch and stages that link (it is followed once: it is not fetched, so it
// cannot redirect again). The limits, the timeout, the URL redaction and the
// error codes are those of FetchNZB, with the torrent size cap for torrents.
// A torrent without torrent support (WithTorrentSources) is an error of kind
// ErrUnavailable.
func (s *AddService) Fetch(ctx context.Context, rawURL string) (FetchedSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return FetchedSource{}, invalid("invalid_url", "url must be an absolute http or https URL")
	}
	log := s.opts.log.With("url", redact.URL(rawURL))
	const what = "NZB or torrent"

	ctx, cancel := context.WithTimeout(ctx, s.opts.fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchedSource{}, invalid("invalid_url", "url must be an absolute http or https URL")
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	req.Header.Set("Accept", "application/x-bittorrent, application/x-nzb, text/xml;q=0.9, */*;q=0.1")

	resp, err := s.anyClient.Do(req)
	if err != nil {
		return FetchedSource{}, s.fetchFailedOf(ctx, log, what, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); len(loc) >= len("magnet:") && strings.EqualFold(loc[:len("magnet:")], "magnet:") {
			st, err := s.StageMagnet(ctx, loc)
			if err != nil {
				return FetchedSource{}, err
			}
			log.Info("fetch redirected to a magnet link")
			return FetchedSource{Torrent: st}, nil
		}
	}
	if resp.StatusCode != http.StatusOK {
		log.Warn("nzb or torrent fetch failed", "status", resp.StatusCode)
		return FetchedSource{}, upstream("fetch_failed", "could not fetch the %s: the server answered %d %s", what, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	body := &errRecorder{r: resp.Body}
	br := bufio.NewReader(body)
	isTorrent := looksLikeBencode(br)
	var out FetchedSource
	switch {
	case isTorrent && resp.ContentLength > core.MaxMetainfoSize:
		return FetchedSource{}, tooLarge("torrent_too_large", "the torrent file is larger than %d MiB", core.MaxMetainfoSize>>20)
	case !isTorrent && resp.ContentLength > s.opts.maxNZBBytes:
		return FetchedSource{}, tooLarge("nzb_too_large", "the NZB is larger than %d MiB", s.opts.maxNZBBytes>>20)
	case isTorrent:
		out.Torrent, err = s.StageTorrent(ctx, br)
	default:
		out.NZB, err = s.Stage(ctx, br)
	}
	if err != nil {
		if _, isApp := errors.AsType[*Error](err); !isApp && body.err != nil && !errors.Is(body.err, io.EOF) {
			return FetchedSource{}, s.fetchFailedOf(ctx, log, what, body.err)
		}
		return FetchedSource{}, err
	}
	if isTorrent {
		log.Info("torrent fetched")
	} else {
		log.Info("nzb fetched")
	}
	return out, nil
}

// looksLikeBencode reports whether the first byte of r is 'd': a .torrent
// file is a bencoded dictionary, an NZB starts with '<' (or a byte order
// mark or white space). It does not consume anything.
func looksLikeBencode(r *bufio.Reader) bool {
	b, err := r.Peek(1)
	return err == nil && b[0] == 'd'
}

// AddTorrent creates the Download for a StagedTorrent at the end of the
// torrent Queue, stores its source and publishes core.DownloadAdded. The
// Download is Queued for a .torrent file and FetchingMetadata for a magnet
// link, whose size is not known yet (TotalBytes is 0). A torrent whose info
// hash a Download already has is refused with an ErrDuplicate. On any error
// the staged files are removed. On success st must not be used again.
//
// The Download's directory is <torrent incomplete>/<download id>/. AddTorrent
// creates it only for a .torrent file, where it holds the stored metainfo.
func (s *AddService) AddTorrent(ctx context.Context, st *StagedTorrent, req AddRequest) (d core.Download, err error) {
	defer func() {
		if err != nil {
			st.Discard()
		}
	}()
	if err := s.torrentsEnabled(); err != nil {
		return core.Download{}, err
	}
	if req.Priority == "" {
		req.Priority = core.DownloadPriorityNormal
	}
	if err := s.Check(ctx, req); err != nil {
		return core.Download{}, err
	}
	src := core.TorrentSource{DownloadID: st.id, InfoHash: st.InfoHash(), Magnet: st.uri}
	to, total := core.StateFetchingMetadata, int64(0)
	if st.info != nil {
		to, total = core.StateQueued, st.info.Size
		final := filepath.Join(st.dir, torrentFileName(st, resolveTorrentName(req, st)))
		if err := os.Rename(st.tmpPath, final); err != nil {
			return core.Download{}, fmt.Errorf("app: store torrent: %w", err)
		}
		src.TorrentPath = final
	}
	if err := core.Transition(core.StateAdded, to); err != nil {
		return core.Download{}, err
	}

	now := s.opts.now()
	d = core.Download{
		ID: st.id, Name: resolveTorrentName(req, st), SourceKind: core.SourceTorrent, CategoryID: req.CategoryID,
		State: core.StateAdded, Priority: req.Priority, Paused: req.Paused,
		TotalBytes: total, AddedAt: now, UpdatedAt: now,
	}
	if err := s.insertTorrent(ctx, &d, src, to); err != nil {
		return core.Download{}, err
	}
	st.done = true
	s.ds.events.Publish(core.DownloadAdded{Download: d})
	return d, nil
}

// insertTorrent is insert for torrents: the Download is created in state
// Added, then its source row is stored, then it moves to its Queue state
// (to), so the runner never sees a torrent Download without its source. A
// failure after the first step deletes the Download again.
func (s *AddService) insertTorrent(ctx context.Context, d *core.Download, src core.TorrentSource, to core.State) error {
	repo := s.opts.torrentSources
	s.ds.mu.Lock()
	defer s.ds.mu.Unlock()
	if existing, err := repo.GetByInfoHash(ctx, src.InfoHash); err == nil || errors.Is(err, core.ErrSecretUnavailable) {
		return s.duplicate(ctx, existing.DownloadID)
	} else if !errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("app: add torrent: %w", err)
	}
	queue, err := s.ds.queueOf(ctx, core.SourceTorrent)
	if err != nil {
		return err
	}
	for _, q := range queue {
		d.QueuePosition = max(d.QueuePosition, q.QueuePosition+1)
	}
	if err := s.ds.downloads.Create(ctx, *d); err != nil {
		return fmt.Errorf("app: add download: %w", err)
	}
	queued, err := s.enqueueTorrent(ctx, src, to)
	if err != nil {
		if derr := s.ds.downloads.Delete(context.WithoutCancel(ctx), d.ID); derr != nil {
			err = errors.Join(err, derr)
		}
		return err
	}
	*d = queued
	return nil
}

func (s *AddService) enqueueTorrent(ctx context.Context, src core.TorrentSource, to core.State) (core.Download, error) {
	if err := s.opts.torrentSources.Create(ctx, src); err != nil {
		if errors.Is(err, core.ErrConflict) {
			// Somebody else stored the same info hash in the meantime.
			if existing, gerr := s.opts.torrentSources.GetByInfoHash(ctx, src.InfoHash); gerr == nil || errors.Is(gerr, core.ErrSecretUnavailable) {
				return core.Download{}, s.duplicate(ctx, existing.DownloadID)
			}
		}
		return core.Download{}, fmt.Errorf("app: add download source: %w", err)
	}
	queued, err := s.ds.downloads.UpdateState(ctx, src.DownloadID, core.StateChange{To: to, At: s.opts.now()})
	if err != nil {
		return core.Download{}, fmt.Errorf("app: queue download: %w", err)
	}
	return queued, nil
}

// duplicate builds the ErrDuplicate for the Download that has the torrent.
func (s *AddService) duplicate(ctx context.Context, id core.ID) error {
	dup := ErrDuplicate{ExistingID: id}
	if d, err := s.ds.downloads.Get(ctx, id); err == nil {
		dup.Name = d.Name
	}
	return dup
}

// resolveTorrentName picks the display name: the request's, the torrent's
// own (or the magnet's "dn"), the upload's file name, else "torrent-<id>".
func resolveTorrentName(req AddRequest, st *StagedTorrent) string {
	var own string
	if st.info != nil {
		own = st.info.Name
	} else {
		own = st.magnet.DisplayName
	}
	fileName := req.FileName
	if len(fileName) >= 8 && strings.EqualFold(fileName[len(fileName)-8:], ".torrent") {
		fileName = fileName[:len(fileName)-8]
	}
	for _, c := range []string{req.Name, own, fileName} {
		if clean := cleanDisplayName(c); clean != "" {
			return clean
		}
	}
	return "torrent-" + string(st.id)
}

// torrentFileName is the name of the stored .torrent file: the display name
// made safe plus ".torrent". It must not be the name of anything the torrent
// itself puts directly into the Download directory (its root directory, or
// its top-level files), so a hostile torrent cannot make the engine write
// over its own metainfo; the reserved name is used in that case.
func torrentFileName(st *StagedTorrent, displayName string) string {
	name, err := fsutil.SafeName(displayName + ".torrent")
	if err != nil {
		name = reservedTorrentName
	}
	taken := make(map[string]bool)
	for _, f := range st.info.Files {
		top, _, _ := strings.Cut(f.Path, "/")
		taken[strings.ToLower(top)] = true
	}
	if taken[strings.ToLower(name)] {
		name = reservedTorrentName
	}
	if taken[strings.ToLower(name)] {
		name = ".bitnews-" + string(st.id) + ".torrent"
	}
	return name
}
