package qbit

import (
	"cmp"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// infinity is qBittorrent's eta for "never" (100 days in seconds).
const infinity = 8640000

// torrentInfo is one entry of torrents/info. The field names and JSON types
// are qBittorrent's (4.6): Sonarr and Radarr read a subset into typed
// structs. Fields bitnews cannot know are a neutral value, never absent.
//
// The magnet link and the tracker are never reported: trackers carry
// passkeys.
type torrentInfo struct {
	AddedOn                int64   `json:"added_on"`
	AmountLeft             int64   `json:"amount_left"`
	AutoTMM                bool    `json:"auto_tmm"`
	Availability           float64 `json:"availability"`
	Category               string  `json:"category"`
	Completed              int64   `json:"completed"`
	CompletionOn           int64   `json:"completion_on"`
	ContentPath            string  `json:"content_path"`
	DLLimit                int64   `json:"dl_limit"`
	DLSpeed                int64   `json:"dlspeed"`
	Downloaded             int64   `json:"downloaded"`
	DownloadedSession      int64   `json:"downloaded_session"`
	ETA                    int64   `json:"eta"`
	FirstLastPiecePrio     bool    `json:"f_l_piece_prio"`
	ForceStart             bool    `json:"force_start"`
	Hash                   string  `json:"hash"`
	InactiveSeedingLimit   int64   `json:"inactive_seeding_time_limit"`
	InfohashV1             string  `json:"infohash_v1"`
	InfohashV2             string  `json:"infohash_v2"`
	LastActivity           int64   `json:"last_activity"`
	MagnetURI              string  `json:"magnet_uri"`
	MaxInactiveSeedingTime int64   `json:"max_inactive_seeding_time"`
	MaxRatio               float64 `json:"max_ratio"`
	MaxSeedingTime         int64   `json:"max_seeding_time"`
	Name                   string  `json:"name"`
	NumComplete            int     `json:"num_complete"`
	NumIncomplete          int     `json:"num_incomplete"`
	NumLeechs              int     `json:"num_leechs"`
	NumSeeds               int     `json:"num_seeds"`
	Priority               int     `json:"priority"`
	Progress               float64 `json:"progress"`
	Ratio                  float64 `json:"ratio"`
	RatioLimit             float64 `json:"ratio_limit"`
	Reannounce             int64   `json:"reannounce"`
	SavePath               string  `json:"save_path"`
	SeedingTime            int64   `json:"seeding_time"`
	SeedingTimeLimit       int64   `json:"seeding_time_limit"`
	SeenComplete           int64   `json:"seen_complete"`
	SequentialDownload     bool    `json:"seq_dl"`
	Size                   int64   `json:"size"`
	State                  string  `json:"state"`
	SuperSeeding           bool    `json:"super_seeding"`
	Tags                   string  `json:"tags"`
	TimeActive             int64   `json:"time_active"`
	TotalSize              int64   `json:"total_size"`
	Tracker                string  `json:"tracker"`
	TrackersCount          int     `json:"trackers_count"`
	UPLimit                int64   `json:"up_limit"`
	Uploaded               int64   `json:"uploaded"`
	UploadedSession        int64   `json:"uploaded_session"`
	UPSpeed                int64   `json:"upspeed"`
}

// entry is a torrent Download with its view and what the view does not
// hold.
type entry struct {
	d core.Download
	v view.View
	t Torrent
	// hash is the info hash, from the view.
	hash string
}

// catNames maps Category IDs to names.
type catNames map[core.ID]string

func (c *call) categoryNames() (catNames, []core.Category, error) {
	cats, err := c.h.Categories.List(c.ctx())
	if err != nil {
		return nil, nil, err
	}
	m := make(catNames, len(cats))
	for _, cat := range cats {
		m[cat.ID] = cat.Name
	}
	return m, cats, nil
}

// loadAll reads every torrent Download. A Download without a torrent (no
// valid hash) is left out, and so is one whose torrent cannot be described
// now.
func (c *call) loadAll() ([]entry, error) {
	var out []entry
	for after := ""; ; {
		page, err := c.h.Downloads.List(c.ctx(), listFilter(after))
		if err != nil {
			return nil, err
		}
		for _, d := range page.Items {
			e, ok, err := c.entryOf(d, false)
			if err != nil {
				c.h.log.Debug("qbit: torrent skipped", "id", d.ID, "err", err)
				continue
			}
			if ok {
				out = append(out, e)
			}
		}
		if page.Next == "" {
			return out, nil
		}
		after = page.Next
	}
}

// entryOf describes the torrent of d; false means d has none. full is passed
// to the TorrentSource.
func (c *call) entryOf(d core.Download, full bool) (entry, bool, error) {
	t, ok, err := c.h.Torrents.Torrent(c.ctx(), d, full)
	if err != nil || !ok {
		return entry{}, false, err
	}
	v := c.h.View.Of(c.ctx(), d)
	if v.Torrent == nil || !validHash(v.Torrent.Hash) {
		return entry{}, false, nil
	}
	return entry{d: d, v: v, t: t, hash: v.Torrent.Hash}, true, nil
}

// loadHashes reads the named torrents; unknown hashes are skipped.
func (c *call) loadHashes(hashes []string) ([]entry, error) {
	out := make([]entry, 0, len(hashes))
	for _, h := range hashes {
		e, ok, err := c.loadOne(h)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func (c *call) loadOne(hash string) (entry, bool, error) {
	d, ok, err := c.h.Torrents.Lookup(c.ctx(), hash)
	if err != nil || !ok {
		return entry{}, false, err
	}
	if d.SourceKind != core.SourceTorrent {
		return entry{}, false, nil
	}
	return c.entryOf(d, true)
}

// targets are the torrents a hashes parameter names: everything for "all".
func (c *call) targets() ([]entry, bool) {
	l, ok := parseHashes(c.params.Get("hashes"))
	if !ok {
		c.text(400, "Bad Request")
		return nil, false
	}
	var (
		es  []entry
		err error
	)
	switch {
	case l.all:
		es, err = c.loadAll()
	default:
		es, err = c.loadHashes(l.hashes)
	}
	if err != nil {
		c.appFail(err)
		return nil, false
	}
	return es, true
}

func validHash(h string) bool {
	_, err := core.ParseInfoHash(h)
	return err == nil && len(h) == 40
}

// infoOf builds the torrents/info entry of a torrent from its view.
func (h *handler) infoOf(e entry, names catNames, paths core.Paths) torrentInfo {
	d, v, tv := e.d, e.v, e.v.Torrent
	state, progress := torrentState(v, h.stopped)
	size, done := max(v.Total, 0), max(v.Done, 0)
	isReady := d.State == core.StateReady
	switch {
	case isReady:
		done = size
	case size > 0:
		done = min(done, size-1) // only Ready is complete
	default:
		done = 0
	}
	content, save := contentAndSavePath(d, e.t, paths)
	ratio, seedLimit := limitsOf(tv.ShareLimit)
	completion := int64(-1)
	if isReady {
		at := d.FetchedAt
		if at.IsZero() {
			at = d.ReadyAt
		}
		if !at.IsZero() {
			completion = at.Unix()
		}
	}
	name := e.t.Name
	if name == "" {
		name = d.Name
	}
	priority := 0
	if v.InQueue {
		priority = max(d.QueuePosition, 0) + 1 // qBittorrent counts from 1
	}
	eta := int64(infinity)
	if v.ETAKnown {
		eta = min(int64(v.ETA/time.Second), infinity)
	}
	secs := int64(tv.SeedingTime / time.Second)
	return torrentInfo{
		AddedOn: d.AddedAt.Unix(), AmountLeft: size - done, Availability: -1,
		Category:  names[d.CategoryID],
		Completed: done, CompletionOn: completion, ContentPath: content,
		DLLimit: -1, DLSpeed: max(v.DownRate, 0), Downloaded: done, DownloadedSession: done, ETA: eta,
		ForceStart: d.Priority == core.DownloadPriorityForce, Hash: e.hash,
		InactiveSeedingLimit: -2, InfohashV1: e.hash, LastActivity: d.UpdatedAt.Unix(),
		MaxInactiveSeedingTime: -2, MaxRatio: ratio, MaxSeedingTime: seedLimit,
		Name: name, NumComplete: -1, NumIncomplete: -1, NumLeechs: tv.Peers, NumSeeds: tv.Seeds,
		Priority: priority, Progress: progress, Ratio: max(tv.Ratio, 0),
		RatioLimit: ratio, SavePath: save, SeedingTime: secs, SeedingTimeLimit: seedLimit,
		Size: size, State: state, TimeActive: secs, TotalSize: size,
		UPLimit: -1, Uploaded: max(tv.Uploaded, 0), UploadedSession: max(tv.Uploaded, 0), UPSpeed: max(v.UpRate, 0),
	}
}

// limitsOf converts the Share Limit in force to qBittorrent's ratio_limit
// (-2 global, -1 none) and seeding_time_limit (minutes, rounded up, same
// codes).
func limitsOf(l view.ShareLimit) (ratio float64, minutes int64) {
	if l.Global {
		return -2, -2
	}
	ratio, minutes = -1, -1
	if l.Ratio >= 0 {
		ratio = l.Ratio
	}
	if l.Seeding >= 0 {
		minutes = (int64(l.Seeding/time.Second) + 59) / 60
	}
	return ratio, minutes
}

// contentAndSavePath are the content_path and save_path of a torrent. The
// content path of a Ready Download is its output dir, which media managers
// scan recursively. Without paths from the engine the torrent roots stand in.
func contentAndSavePath(d core.Download, t Torrent, paths core.Paths) (content, save string) {
	content, save = t.ContentPath, t.SavePath
	if content == "" && d.State == core.StateReady {
		content = d.OutputPath
	}
	if save == "" && content != "" {
		save = filepath.Dir(content)
	}
	if save == "" {
		save = paths.Torrent.Incomplete
	}
	if content == "" {
		name := t.Name
		if name == "" {
			name = d.Name
		}
		content = filepath.Join(save, name)
	}
	return content, save
}

// info serves torrents/info.
func (c *call) info() {
	infos, ok := c.infos()
	if !ok {
		return
	}
	c.json(infos)
}

// infos builds the filtered, sorted and paged list of torrents/info.
func (c *call) infos() ([]torrentInfo, bool) {
	l, ok := parseHashes(c.params.Get("hashes"))
	if !ok {
		c.text(400, "Bad Request")
		return nil, false
	}
	var (
		es  []entry
		err error
	)
	if l.given && !l.all {
		es, err = c.loadHashes(l.hashes)
	} else {
		es, err = c.loadAll()
	}
	if err != nil {
		c.appFail(err)
		return nil, false
	}
	names, cats, err := c.categoryNames()
	if err != nil {
		c.appFail(err)
		return nil, false
	}
	if _, has := c.params["category"]; has {
		es = filterCategory(es, c.get("category"), cats)
	}
	paths := c.h.Settings.Current().Paths
	infos := make([]torrentInfo, 0, len(es))
	for _, e := range es {
		infos = append(infos, c.h.infoOf(e, names, paths))
	}
	if f := strings.ToLower(c.get("filter")); f != "" && f != "all" {
		infos = slices.DeleteFunc(infos, func(t torrentInfo) bool { return !matchesFilter(f, t) })
	}
	sortInfos(infos, c.get("sort"), isTrue(c.get("reverse")))
	return pageOf(infos, c.get("offset"), c.get("limit")), true
}

// filterCategory keeps the torrents of the category; "" is the ones without.
// Names match ignoring case.
func filterCategory(es []entry, name string, cats []core.Category) []entry {
	var want core.ID
	if name != "" {
		cat, ok := findCategory(cats, name)
		if !ok {
			return nil
		}
		want = cat.ID
	}
	return slices.DeleteFunc(es, func(e entry) bool { return e.d.CategoryID != want })
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// matchesFilter applies qBittorrent's filter names. "completed" and
// "seeding" never match the checkingUP hold-off state, so a torrent that is
// not Ready is never listed as completed.
func matchesFilter(f string, t torrentInfo) bool {
	s := t.State
	switch f {
	case "downloading":
		return slices.Contains([]string{stateDownloading, stateMetaDL, stateStalledDL, stateQueuedDL, statePausedDL, stateStoppedDL, stateForcedDL}, s)
	case "seeding":
		return isCompleteState(s) && s != statePausedUP && s != stateStoppedUP
	case "completed":
		return isCompleteState(s)
	case "paused", "stopped":
		return slices.Contains([]string{statePausedDL, stateStoppedDL, statePausedUP, stateStoppedUP}, s)
	case "resumed", "running":
		return !slices.Contains([]string{statePausedDL, stateStoppedDL, statePausedUP, stateStoppedUP}, s)
	case "active":
		return t.DLSpeed > 0 || t.UPSpeed > 0
	case "inactive":
		return t.DLSpeed == 0 && t.UPSpeed == 0
	case "stalled":
		return s == stateStalledDL || s == stateStalledUP
	case "stalled_uploading":
		return s == stateStalledUP
	case "stalled_downloading":
		return s == stateStalledDL
	case "errored":
		return s == stateError
	}
	return true // an unknown filter filters nothing
}

// sortInfos orders the list by a qBittorrent field name. The default and the
// tie-break are the order of adding, then the hash.
func sortInfos(infos []torrentInfo, key string, reverse bool) {
	base := func(a, b torrentInfo) int {
		return cmp.Or(cmp.Compare(a.AddedOn, b.AddedOn), strings.Compare(a.Hash, b.Hash))
	}
	by := base
	switch key {
	case "name":
		by = func(a, b torrentInfo) int {
			return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), base(a, b))
		}
	case "hash":
		by = func(a, b torrentInfo) int { return strings.Compare(a.Hash, b.Hash) }
	case "size", "total_size":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.Size, b.Size), base(a, b)) }
	case "progress":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.Progress, b.Progress), base(a, b)) }
	case "state":
		by = func(a, b torrentInfo) int { return cmp.Or(strings.Compare(a.State, b.State), base(a, b)) }
	case "category":
		by = func(a, b torrentInfo) int { return cmp.Or(strings.Compare(a.Category, b.Category), base(a, b)) }
	case "priority":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.Priority, b.Priority), base(a, b)) }
	case "dlspeed":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.DLSpeed, b.DLSpeed), base(a, b)) }
	case "upspeed":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.UPSpeed, b.UPSpeed), base(a, b)) }
	case "ratio":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.Ratio, b.Ratio), base(a, b)) }
	case "eta":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.ETA, b.ETA), base(a, b)) }
	case "completion_on":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.CompletionOn, b.CompletionOn), base(a, b)) }
	case "uploaded":
		by = func(a, b torrentInfo) int { return cmp.Or(cmp.Compare(a.Uploaded, b.Uploaded), base(a, b)) }
	}
	slices.SortStableFunc(infos, func(a, b torrentInfo) int {
		if reverse {
			return by(b, a)
		}
		return by(a, b)
	})
}

// pageOf applies offset (negative counts from the end) and limit (zero or
// less is everything).
func pageOf(infos []torrentInfo, offset, limit string) []torrentInfo {
	off, _ := strconv.Atoi(offset)
	lim, _ := strconv.Atoi(limit)
	n := len(infos)
	if off < 0 {
		off = max(n+off, 0)
	}
	off = min(off, n)
	infos = infos[off:]
	if lim > 0 && lim < len(infos) {
		infos = infos[:lim]
	}
	return infos
}

// fileName is the path of a torrent file as qBittorrent shows it.
func fileName(s string) string { return strings.TrimLeft(path.Clean("/"+s), "/") }
