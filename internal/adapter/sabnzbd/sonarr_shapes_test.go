package sabnzbd_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// The structs below mirror the classes Sonarr and Radarr deserialize SAB's
// answers into (SabnzbdQueue, SabnzbdHistory, SabnzbdConfig, ...), as far as
// the author remembers them: NOT copied from their source, which is to be
// checked against the recordings of M4-01. Newtonsoft.Json converts numeric
// strings to numbers on its own; Go's ",string" option does the same for
// these tests, so a field our adapter sends as a non-numeric string fails to
// decode here just as it would fail in Sonarr.

type sonarrQueueResponse struct {
	Queue struct {
		Paused bool              `json:"paused"`
		Items  []sonarrQueueItem `json:"slots"`
	} `json:"queue"`
}

type sonarrQueueItem struct {
	Status     string  `json:"status"`
	Index      int     `json:"index"`
	Timeleft   string  `json:"timeleft"`
	Priority   string  `json:"priority"`
	Size       float64 `json:"mb,string"`
	Sizeleft   float64 `json:"mbleft,string"`
	Filename   string  `json:"filename"`
	Cat        string  `json:"cat"`
	ID         string  `json:"nzo_id"`
	Percentage int     `json:"percentage,string"`
}

type sonarrHistoryResponse struct {
	History struct {
		Total int                 `json:"noofslots"`
		Items []sonarrHistoryItem `json:"slots"`
	} `json:"history"`
}

type sonarrHistoryItem struct {
	FailMessage  string `json:"fail_message"`
	Size         int64  `json:"bytes"`
	Category     string `json:"category"`
	DownloadTime int    `json:"download_time"`
	Storage      string `json:"storage"`
	Status       string `json:"status"`
	ID           string `json:"nzo_id"`
	Title        string `json:"name"`
	NzbName      string `json:"nzb_name"`
}

type sonarrAddResponse struct {
	Status bool     `json:"status"`
	IDs    []string `json:"nzo_ids"`
}

type sonarrConfigResponse struct {
	Config struct {
		Misc struct {
			CompleteDir        string   `json:"complete_dir"`
			EnableTVSorting    bool     `json:"enable_tv_sorting"`
			EnableMovieSorting bool     `json:"enable_movie_sorting"`
			EnableDateSorting  bool     `json:"enable_date_sorting"`
			TVCategories       []string `json:"tv_categories"`
			PreCheck           bool     `json:"pre_check"`
			HistoryRetention   string   `json:"history_retention"`
		} `json:"misc"`
		Categories []struct {
			Name     string `json:"name"`
			Order    int    `json:"order"`
			PP       string `json:"pp"`
			Script   string `json:"script"`
			Dir      string `json:"dir"`
			Newzbin  string `json:"newzbin"`
			Priority int    `json:"priority"`
		} `json:"categories"`
	} `json:"config"`
}

type sonarrFullStatusResponse struct {
	Status struct {
		CompleteDir string `json:"completedir"`
	} `json:"status"`
}

// parseTimeSpan reads what .NET's TimeSpan.Parse accepts of SAB's timeleft:
// "H:MM:SS" and "D:HH:MM:SS".
func parseTimeSpan(t *testing.T, s string) time.Duration {
	t.Helper()
	parts := strings.Split(s, ":")
	require.Contains(t, []int{3, 4}, len(parts), "timeleft %q", s)
	var n []int
	for _, p := range parts {
		v, err := strconv.Atoi(p)
		require.NoError(t, err, "timeleft %q", s)
		n = append(n, v)
	}
	if len(n) == 3 {
		n = append([]int{0}, n...)
	}
	return time.Duration(n[0])*24*time.Hour + time.Duration(n[1])*time.Hour + time.Duration(n[2])*time.Minute + time.Duration(n[3])*time.Second
}

func TestSonarrCanDecodeTheQueue(t *testing.T) {
	s := newStack(t)
	tv := s.category("tv")
	s.seed("Show.S01E01", inState(core.StateFetching), sized(3<<30, 1<<30), inCategory(tv.ID))
	s.seed("Show.S01E02", sized(2<<30, 0), inCategory(tv.ID))
	s.seed("Zero.Size")
	s.setSpeed(10<<20, false)

	var q sonarrQueueResponse
	body(t, s.call("mode=queue&start=0&limit=30"), &q)

	require.Len(t, q.Queue.Items, 3)
	first := q.Queue.Items[0]
	assert.Equal(t, "Downloading", first.Status)
	assert.InDelta(t, 3072, first.Size, 0.01)
	assert.InDelta(t, 2048, first.Sizeleft, 0.01)
	assert.Equal(t, 33, first.Percentage)
	assert.Equal(t, "Normal", first.Priority)
	assert.Equal(t, 204*time.Second, parseTimeSpan(t, first.Timeleft))
	for _, item := range q.Queue.Items {
		parseTimeSpan(t, item.Timeleft)
		assert.NotEmpty(t, item.ID)
	}
}

