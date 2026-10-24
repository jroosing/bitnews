package qbit

import (
	"strconv"
	"strings"

	"github.com/jroosing/bitnews/internal/core"
)

// webAPIAtLeast reports whether the dotted version v is at least major.minor.
func webAPIAtLeast(v string, major, minor int) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || ma == major && mi >= minor
}

// version serves app/version: a bare string.
func (c *call) version() { c.text(200, c.h.Version) }

// webAPIVersion serves app/webapiVersion: a bare string.
func (c *call) webAPIVersion() { c.text(200, c.h.WebAPIVersion) }

// preferences is the read-only view of the settings that media managers
// read: the paths, the queueing and the global Share Limit. The fields keep
// qBittorrent's names and JSON types.
type preferences struct {
	SavePath             string  `json:"save_path"`
	TempPathEnabled      bool    `json:"temp_path_enabled"`
	TempPath             string  `json:"temp_path"`
	AutoTMMEnabled       bool    `json:"auto_tmm_enabled"`
	CreateSubfolder      bool    `json:"create_subfolder_enabled"`
	StartPausedEnabled   bool    `json:"start_paused_enabled"`
	QueueingEnabled      bool    `json:"queueing_enabled"`
	MaxActiveDownloads   int     `json:"max_active_downloads"`
	MaxActiveUploads     int     `json:"max_active_uploads"`
	MaxActiveTorrents    int     `json:"max_active_torrents"`
	MaxRatioEnabled      bool    `json:"max_ratio_enabled"`
	MaxRatio             float64 `json:"max_ratio"`
	MaxRatioAct          int     `json:"max_ratio_act"`
	MaxSeedingTimeOn     bool    `json:"max_seeding_time_enabled"`
	MaxSeedingTime       int     `json:"max_seeding_time"`
	MaxInactiveSeedingOn bool    `json:"max_inactive_seeding_time_enabled"`
	MaxInactiveSeeding   int     `json:"max_inactive_seeding_time"`
	DHT                  bool    `json:"dht"`
	PeX                  bool    `json:"pex"`
	LSD                  bool    `json:"lsd"`
	DLLimit              int     `json:"dl_limit"`
	ULLimit              int     `json:"up_limit"`
	Encryption           int     `json:"encryption"`
	ListenPort           int     `json:"listen_port"`
	AnonymousMode        bool    `json:"anonymous_mode"`
	WebUIUsername        string  `json:"web_ui_username"`
}

// qBittorrent's max_ratio_act values.
const (
	ratioActPause  = 0
	ratioActRemove = 1
	ratioActFiles  = 3
)

// encryptionOf maps the MSE policy onto qBittorrent's encryption setting:
// 0 prefer, 1 force on, 2 force off.
func encryptionOf(p core.MSEPolicy) int {
	switch p {
	case core.MSERequire:
		return 1
	case core.MSEDisable:
		return 2
	}
	return 0
}

func (h *handler) preferences() preferences {
	s := h.Settings.Current()
	p := preferences{
		SavePath: s.Paths.Torrent.Complete, TempPathEnabled: s.Paths.Torrent.Incomplete != s.Paths.Torrent.Complete,
		TempPath: s.Paths.Torrent.Incomplete, CreateSubfolder: true, QueueingEnabled: true,
		MaxActiveDownloads: s.Queues.Torrent, MaxActiveUploads: 0, MaxActiveTorrents: 0,
		MaxRatio: -1, MaxSeedingTime: -1, MaxInactiveSeeding: -1,
		DHT: s.Torrent.DHTEnabled(), PeX: s.Torrent.PEXEnabled(), LSD: s.Torrent.LSD,
		Encryption: encryptionOf(s.Torrent.EffectiveMSE()), ListenPort: s.Torrent.EffectivePort(),
	}
	if h.GlobalShare != nil {
		g := h.GlobalShare()
		p.MaxRatioEnabled, p.MaxSeedingTimeOn = g.RatioEnabled, g.SeedingTimeEnabled
		if g.RatioEnabled {
			p.MaxRatio = g.Ratio
		}
		if g.SeedingTimeEnabled {
			p.MaxSeedingTime = g.SeedingMinutes
		}
		switch g.Action {
		case "remove":
			p.MaxRatioAct = ratioActRemove
		case "remove_files":
			p.MaxRatioAct = ratioActFiles
		default:
			p.MaxRatioAct = ratioActPause
		}
	}
	return p
}

// getPreferences serves app/preferences.
func (c *call) getPreferences() { c.json(c.h.preferences()) }

// setPreferences serves app/setPreferences. The settings of bitnews are not
// changed through the adapter: the call is accepted and ignored,
// so a tool that sends it does not break.
func (c *call) setPreferences() {
	c.h.log.Debug("qbit: setPreferences ignored")
	c.ok()
}
