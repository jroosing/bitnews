package qbit_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/qbit"
	"github.com/jroosing/bitnews/internal/core"
)

// These structs mirror what Sonarr and Radarr deserialize: the C# classes
// QBittorrentTorrent, QBittorrentTorrentProperties, QBittorrentTorrentFile
// and QBittorrentPreferences, with the JSON types of their members (long is
// an integer, double a float). Go's decoder refuses a string where a number
// is expected and a fraction where an integer is, as Newtonsoft.Json does, so
// decoding the adapter's answers into them is a check of the types. They were
// written from memory of those classes; the recording issue (M5-09) was
// dropped, so real use is what checks them.

type sonarrTorrent struct {
	Hash             string  `json:"hash"`
	Name             string  `json:"name"`
	Size             int64   `json:"size"`
	Progress         float64 `json:"progress"`
	Eta              int64   `json:"eta"`
	State            string  `json:"state"`
	Label            string  `json:"label"`
	Category         string  `json:"category"`
	SavePath         string  `json:"save_path"`
	ContentPath      string  `json:"content_path"`
	Ratio            float64 `json:"ratio"`
	RatioLimit       float64 `json:"ratio_limit"`
	SeedingTimeLimit int64   `json:"seeding_time_limit"`
	SeedingTime      int64   `json:"seeding_time"`
	Tags             string  `json:"tags"`
	AddedOn          int64   `json:"added_on"`
	CompletionOn     int64   `json:"completion_on"`
	DlSpeed          int64   `json:"dlspeed"`
	UpSpeed          int64   `json:"upspeed"`
	AmountLeft       int64   `json:"amount_left"`
	Priority         int64   `json:"priority"`
	NumSeeds         int64   `json:"num_seeds"`
	NumLeechs        int64   `json:"num_leechs"`
}

type sonarrProperties struct {
	Hash         string  `json:"hash"`
	SavePath     string  `json:"save_path"`
	SeedingTime  int64   `json:"seeding_time"`
	ShareRatio   float64 `json:"share_ratio"`
	TotalSize    int64   `json:"total_size"`
	IsPrivate    bool    `json:"is_private"`
	AdditionDate int64   `json:"addition_date"`
}

type sonarrFile struct {
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Priority int64   `json:"priority"`
	IsSeed   bool    `json:"is_seed"`
}

type sonarrPreferences struct {
	SavePath           string  `json:"save_path"`
	DhtEnabled         bool    `json:"dht"`
	QueueingEnabled    bool    `json:"queueing_enabled"`
	MaxRatioEnabled    bool    `json:"max_ratio_enabled"`
	MaxRatio           float64 `json:"max_ratio"`
	MaxRatioAct        int64   `json:"max_ratio_act"`
	MaxSeedingTimeOn   bool    `json:"max_seeding_time_enabled"`
	MaxSeedingTime     int64   `json:"max_seeding_time"`
	MaxActiveDownloads int64   `json:"max_active_downloads"`
}

type sonarrCategory struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

