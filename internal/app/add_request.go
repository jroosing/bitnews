package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/jroosing/bitnews/internal/core"
)

// Upload is a file received for a new Download: an NZB or a .torrent,
// already staged in the new Download's directory while it was read, so a
// large NZB is never held in memory. Receive makes one; Add consumes it. One
// that is not passed to Add must be discarded.
type Upload struct {
	nzb      *StagedNZB
	torrent  *StagedTorrent
	fileName string
}

// Kind is the Source the file turned out to be.
func (u *Upload) Kind() core.SourceKind {
	if u.torrent != nil {
		return core.SourceTorrent
	}
	return core.SourceNZB
}

// Discard removes the staged files. It does nothing after a successful Add,
// so it is safe to defer.
func (u *Upload) Discard() {
	if u == nil {
		return
	}
	u.nzb.Discard()
	u.torrent.Discard()
}

// Receive reads an uploaded file from r and stages it. kind says what it
// must be: core.SourceNZB or core.SourceTorrent, or empty to decide by the
// content (a .torrent is a bencoded dictionary and starts with "d"; anything
// else is taken to be an NZB). Undecided, a file whose extension says the
// other kind is refused (codes "not_a_torrent", "not_an_nzb"), and an empty
// one is code "empty_file". The errors of the NZB and torrent parsers and
// their size limits apply.
func (s *AddService) Receive(ctx context.Context, r io.Reader, fileName string, kind core.SourceKind) (*Upload, error) {
	u := &Upload{fileName: path.Base(strings.ReplaceAll(fileName, `\`, "/"))}
	if fileName == "" {
		u.fileName = ""
	}
	if kind == "" {
		br := bufio.NewReader(r)
		head, err := br.Peek(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("app: read upload: %w", err)
		}
		if len(head) == 0 {
			return nil, invalid("empty_file", "That file is empty.")
		}
		kind = core.SourceNZB
		if head[0] == 'd' {
			kind = core.SourceTorrent
		}
		switch ext := strings.ToLower(path.Ext(u.fileName)); {
		case ext == ".torrent" && kind != core.SourceTorrent:
			return nil, invalid("not_a_torrent", "That file ends in .torrent but is not a torrent file.")
		case ext == ".nzb" && kind == core.SourceTorrent:
			return nil, invalid("not_an_nzb", "That file ends in .nzb but is a torrent file.")
		}
		r = br
	}
	var err error
	if kind == core.SourceTorrent {
		u.torrent, err = s.stageTorrent(ctx, r)
	} else {
		u.nzb, err = s.stage(ctx, r)
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// isMagnet reports whether s is a magnet link (the scheme ignores case).
func isMagnet(s string) bool {
	return len(s) >= len("magnet:") && strings.EqualFold(s[:len("magnet:")], "magnet:")
}

// Add creates a Download from req and returns it. It is the one way in for
// every caller: the own API, the UI and both media-manager adapters.
//
// The source is req.Upload (from Receive) or req.URL: a magnet link, or the
// http(s) URL of an NZB or a .torrent, which is fetched now (never stored,
// logged only redacted). Exactly one must be given (codes "missing_source",
// "ambiguous_source"). req.Only restricts the kind (codes "not_an_nzb",
// "not_a_torrent"). The cheap fields (priority, password, CategoryID) are
// checked before anything is fetched.
//
// req.CategoryName is a Category to use by name, created when it does not
// exist (what SABnzbd and qBittorrent do for media managers); CategoryID
// wins when both are set. req.ShareLimit is the torrent's own Share Limit.
//
// On any error whatever was staged is removed, including req.Upload. A
// torrent that a Download already has is an ErrDuplicate.
func (s *AddService) Add(ctx context.Context, req AddRequest) (d core.Download, err error) {
	up := req.Upload
	defer func() { up.Discard() }() // does nothing after a successful add
	switch {
	case up == nil && req.URL == "":
		return core.Download{}, invalid("missing_source", "give an NZB or torrent file, or a link")
	case up != nil && req.URL != "":
		return core.Download{}, invalid("ambiguous_source", "give either a file or a link, not both")
	}
	if err := s.check(ctx, req); err != nil {
		return core.Download{}, err
	}
	if up == nil {
		if up, err = s.fetchSource(ctx, strings.TrimSpace(req.URL), req.Only); err != nil {
			return core.Download{}, err
		}
	}
	switch {
	case req.Only == core.SourceNZB && up.Kind() != core.SourceNZB:
		return core.Download{}, invalid("not_an_nzb", "this is a torrent, and only NZBs can be added here")
	case req.Only == core.SourceTorrent && up.Kind() != core.SourceTorrent:
		return core.Download{}, invalid("not_a_torrent", "this is an NZB, and only torrents can be added here")
	}
	if req.FileName == "" {
		req.FileName = up.fileName
	}
	if req.CategoryID == "" && strings.TrimSpace(req.CategoryName) != "" {
		cat, err := ensureCategory(ctx, s.ds.cats, s.opts.log, req.CategoryName)
		if err != nil {
			return core.Download{}, err
		}
		req.CategoryID = cat.ID
	}
	if up.nzb != nil {
		return s.addNZB(ctx, up.nzb, req)
	}
	d, err = s.addTorrent(ctx, up.torrent, req)
	if err != nil {
		return core.Download{}, err
	}
	if req.ShareLimit != nil && s.opts.shareLimits != nil {
		if err := s.opts.shareLimits.SetShareLimit(ctx, d.ID, *req.ShareLimit); err != nil {
			s.opts.log.Warn("cannot set the share limit of a new torrent", "id", d.ID, "err", err)
		}
	}
	return d, nil
}

// fetchSource stages the source a link points at.
func (s *AddService) fetchSource(ctx context.Context, link string, only core.SourceKind) (*Upload, error) {
	switch {
	case isMagnet(link):
		if only == core.SourceNZB {
			return nil, invalid("not_an_nzb", "a magnet link is a torrent, and only NZBs can be added here")
		}
		st, err := s.stageMagnet(ctx, link)
		if err != nil {
			return nil, err
		}
		return &Upload{torrent: st}, nil
	case only == core.SourceNZB:
		st, err := s.fetchNZB(ctx, link)
		if err != nil {
			return nil, err
		}
		return &Upload{nzb: st}, nil
	}
	got, err := s.fetchAny(ctx, link)
	if err != nil {
		return nil, err
	}
	return &Upload{nzb: got.NZB, torrent: got.Torrent}, nil
}
