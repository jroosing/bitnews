package qbit_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

func inCategory(id core.ID) func(*core.Download, *torrent) {
	return func(d *core.Download, _ *torrent) { d.CategoryID = id }
}

func TestInfoOfNothingIsAnEmptyArrayNotNull(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	rec := s.get(sid, "/api/v2/torrents/info")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestInfoListsEveryFieldTheMediaManagersRead(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	tv := s.category("tv")
	d, v := s.seed("Show.S01E01.1080p", ready("/data/torrents/tv/Show.S01E01.1080p"), sized(2000, 2000), inCategory(tv.ID),
		func(d *core.Download, v *torrent) {
			v.UpRate, v.Uploaded, v.SeedingTime = 100, 3000, 90*time.Minute
			v.Seeds, v.Peers = 4, 2
			v.SavePath = "/data/torrents/tv"
			d.FetchedAt = epoch.Add(10 * time.Minute)
			v.OwnRatio, v.OwnSeeding = new(2.0), new(7200*time.Second)
		})
	sid := s.login()

	var raw []map[string]any
	decode(t, s.get(sid, "/api/v2/torrents/info"), &raw)

	require.Len(t, raw, 1)
	m := raw[0]
	for _, key := range []string{
		"hash", "name", "size", "total_size", "progress", "dlspeed", "upspeed", "eta", "state", "category", "tags",
		"save_path", "content_path", "ratio", "ratio_limit", "seeding_time", "seeding_time_limit", "max_ratio",
		"max_seeding_time", "added_on", "completion_on", "amount_left", "completed", "downloaded", "uploaded",
		"num_seeds", "num_leechs", "priority", "force_start", "magnet_uri", "tracker", "infohash_v1",
	} {
		assert.Contains(t, m, key)
	}
	assert.Equal(t, v.InfoHash, m["hash"])
	assert.Equal(t, "Show.S01E01.1080p", m["name"])
	assert.Equal(t, "tv", m["category"])
	assert.Equal(t, "uploading", m["state"])
	assert.EqualValues(t, 1, m["progress"])
	assert.EqualValues(t, 2000, m["size"])
	assert.EqualValues(t, 0, m["amount_left"])
	assert.EqualValues(t, 2000, m["completed"])
	assert.EqualValues(t, 100, m["upspeed"])
	assert.EqualValues(t, 3000, m["uploaded"])
	assert.EqualValues(t, 1.5, m["ratio"], "uploaded over size, from the view")
	assert.EqualValues(t, 2.0, m["ratio_limit"])
	assert.EqualValues(t, 120, m["seeding_time_limit"], "minutes")
	assert.EqualValues(t, 5400, m["seeding_time"], "seconds")
	assert.EqualValues(t, 4, m["num_seeds"])
	assert.EqualValues(t, 2, m["num_leechs"])
	assert.Equal(t, "/data/torrents/tv", m["save_path"])
	assert.Equal(t, "/data/torrents/tv/Show.S01E01.1080p", m["content_path"], "the Download dir")
	assert.EqualValues(t, d.AddedAt.Unix(), m["added_on"])
	assert.EqualValues(t, epoch.Add(10*time.Minute).Unix(), m["completion_on"])
	assert.Equal(t, "", m["magnet_uri"], "magnet links carry passkeys: never shown")
	assert.Equal(t, "", m["tracker"])
	assert.EqualValues(t, 8640000, m["eta"])
}

func TestShareLimitCodes(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("global", ready("/d/g"))
	s.seed("none", ready("/d/n"), func(_ *core.Download, v *torrent) {
		v.OwnRatio, v.OwnSeeding = new(-1.0), new(time.Duration(-1))
	})
	s.seed("odd seconds", ready("/d/o"), func(_ *core.Download, v *torrent) {
		v.OwnRatio, v.OwnSeeding = new(0.5), new(61*time.Second)
	})
	sid := s.login()

	by := map[string]torrentJSON{}
	for _, tj := range s.info(sid, "") {
		by[tj.Name] = tj
	}

	assert.Equal(t, -2.0, by["global"].RatioLimit)
	assert.EqualValues(t, -2, by["global"].SeedingLimit)
	assert.Equal(t, -1.0, by["none"].RatioLimit)
	assert.EqualValues(t, -1, by["none"].SeedingLimit)
	assert.Equal(t, 0.5, by["odd seconds"].RatioLimit)
	assert.EqualValues(t, 2, by["odd seconds"].SeedingLimit, "rounded up to whole minutes")
}

func TestInfoFiltersByCategoryIgnoringCaseAndEmptyMeansUncategorised(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	tv, movies := s.category("tv"), s.category("movies")
	s.seed("a", inCategory(tv.ID))
	s.seed("b", inCategory(movies.ID))
	s.seed("c", inCategory(tv.ID))
	s.seed("d")
	sid := s.login()

	assert.Equal(t, []string{"a", "c"}, names(s.info(sid, "category=tv")))
	assert.Equal(t, []string{"a", "c"}, names(s.info(sid, "category=TV")))
	assert.Equal(t, []string{"d"}, names(s.info(sid, "category=")), "an empty category is the uncategorised")
	assert.Equal(t, []string{"a", "b", "c", "d"}, names(s.info(sid, "")))
	assert.Empty(t, s.info(sid, "category=nope"))
}

func TestInfoFiltersByHashes(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	_, a := s.seed("a")
	_, b := s.seed("b")
	s.seed("c")
	sid := s.login()

	assert.Equal(t, []string{"a", "b"}, names(s.info(sid, "hashes="+a.InfoHash+"|"+b.InfoHash)))
	assert.Equal(t, []string{"b"}, names(s.info(sid, "hashes="+strings.ToUpper(b.InfoHash))), "case does not matter")
	assert.Equal(t, []string{"a", "b", "c"}, names(s.info(sid, "hashes=all")))
	assert.Empty(t, s.info(sid, "hashes="+strings.Repeat("0", 40)))
	assert.Empty(t, s.info(sid, "hashes=zzz|"+url.QueryEscape("../../etc")), "bad hashes match nothing")
	assert.Equal(t, []string{"a", "b", "c"}, names(s.info(sid, "hashes=")), "empty means no filter")
}

func TestInfoRefusesTooManyHashes(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()
	var sb strings.Builder
	for i := range 1001 {
		fmt.Fprintf(&sb, "%040x|", i)
	}

	rec := s.get(sid, "/api/v2/torrents/info?hashes="+sb.String())

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInfoFilters(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("downloading", inState(core.StateFetching), sized(100, 10), func(_ *core.Download, v *torrent) { v.DownRate = 5 })
	s.seed("stalled", inState(core.StateFetching), sized(100, 10))
	s.seed("queued")
	s.seed("paused", func(d *core.Download, _ *torrent) { d.Paused = true })
	s.seed("fetched", inState(core.StateFetched), sized(100, 100))
	s.seed("seeding", ready("/d/s"), func(_ *core.Download, v *torrent) { v.UpRate = 9 })
	s.seed("idle", ready("/d/i"))
	s.seed("limit reached", ready("/d/l"), func(_ *core.Download, v *torrent) { v.Paused = true })
	s.seed("failed", func(d *core.Download, _ *torrent) {
		d.State, d.FailureReason = core.StateFailed, core.FailureArchiveCorrupt
	})
	sid := s.login()

	filter := func(f string) []string { return names(s.info(sid, "filter="+f+"&sort=name")) }

	assert.Equal(t, []string{"downloading", "paused", "queued", "stalled"}, filter("downloading"))
	assert.Equal(t, []string{"idle", "limit reached", "seeding"}, filter("completed"),
		"completed is Ready only: not the checkingUP torrent")
	assert.Equal(t, []string{"idle", "seeding"}, filter("seeding"))
	assert.Equal(t, []string{"limit reached", "paused"}, filter("paused"))
	assert.Equal(t, []string{"limit reached", "paused"}, filter("stopped"))
	assert.ElementsMatch(t, []string{"downloading", "fetched", "failed", "idle", "queued", "seeding", "stalled"}, filter("resumed"))
	assert.Equal(t, []string{"downloading", "seeding"}, filter("active"))
	assert.Equal(t, []string{"idle", "stalled"}, filter("stalled"))
	assert.Equal(t, []string{"idle"}, filter("stalled_uploading"))
	assert.Equal(t, []string{"stalled"}, filter("stalled_downloading"))
	assert.Equal(t, []string{"failed"}, filter("errored"))
	assert.Len(t, filter("all"), 9)
	assert.Len(t, filter("something-new"), 9, "an unknown filter filters nothing")
}

func TestInfoSortsAndPages(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("b", sized(300, 0))
	s.seed("c", sized(100, 0))
	s.seed("a", sized(200, 0))
	s.seed("D", sized(400, 0))
	sid := s.login()

	assert.Equal(t, []string{"b", "c", "a", "D"}, names(s.info(sid, "")), "oldest first by default")
	assert.Equal(t, []string{"a", "b", "c", "D"}, names(s.info(sid, "sort=name")), "ignoring case")
	assert.Equal(t, []string{"D", "c", "b", "a"}, names(s.info(sid, "sort=name&reverse=true")))
	assert.Equal(t, []string{"c", "a", "b", "D"}, names(s.info(sid, "sort=size")))
	assert.Equal(t, []string{"D", "b", "a", "c"}, names(s.info(sid, "sort=size&reverse=true")))
	assert.Equal(t, []string{"a", "b"}, names(s.info(sid, "sort=name&limit=2")))
	assert.Equal(t, []string{"c", "D"}, names(s.info(sid, "sort=name&offset=2")))
	assert.Equal(t, []string{"c"}, names(s.info(sid, "sort=name&offset=2&limit=1")))
	assert.Equal(t, []string{"c", "D"}, names(s.info(sid, "sort=name&offset=-2")), "negative offsets count from the end")
	assert.Empty(t, s.info(sid, "offset=99"))
	assert.Len(t, s.info(sid, "limit=0"), 4, "limit 0 is everything")
	assert.Len(t, s.info(sid, "limit=-1&offset=junk"), 4)
	assert.Equal(t, []string{"b", "c", "a", "D"}, names(s.info(sid, "sort=bogus")))
}

func TestInfoShowsTheEtaOfADownload(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("x", inState(core.StateFetching), sized(1000, 400), func(_ *core.Download, v *torrent) { v.DownRate = 100 })
	sid := s.login()

	got := s.info(sid, "")

	assert.EqualValues(t, 6, got[0].ETA)
	assert.EqualValues(t, 600, got[0].AmountLeft)
}

func TestTorrentDownloadsOnlyNotNZBs(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("tor")
	s.seed("nzb", func(d *core.Download, _ *torrent) { d.SourceKind = core.SourceNZB })
	sid := s.login()

	assert.Equal(t, []string{"tor"}, names(s.info(sid, "")))
}

func TestAViewWithoutADownloadOrWithABadHashIsLeftOut(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("real")
	s.engine.put(torrent{DownloadID: core.NewID(), InfoHash: strings.Repeat("a", 40), Name: "orphan"})
	s.seed("bad hash", func(_ *core.Download, v *torrent) { v.InfoHash = "nothex" })
	sid := s.login()

	assert.Equal(t, []string{"real"}, names(s.info(sid, "")))
}

func TestPathsFallBackToTheTorrentRoots(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("no paths yet")
	s.seed("done without engine paths", ready("/data/torrents/tv/done"))
	sid := s.login()

	by := map[string]torrentJSON{}
	for _, tj := range s.info(sid, "") {
		by[tj.Name] = tj
	}

	assert.Equal(t, s.incomplete, by["no paths yet"].SavePath)
	assert.Equal(t, "/data/torrents/tv/done", by["done without engine paths"].ContentPath)
	assert.Equal(t, "/data/torrents/tv", by["done without engine paths"].SavePath)
}

func TestPropertiesAndFiles(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	_, v := s.seed("rel", inState(core.StateFetched), sized(300, 300), func(_ *core.Download, v *torrent) {
		v.Private, v.SavePath = true, "/data/torrents/tv"
	})
	s.engine.setFiles(v.InfoHash, core.TorrentFile{Path: "rel/a.mkv", Size: 200}, core.TorrentFile{Path: "rel/../b.nfo", Size: 100})
	sid := s.login()

	var props map[string]any
	decode(t, s.get(sid, "/api/v2/torrents/properties?hash="+v.InfoHash), &props)
	var files []map[string]any
	decode(t, s.get(sid, "/api/v2/torrents/files?hash="+v.InfoHash), &files)

	assert.Equal(t, v.InfoHash, props["hash"])
	assert.Equal(t, "/data/torrents/tv", props["save_path"])
	assert.Equal(t, true, props["is_private"])
	assert.EqualValues(t, 300, props["total_size"])
	require.Len(t, files, 2)
	assert.Equal(t, "rel/a.mkv", files[0]["name"])
	assert.EqualValues(t, 0, files[0]["index"])
	assert.Equal(t, "b.nfo", files[1]["name"], "names are cleaned")
	assert.InDelta(t, 0.9999, files[0]["progress"], 1e-9, "no file looks complete before Ready")
	assert.Equal(t, false, files[0]["is_seed"])
}

func TestFilesOfAReadyTorrentAreComplete(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	_, v := s.seed("rel", ready("/d/rel"), sized(100, 100))
	s.engine.setFiles(v.InfoHash, core.TorrentFile{Path: "rel/a.mkv", Size: 100})
	sid := s.login()

	var files []map[string]any
	decode(t, s.get(sid, "/api/v2/torrents/files?hash="+v.InfoHash), &files)

	assert.EqualValues(t, 1, files[0]["progress"])
	assert.Equal(t, true, files[0]["is_seed"])
}

func TestPropertiesAndFilesOfAnUnknownOrBadHashAre404(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("x")
	sid := s.login()

	for _, ep := range []string{"properties", "files"} {
		for _, h := range []string{strings.Repeat("0", 40), "nothex", "", "all", "a|b"} {
			rec := s.get(sid, "/api/v2/torrents/"+ep+"?hash="+h)

			assert.Equal(t, http.StatusNotFound, rec.Code, ep+" "+h)
		}
	}
}

func TestFilesOfATorrentWithoutMetadataIsEmpty(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	_, v := s.seed("x")
	sid := s.login()

	rec := s.get(sid, "/api/v2/torrents/files?hash="+v.InfoHash)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

func TestMaindataIsAFullUpdate(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	tv := s.category("tv")
	_, v := s.seed("x", inCategory(tv.ID), inState(core.StateFetching), func(_ *core.Download, v *torrent) { v.DownRate = 7 })
	sid := s.login()

	var out struct {
		RID        int                    `json:"rid"`
		Full       bool                   `json:"full_update"`
		Torrents   map[string]torrentJSON `json:"torrents"`
		Categories map[string]struct {
			Name     string `json:"name"`
			SavePath string `json:"savePath"`
		} `json:"categories"`
		ServerState struct {
			DLInfoSpeed int64 `json:"dl_info_speed"`
		} `json:"server_state"`
	}
	decode(t, s.get(sid, "/api/v2/sync/maindata?rid=4"), &out)

	assert.Equal(t, 5, out.RID)
	assert.True(t, out.Full)
	assert.Equal(t, "downloading", out.Torrents[v.InfoHash].State)
	assert.Equal(t, "tv", out.Categories["tv"].Name)
	assert.EqualValues(t, 7, out.ServerState.DLInfoSpeed)
}

func TestTenThousandTorrentsListQuickly(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 10,000 Downloads")
	}
	t.Parallel()
	s := newStack(t)
	const n = 10_000
	for i := range n {
		s.seedBulk(i)
	}
	sid := s.login()

	start := time.Now()
	var out []torrentJSON
	rec := s.get(sid, "/api/v2/torrents/info")
	took := time.Since(start)
	decode(t, rec, &out)

	assert.Len(t, out, n)
	assert.Less(t, took, 5*time.Second, "10,000 torrents must list in a poll interval, even under -race")
	t.Logf("10,000 torrents listed in %v", took)

	start = time.Now()
	one := s.info(sid, "hashes="+out[0].Hash)
	assert.Len(t, one, 1)
	assert.Less(t, time.Since(start), time.Second, "a hash lookup does not scan everything")
}

func TestRatioAndShareLimitComeFromTheView(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	cur := s.settings.Current()
	cur.Seeding = core.SeedingSettings{RatioLimit: 3, SeedingTimeMinutes: 90}
	require.NoError(t, s.settings.Update(t.Context(), cur))
	s.seed("own ratio", ready("/d/r"), sized(1000, 1000), func(_ *core.Download, v *torrent) {
		v.Uploaded, v.OwnRatio = 500, new(1.5)
	})
	s.seed("own time", ready("/d/t"), sized(1000, 1000), func(_ *core.Download, v *torrent) {
		v.OwnSeeding = new(10 * time.Minute)
	})
	s.seed("engine size", ready("/d/e"), func(d *core.Download, v *torrent) {
		d.TotalBytes, d.DoneBytes = 4000, 4000
		v.Size, v.Uploaded = 2000, 3000
	})
	sid := s.login()

	by := map[string]torrentJSON{}
	for _, tj := range s.info(sid, "") {
		by[tj.Name] = tj
	}

	assert.InDelta(t, 0.5, by["own ratio"].Ratio, 1e-9, "uploaded over size")
	assert.Equal(t, 1.5, by["own ratio"].RatioLimit)
	assert.EqualValues(t, 90, by["own ratio"].SeedingLimit, "the global seeding time fills in the other half")
	assert.Equal(t, 3.0, by["own time"].RatioLimit, "the global ratio fills in the other half")
	assert.EqualValues(t, 10, by["own time"].SeedingLimit)
	assert.InDelta(t, 1.5, by["engine size"].Ratio, 1e-9, "over the engine's size")
	assert.EqualValues(t, 2000, by["engine size"].Size)
}

func TestAPausedDownloadHasNoEta(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("paused", inState(core.StateFetching), sized(1000, 400), func(d *core.Download, v *torrent) {
		d.Paused, v.DownRate = true, 100
	})
	sid := s.login()

	got := s.info(sid, "")

	assert.Equal(t, "pausedDL", got[0].State)
	assert.EqualValues(t, 8640000, got[0].ETA, "qBittorrent's infinity")
}

func TestTheStateFollowsTheViewOfTheStoredDownload(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("x", inState(core.StateFetching), sized(1000, 250), func(_ *core.Download, v *torrent) { v.DownRate = 10 })
	sid := s.login()
	require.Equal(t, "downloading", s.info(sid, "")[0].State)

	_, err := s.dls.Pause(t.Context(), d.ID)
	require.NoError(t, err)

	got := s.info(sid, "hashes="+v.InfoHash)
	require.Len(t, got, 1)
	assert.Equal(t, "pausedDL", got[0].State)
	assert.InDelta(t, 0.25, got[0].Progress, 1e-9)
}