func TestSonarrAndRadarrCanDecodeEveryAnswer(t *testing.T) {
	t.Parallel()
	s := newStack(t, withDeps(func(d *qbit.Deps) {
		d.GlobalShare = func() qbit.GlobalShare {
			return qbit.GlobalShare{RatioEnabled: true, Ratio: 2, SeedingTimeEnabled: true, SeedingMinutes: 60}
		}
	}))
	tv := s.category("tv")
	// A fractional ratio and a limit that is not a whole number of minutes
	// exercise the number types.
	_, seeding := s.seed("Show.S01E01", ready("/data/torrents/tv/Show.S01E01"), sized(1<<40, 1<<40), inCategory(tv.ID),
		func(_ *core.Download, v *torrent) {
			v.UpRate, v.Uploaded, v.SeedingTime = 12345, 1<<41+1<<20, 3*time.Hour // a fractional ratio
			v.OwnRatio, v.OwnSeeding = new(1.7), new(90*time.Second)
			v.Private = true
		})
	s.seed("Show.S01E02", inState(core.StateFetching), sized(1<<30, 1<<20), func(_ *core.Download, v *torrent) { v.DownRate = 3 })
	s.seed("Show.S01E03", inState(core.StateFetchingMetadata))
	s.seed("Show.S01E04", inState(core.StatePostProcessing), sized(1000, 1000))
	s.engine.setFiles(seeding.InfoHash, qbit.TorrentFile{Name: "Show.S01E01/a.mkv", Size: 1 << 40})
	sid := s.login()

	var torrents []sonarrTorrent
	decode(t, s.get(sid, "/api/v2/torrents/info?category=tv"), &torrents)
	var all []sonarrTorrent
	decode(t, s.get(sid, "/api/v2/torrents/info"), &all)
	var props sonarrProperties
	decode(t, s.get(sid, "/api/v2/torrents/properties?hash="+seeding.InfoHash), &props)
	var files []sonarrFile
	decode(t, s.get(sid, "/api/v2/torrents/files?hash="+seeding.InfoHash), &files)
	var prefs sonarrPreferences
	decode(t, s.get(sid, "/api/v2/app/preferences"), &prefs)
	var cats map[string]sonarrCategory
	decode(t, s.get(sid, "/api/v2/torrents/categories"), &cats)

	require.Len(t, torrents, 1)
	assert.Equal(t, seeding.InfoHash, torrents[0].Hash)
	assert.Equal(t, "tv", torrents[0].Category)
	assert.Equal(t, int64(1<<40), torrents[0].Size)
	assert.Equal(t, int64(2), torrents[0].SeedingTimeLimit, "90 s rounds up to 2 minutes")
	assert.Equal(t, 1.7, torrents[0].RatioLimit)
	assert.Equal(t, "/data/torrents/tv/Show.S01E01", torrents[0].ContentPath)
	assert.Len(t, all, 4)
	assert.Equal(t, seeding.InfoHash, props.Hash)
	assert.True(t, props.IsPrivate)
	assert.Equal(t, int64(3*3600), props.SeedingTime)
	require.Len(t, files, 1)
	assert.Equal(t, "Show.S01E01/a.mkv", files[0].Name)
	assert.True(t, prefs.DhtEnabled)
	assert.True(t, prefs.MaxRatioEnabled)
	assert.Equal(t, 2.0, prefs.MaxRatio)
	assert.Equal(t, int64(60), prefs.MaxSeedingTime)
	assert.Equal(t, s.torrents+"/tv", cats["tv"].SavePath)
}

// A media manager decides what to do from state and progress. This re-states
// Sonarr's reading of them: only these states mean "completed" to it.
func TestTheSonarrReadingOfEveryStateWeProduce(t *testing.T) {
	t.Parallel()
	sonarrStatus := func(state string) string {
		switch state {
		case "error":
			return "failed"
		case "pausedDL", "stoppedDL":
			return "paused"
		case "queuedDL", "checkingDL", "checkingUP", "queuedUP", "checkingResumeData":
			return "queued"
		case "pausedUP", "stoppedUP", "uploading", "stalledUP", "forcedUP":
			return "completed"
		case "moving", "downloading", "forcedDL", "stalledDL":
			return "downloading"
		case "metaDL", "forcedMetaDL":
			return "queued-or-warning"
		}
		return "unknown"
	}
	s := newStack(t)
	s.seed("added", inState(core.StateAdded))
	s.seed("meta", inState(core.StateFetchingMetadata))
	s.seed("queued")
	s.seed("fetching", inState(core.StateFetching), func(_ *core.Download, v *torrent) { v.DownRate = 1 })
	s.seed("stalled", inState(core.StateFetching))
	s.seed("fetched", inState(core.StateFetched))
	s.seed("post", inState(core.StatePostProcessing))
	s.seed("ready", ready("/d/r"))
	s.seed("failed", inState(core.StateFailed))
	sid := s.login()

	got := map[string]string{}
	for _, tj := range s.info(sid, "") {
		got[tj.Name] = sonarrStatus(tj.State)
	}

	assert.Equal(t, map[string]string{
		"added": "queued", "meta": "queued-or-warning", "queued": "queued", "fetching": "downloading", "stalled": "downloading",
		"fetched": "queued", "post": "queued", "ready": "completed", "failed": "failed",
	}, got)
}

func TestTheAnswersAreValidJSONWithoutNullsWhereArraysAreExpected(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	for _, target := range []string{"/api/v2/torrents/info", "/api/v2/torrents/tags"} {
		var v []json.RawMessage
		decode(t, s.get(sid, target), &v)
		assert.NotNil(t, v, target)
	}
}
