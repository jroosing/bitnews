package qbit

import (
	"strconv"
	"strings"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

func listFilter(after string) app.ListFilter {
	return app.ListFilter{Kind: core.SourceTorrent, After: after, Limit: pageSize}
}

// hashParam reads the hash parameter of properties and files: one valid hash.
func (c *call) hashParam() (string, bool) {
	l, ok := parseHashes(c.params.Get("hash"))
	if !ok || len(l.hashes) != 1 || l.all {
		c.text(404, "Not Found")
		return "", false
	}
	return l.hashes[0], true
}

// properties is the answer of torrents/properties. What bitnews cannot know
// (piece counts, creation date) is -1 like qBittorrent's "unknown".
type properties struct {
	Hash                   string  `json:"hash"`
	SavePath               string  `json:"save_path"`
	CreationDate           int64   `json:"creation_date"`
	PieceSize              int64   `json:"piece_size"`
	Comment                string  `json:"comment"`
	TotalWasted            int64   `json:"total_wasted"`
	TotalUploaded          int64   `json:"total_uploaded"`
	TotalUploadedSession   int64   `json:"total_uploaded_session"`
	TotalDownloaded        int64   `json:"total_downloaded"`
	TotalDownloadedSession int64   `json:"total_downloaded_session"`
	UpLimit                int64   `json:"up_limit"`
	DLLimit                int64   `json:"dl_limit"`
	TimeElapsed            int64   `json:"time_elapsed"`
	SeedingTime            int64   `json:"seeding_time"`
	NbConnections          int     `json:"nb_connections"`
	NbConnectionsLimit     int     `json:"nb_connections_limit"`
	ShareRatio             float64 `json:"share_ratio"`
	AdditionDate           int64   `json:"addition_date"`
	CompletionDate         int64   `json:"completion_date"`
	CreatedBy              string  `json:"created_by"`
	DLSpeedAvg             int64   `json:"dl_speed_avg"`
	DLSpeed                int64   `json:"dl_speed"`
	ETA                    int64   `json:"eta"`
	LastSeen               int64   `json:"last_seen"`
	Peers                  int     `json:"peers"`
	PeersTotal             int     `json:"peers_total"`
	PiecesHave             int     `json:"pieces_have"`
	PiecesNum              int     `json:"pieces_num"`
	Reannounce             int64   `json:"reannounce"`
	Seeds                  int     `json:"seeds"`
	SeedsTotal             int     `json:"seeds_total"`
	TotalSize              int64   `json:"total_size"`
	UpSpeedAvg             int64   `json:"up_speed_avg"`
	UpSpeed                int64   `json:"up_speed"`
	IsPrivate              bool    `json:"is_private"`
	IsPrivateLegacy        bool    `json:"isPrivate"`
}

// properties serves torrents/properties?hash=: 404 for an unknown torrent.
func (c *call) properties() {
	hash, ok := c.hashParam()
	if !ok {
		return
	}
	e, found, err := c.loadOne(hash)
	if err != nil {
		c.appFail(err)
		return
	}
	if !found {
		c.text(404, "Not Found")
		return
	}
	names, _, err := c.categoryNames()
	if err != nil {
		c.appFail(err)
		return
	}
	t := c.h.infoOf(e, names, c.h.Settings.Current().Paths)
	c.json(properties{
		Hash: t.Hash, SavePath: t.SavePath, CreationDate: -1, PieceSize: -1,
		TotalUploaded: t.Uploaded, TotalUploadedSession: t.Uploaded, TotalDownloaded: t.Downloaded,
		TotalDownloadedSession: t.Downloaded, UpLimit: -1, DLLimit: -1, TimeElapsed: t.TimeActive,
		SeedingTime: t.SeedingTime, NbConnections: t.NumSeeds + t.NumLeechs, NbConnectionsLimit: -1,
		ShareRatio: t.Ratio, AdditionDate: t.AddedOn, CompletionDate: t.CompletionOn,
		DLSpeed: t.DLSpeed, ETA: t.ETA, LastSeen: -1, Peers: t.NumLeechs, PeersTotal: -1,
		PiecesHave: -1, PiecesNum: -1, Seeds: t.NumSeeds, SeedsTotal: -1, TotalSize: t.Size,
		UpSpeed: t.UPSpeed, IsPrivate: e.v.Torrent.Private, IsPrivateLegacy: e.v.Torrent.Private,
	})
}

// fileInfo is one entry of torrents/files.
type fileInfo struct {
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	Priority     int     `json:"priority"`
	IsSeed       bool    `json:"is_seed"`
	PieceRange   [2]int  `json:"piece_range"`
	Availability float64 `json:"availability"`
}

// files serves torrents/files?hash=: 404 for an unknown torrent; an empty
// list while the engine cannot list files (no metadata yet).
// The engine knows no progress per file, so every file shows the progress of
// the whole torrent.
func (c *call) files() {
	hash, ok := c.hashParam()
	if !ok {
		return
	}
	e, found, err := c.loadOne(hash)
	if err != nil {
		c.appFail(err)
		return
	}
	if !found {
		c.text(404, "Not Found")
		return
	}
	out := []fileInfo{}
	ready := e.d.State == core.StateReady
	p := min(max(e.v.Progress, 0), 1)
	if !ready {
		p = min(p, holdOffProgress) // only Ready is complete
	}
	for i, f := range e.v.Torrent.Files {
		out = append(out, fileInfo{
			Index: i, Name: fileName(f.Path), Size: f.Size, Progress: p, Priority: 1,
			IsSeed: ready && p == 1, Availability: -1,
		})
	}
	c.json(out)
}

// tags serves torrents/tags: tags are not supported, so there are none.
func (c *call) tags() { c.json([]string{}) }

// maindata serves sync/maindata as a full update: every torrent, the
// Categories and the transfer totals. There is no incremental sync, so a
// client that sends rid gets the whole state again with the next rid.
func (c *call) maindata() {
	es, err := c.loadAll()
	if err != nil {
		c.appFail(err)
		return
	}
	names, cats, err := c.categoryNames()
	if err != nil {
		c.appFail(err)
		return
	}
	paths := c.h.Settings.Current().Paths
	torrents := make(map[string]torrentInfo, len(es))
	var dl, up int64
	for _, e := range es {
		t := c.h.infoOf(e, names, paths)
		torrents[t.Hash] = t
		dl += t.DLSpeed
		up += t.UPSpeed
	}
	rid, _ := strconv.Atoi(strings.TrimSpace(c.params.Get("rid")))
	c.json(map[string]any{
		"rid": max(rid, 0) + 1, "full_update": true,
		"torrents":   torrents,
		"categories": c.h.categoryMap(cats),
		"tags":       []string{},
		"trackers":   map[string][]string{},
		"server_state": map[string]any{
			"connection_status": "connected", "dl_info_speed": dl, "up_info_speed": up,
			"free_space_on_disk": 0, "queueing": true, "use_alt_speed_limits": false,
		},
	})
}