func TestSonarrCanDecodeALongTimeLeft(t *testing.T) {
	s := newStack(t)
	s.seed("Huge", inState(core.StateFetching), sized(100<<30, 0))
	s.setSpeed(100<<10, false) // 100 KiB/s

	var q sonarrQueueResponse
	body(t, s.call("mode=queue"), &q)

	require.Len(t, q.Queue.Items, 1)
	assert.Greater(t, parseTimeSpan(t, q.Queue.Items[0].Timeleft), 24*time.Hour)
}

func TestSonarrCanDecodeTheHistory(t *testing.T) {
	s := newStack(t)
	tv := s.category("tv")
	s.seed("Good", ready("/data/usenet/complete/tv/Good"), sized(1<<30, 1<<30), inCategory(tv.ID))
	s.seed("Bad", failed(core.FailureRepairInsufficient, "short by 12 blocks"), inCategory(tv.ID))
	pp := s.seed("Working", inState(core.StatePostProcessing), inCategory(tv.ID))
	s.setStage(pp.ID, core.StageRepairing)

	var h sonarrHistoryResponse
	body(t, s.call("mode=history&start=0&limit=60&category=tv"), &h)

	require.Len(t, h.History.Items, 3)
	assert.Equal(t, 3, h.History.Total)
	byTitle := map[string]sonarrHistoryItem{}
	for _, item := range h.History.Items {
		byTitle[item.Title] = item
	}
	assert.Equal(t, "Completed", byTitle["Good"].Status)
	assert.Equal(t, "/data/usenet/complete/tv/Good", byTitle["Good"].Storage)
	assert.EqualValues(t, 1<<30, byTitle["Good"].Size)
	assert.Equal(t, "Failed", byTitle["Bad"].Status)
	assert.Contains(t, byTitle["Bad"].FailMessage, "short by 12 blocks")
	assert.Equal(t, "Repairing", byTitle["Working"].Status)
	assert.Equal(t, "Good.nzb", byTitle["Good"].NzbName)
}

func TestSonarrCanDecodeAddAndVersionAndConfigAndFullStatus(t *testing.T) {
	s := newStack(t)
	s.category("tv")

	var add sonarrAddResponse
	body(t, s.upload("mode=addfile&cat=tv&priority=-100", "x.nzb", plainNZB), &add)
	assert.True(t, add.Status)
	require.Len(t, add.IDs, 1)

	var version struct {
		Version string `json:"version"`
	}
	body(t, s.callWith("", "mode=version"), &version)
	parts := strings.Split(version.Version, ".")
	require.Len(t, parts, 3, "x.y.z, which Sonarr and Radarr parse as a version")
	major, err := strconv.Atoi(parts[0])
	require.NoError(t, err)
	assert.GreaterOrEqual(t, major, 3, "Sonarr needs 3.0.0, Radarr 2.0.0")

	var cfg sonarrConfigResponse
	body(t, s.call("mode=get_config"), &cfg)
	assert.Equal(t, s.complete, cfg.Config.Misc.CompleteDir)
	assert.False(t, cfg.Config.Misc.EnableTVSorting || cfg.Config.Misc.EnableMovieSorting || cfg.Config.Misc.EnableDateSorting)
	require.Len(t, cfg.Config.Categories, 2)
	assert.Equal(t, "tv", cfg.Config.Categories[1].Name)
	assert.Equal(t, "tv", cfg.Config.Categories[1].Dir)

	var full sonarrFullStatusResponse
	body(t, s.call("mode=fullstatus&skip_dashboard=1"), &full)
	assert.Equal(t, s.complete, full.Status.CompleteDir)
}

func TestSonarrCanDecodeAnError(t *testing.T) {
	s := newStack(t)

	var e struct {
		Status bool   `json:"status"`
		Error  string `json:"error"`
	}
	body(t, s.callWith("", "mode=queue"), &e)

	assert.False(t, e.Status)
	assert.Equal(t, "API Key Required", e.Error)
}
