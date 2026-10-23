package sabnzbd_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

type queueResp struct {
	Queue struct {
		Version        string `json:"version"`
		Paused         bool   `json:"paused"`
		Status         string `json:"status"`
		Speed          string `json:"speed"`
		KBPerSec       string `json:"kbpersec"`
		MB             string `json:"mb"`
		MBLeft         string `json:"mbleft"`
		TimeLeft       string `json:"timeleft"`
		NoOfSlots      int    `json:"noofslots"`
		NoOfSlotsTotal int    `json:"noofslots_total"`
		Start          int    `json:"start"`
		Limit          int    `json:"limit"`
		DiskSpace1     string `json:"diskspace1"`
		Slots          []struct {
			NzoID      string `json:"nzo_id"`
			Filename   string `json:"filename"`
			Cat        string `json:"cat"`
			Priority   string `json:"priority"`
			Status     string `json:"status"`
			MB         string `json:"mb"`
			MBLeft     string `json:"mbleft"`
			Size       string `json:"size"`
			SizeLeft   string `json:"sizeleft"`
			Percentage string `json:"percentage"`
			TimeLeft   string `json:"timeleft"`
			Index      int    `json:"index"`
			Password   string `json:"password"`
		} `json:"slots"`
	} `json:"queue"`
}

func (s *stack) queue(query string) queueResp {
	s.t.Helper()
	var q queueResp
	body(s.t, s.call("mode=queue&"+query), &q)
	return q
}

func TestAnEmptyQueueHasEmptySlotsNotNull(t *testing.T) {
	s := newStack(t)

	rec := s.call("mode=queue")

	q := generic(t, rec)["queue"].(map[string]any)
	assert.Equal(t, []any{}, q["slots"])
	assert.EqualValues(t, 0, q["noofslots"])
	assert.Equal(t, "Idle", q["status"])
	assert.Equal(t, false, q["paused"])
	assert.Equal(t, "0:00:00", q["timeleft"])
}

func TestQueueSlotFields(t *testing.T) {
	s := newStack(t)
	tv := s.category("tv")
	d := s.seed("Show.S01E01", inState(core.StateFetching), sized(3<<30, 1<<30), inCategory(tv.ID),
		func(d *core.Download) { d.Priority = core.DownloadPriorityHigh })
	s.setRate(d.ID, 10<<20)   // 10 MiB/s of its own
	s.setSpeed(10<<20, false) // and 10 MiB/s for the whole queue

	q := s.queue("")

	require.Len(t, q.Queue.Slots, 1)
	slot := q.Queue.Slots[0]
	assert.Equal(t, nzo(d), slot.NzoID)
	assert.Equal(t, "Show.S01E01", slot.Filename)
	assert.Equal(t, "tv", slot.Cat)
	assert.Equal(t, "High", slot.Priority)
	assert.Equal(t, "Downloading", slot.Status)
	assert.Equal(t, "3072.00", slot.MB)
	assert.Equal(t, "2048.00", slot.MBLeft)
	assert.Equal(t, "3.0 GB", slot.Size)
	assert.Equal(t, "2.0 GB", slot.SizeLeft)
	assert.Equal(t, "33", slot.Percentage)
	assert.Equal(t, "0:03:24", slot.TimeLeft, "2048 MiB at 10 MiB/s is 204 s")
	assert.Equal(t, 0, slot.Index)
	assert.Empty(t, slot.Password, "the archive password is never shown")
	assert.Equal(t, "Downloading", q.Queue.Status)
	assert.Equal(t, "10.0 M", q.Queue.Speed)
	assert.Equal(t, "10240.00", q.Queue.KBPerSec)
	assert.Equal(t, "0:03:24", q.Queue.TimeLeft)
	assert.Equal(t, sabnzbd.DefaultVersion, q.Queue.Version)
}

// TestQueueSlotTimeLeftUsesTheDownloadsOwnRate: each slot's time left is
// its own rest at its own rate, not the queue-wide speed; a slot that is not
// downloading has none.
func TestQueueSlotTimeLeftUsesTheDownloadsOwnRate(t *testing.T) {
	s := newStack(t)
	fast := s.seed("fast", inState(core.StateFetching), sized(30<<20, 10<<20))
	slow := s.seed("slow", inState(core.StateFetching), sized(30<<20, 10<<20))
	s.seed("waiting", sized(30<<20, 0))
	s.setRate(fast.ID, 4<<20)
	s.setRate(slow.ID, 1<<20)
	s.setSpeed(5<<20, false)

	q := s.queue("")

	require.Len(t, q.Queue.Slots, 3)
	assert.Equal(t, "0:00:05", q.Queue.Slots[0].TimeLeft, "20 MiB at 4 MiB/s")
	assert.Equal(t, "0:00:20", q.Queue.Slots[1].TimeLeft, "20 MiB at 1 MiB/s")
	assert.Equal(t, "0:00:00", q.Queue.Slots[2].TimeLeft, "not downloading")
	assert.Equal(t, "0:00:14", q.Queue.TimeLeft, "the queue's 70 MiB at the global 5 MiB/s")
}

func TestQueueUncategorisedSlotsShowTheDefaultCategory(t *testing.T) {
	s := newStack(t)
	s.seed("a")

	assert.Equal(t, "*", s.queue("").Queue.Slots[0].Cat)
}

func TestQueueStatusesAndOrder(t *testing.T) {
	s := newStack(t)
	third := s.seed("third", func(d *core.Download) { d.QueuePosition = 3 })
	first := s.seed("first", inState(core.StateFetching), func(d *core.Download) { d.QueuePosition = 1 })
	second := s.seed("second", func(d *core.Download) { d.QueuePosition = 2; d.Paused = true })

	q := s.queue("")

	var ids, statuses []string
	for i, slot := range q.Queue.Slots {
		ids = append(ids, slot.NzoID)
		statuses = append(statuses, slot.Status)
		assert.Equal(t, i, slot.Index)
	}
	assert.Equal(t, []string{nzo(first), nzo(second), nzo(third)}, ids)
	assert.Equal(t, []string{"Downloading", "Paused", "Queued"}, statuses)
}

func TestQueueDoesNotListHistoryOrTorrentDownloads(t *testing.T) {
	s := newStack(t)
	s.seed("queued")
	s.seed("fetched", inState(core.StateFetched))
	s.seed("pp", inState(core.StatePostProcessing))
	s.seed("done", ready("/x"))
	s.seed("bad", failed(core.FailureInternal, ""))
	s.seed("torrent", func(d *core.Download) { d.SourceKind = core.SourceTorrent })

	q := s.queue("")

	require.Len(t, q.Queue.Slots, 1)
	assert.Equal(t, "queued", q.Queue.Slots[0].Filename)
	assert.Equal(t, 1, q.Queue.NoOfSlotsTotal)
}

func TestQueueListsADeferredVolumeRefetchAsDownloading(t *testing.T) {
	s := newStack(t)
	// M3-07: a Download in post-processing that needs more PAR2 volumes goes
	// back to Fetching, which is what the store holds.
	s.seed("refetch", inState(core.StateFetching), func(d *core.Download) { d.FetchedAt = epoch })

	q := s.queue("")

	require.Len(t, q.Queue.Slots, 1)
	assert.Equal(t, "Downloading", q.Queue.Slots[0].Status)
}

func TestQueuePagingAndCategoryFilter(t *testing.T) {
	s := newStack(t)
	tv, movies := s.category("tv"), s.category("movies")
	for i := range 7 {
		cat := tv.ID
		if i%2 == 1 {
			cat = movies.ID
		}
		s.seed(fmt.Sprintf("dl%d", i), inCategory(cat))
	}
	s.seed("plain")

	all := s.queue("")
	page := s.queue("start=2&limit=3")
	beyond := s.queue("start=50&limit=3")
	filtered := s.queue("cat=movies")
	filteredPage := s.queue("cat=tv&start=1&limit=2")
	uncategorised := s.queue("cat=*")
	unknown := s.queue("cat=nope")
	both := s.queue("cat=tv,movies")

	assert.Equal(t, 8, all.Queue.NoOfSlots)
	assert.Len(t, all.Queue.Slots, 8, "no limit means everything")
	assert.Equal(t, 8, page.Queue.NoOfSlotsTotal)
	assert.Equal(t, 8, page.Queue.NoOfSlots)
	require.Len(t, page.Queue.Slots, 3)
	assert.Equal(t, "dl2", page.Queue.Slots[0].Filename)
	assert.Equal(t, 2, page.Queue.Slots[0].Index)
	assert.Equal(t, 2, page.Queue.Start)
	assert.Equal(t, 3, page.Queue.Limit)
	assert.Empty(t, beyond.Queue.Slots)
	assert.Equal(t, 3, filtered.Queue.NoOfSlots)
	assert.Equal(t, 8, filtered.Queue.NoOfSlotsTotal)
	for _, slot := range filtered.Queue.Slots {
		assert.Equal(t, "movies", slot.Cat)
	}
	require.Len(t, filteredPage.Queue.Slots, 2)
	assert.Equal(t, "dl2", filteredPage.Queue.Slots[0].Filename)
	assert.Equal(t, 2, filteredPage.Queue.Slots[0].Index, "the index is the place in the whole queue")
	require.Len(t, uncategorised.Queue.Slots, 1)
	assert.Equal(t, "plain", uncategorised.Queue.Slots[0].Filename)
	assert.Empty(t, unknown.Queue.Slots)
	assert.Len(t, both.Queue.Slots, 7)
	for _, bad := range []string{"start=-5", "limit=-1", "start=x&limit=y"} {
		assert.Len(t, s.queue(bad).Queue.Slots, 8, bad)
	}
}

func TestQueueTotalsCoverTheWholeQueue(t *testing.T) {
	s := newStack(t)
	s.seed("a", sized(2<<20, 1<<20))
	s.seed("b", sized(4<<20, 0))

	q := s.queue("limit=1")

	assert.Equal(t, "6.00", q.Queue.MB)
	assert.Equal(t, "5.00", q.Queue.MBLeft)
	assert.Len(t, q.Queue.Slots, 1)
}

func TestQueueGlobalPauseAndSpeedComeFromTheSpeedSource(t *testing.T) {
	s := newStack(t)
	s.seed("a", inState(core.StateFetching), sized(10<<20, 0))
	s.setSpeed(0, true)

	q := s.queue("")

	assert.True(t, q.Queue.Paused)
	assert.Equal(t, "Paused", q.Queue.Status)
	assert.Equal(t, "0:00:00", q.Queue.TimeLeft)
}

func TestQueueWithoutASpeedSourceHasZeroSpeed(t *testing.T) {
	s := newStack(t, withDeps(func(d *sabnzbd.Deps) { d.SpeedSource = nil }))
	s.seed("a", inState(core.StateFetching), sized(10<<20, 0))

	q := s.queue("")

	assert.Equal(t, "0  ", q.Queue.Speed)
	assert.Equal(t, "0.00", q.Queue.KBPerSec)
	assert.Equal(t, "0:00:00", q.Queue.Slots[0].TimeLeft)
}

func TestQueueDiskSpaceFromTheOptionalFunction(t *testing.T) {
	s := newStack(t, withDeps(func(d *sabnzbd.Deps) {
		d.DiskSpace = func(p string) (int64, int64, bool) { return 3 << 30, 4 << 30, p != "" }
	}))

	assert.Equal(t, "3.00", s.queue("").Queue.DiskSpace1)
}

func TestTimeLeftFormats(t *testing.T) {
	for sec, want := range map[int64]string{
		0: "0:00:00", 59: "0:00:59", 61: "0:01:01", 3599: "0:59:59", 3600: "1:00:00",
		86399: "23:59:59", 86400: "1:00:00:00", 90061: "1:01:01:01", -5: "0:00:00",
	} {
		assert.Equal(t, want, sabnzbd.FormatTimeLeft(sec), "%d s", sec)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KB", 1536: "1.5 KB", 5 << 20: "5.0 MB", 3 << 30: "3.0 GB", 2 << 40: "2.0 TB", -1: "0 B",
	} {
		assert.Equal(t, want, sabnzbd.HumanBytes(n), "%d", n)
	}
}

func TestParseNzoID(t *testing.T) {
	id := core.NewID()
	got, ok := sabnzbd.ParseNzoID(" SABnzbd_nzo_" + string(id) + " ")
	assert.True(t, ok)
	assert.Equal(t, id, got)
	for _, bad := range []string{"", "SABnzbd_nzo_", string(id), "SABnzbd_nzo_UPPER", "SABnzbd_nzo_a/b", "SABnzbd_nzo_../x", "nzo_" + string(id)} {
		_, ok := sabnzbd.ParseNzoID(bad)
		assert.False(t, ok, bad)
	}
}

func TestQueuePriorityChangesThePriority(t *testing.T) {
	s := newStack(t)
	d := s.seed("a")
	other := s.seed("b")

	for sab, want := range map[string]core.DownloadPriority{
		"2": core.DownloadPriorityForce, "1": core.DownloadPriorityHigh, "-1": core.DownloadPriorityLow,
		"0": core.DownloadPriorityNormal, "-100": core.DownloadPriorityNormal,
	} {
		rec := s.call("mode=queue&name=priority&value=" + nzo(d) + "&value2=" + sab)
		var out struct {
			Status   bool `json:"status"`
			Position int  `json:"position"`
		}
		body(t, rec, &out)
		assert.True(t, out.Status)
		got, err := s.downloads.Get(context.Background(), d.ID)
		require.NoError(t, err)
		assert.Equal(t, want, got.Priority, "SAB priority %s", sab)
	}
	got, _ := s.downloads.Get(context.Background(), other.ID)
	assert.Equal(t, core.DownloadPriorityNormal, got.Priority, "other Downloads are left alone")
}

func TestQueuePriorityPausedPausesAndUnknownIDsAreHarmless(t *testing.T) {
	s := newStack(t)
	d := s.seed("a")

	s.call("mode=queue&name=priority&value=" + nzo(d) + "&value2=-2")
	got, _ := s.downloads.Get(context.Background(), d.ID)
	assert.True(t, got.Paused)

	unknown := s.call("mode=queue&name=priority&value=SABnzbd_nzo_" + string(core.NewID()) + "&value2=1")
	assert.JSONEq(t, `{"status":false}`, unknown.Body.String())
	noValue := s.call("mode=queue&name=priority")
	assert.Equal(t, "expect one parameter", errorOf(t, noValue))
}

func TestQueuePauseAndResumeIndividualDownloads(t *testing.T) {
	s := newStack(t)
	a, b := s.seed("a"), s.seed("b")

	paused := s.call("mode=queue&name=pause&value=" + joinNzo(a, b) + ",SABnzbd_nzo_unknown")
	assert.Equal(t, []string{nzo(a), nzo(b)}, nzoIDs(t, paused))
	q := s.queue("")
	assert.Equal(t, "Paused", q.Queue.Slots[0].Status)
	assert.Equal(t, "Paused", q.Queue.Slots[1].Status)

	resumed := s.call("mode=queue&name=resume&value=" + nzo(a))
	assert.Equal(t, []string{nzo(a)}, nzoIDs(t, resumed))
	assert.Equal(t, "Queued", s.queue("").Queue.Slots[0].Status)
}

func TestQueueSwitchMovesADownloadToAPosition(t *testing.T) {
	s := newStack(t)
	var ds []core.Download
	for i := range 5 {
		ds = append(ds, s.seed(fmt.Sprintf("d%d", i)))
	}
	names := func() []string {
		var out []string
		for _, slot := range s.queue("").Queue.Slots {
			out = append(out, slot.Filename)
		}
		return out
	}

	s.call("mode=queue&name=switch&value=" + nzo(ds[3]) + "&value2=0")
	assert.Equal(t, []string{"d3", "d0", "d1", "d2", "d4"}, names())

	s.call("mode=queue&name=switch&value=" + nzo(ds[3]) + "&value2=4")
	assert.Equal(t, []string{"d0", "d1", "d2", "d4", "d3"}, names())

	rec := s.call("mode=queue&name=switch&value=" + nzo(ds[0]) + "&value2=2")
	assert.Equal(t, []string{"d1", "d2", "d0", "d4", "d3"}, names())
	var out struct {
		Status   bool `json:"status"`
		Position int  `json:"position"`
	}
	body(t, rec, &out)
	assert.Equal(t, 2, out.Position)

	assert.Equal(t, "expect two parameters", errorOf(t, s.call("mode=queue&name=switch&value="+nzo(ds[0]))))
}

func TestQueueUnknownSubcommandIsNotImplemented(t *testing.T) {
	s := newStack(t)

	assert.Equal(t, "not implemented", errorOf(t, s.call("mode=queue&name=rename&value=x")))
}

// TestQueueAndHistoryPageWithTenThousandDownloads is a sanity check on the
// cost of a page. Both lists read every Download in their states through the
// service's keyset pages (the store has no "newest first" or offset query),
// so the cost grows with the library; this guards against it becoming
// unusable. Without -race it is about 0.2 s here; the budget leaves room for
// -race and a busy machine.
func TestQueueAndHistoryPageWithTenThousandDownloads(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 10k downloads")
	}
	s := newStack(t)
	tv := s.category("tv")
	tx, err := s.db.Writer().BeginTx(context.Background(), nil)
	require.NoError(t, err)
	stmt, err := tx.Prepare(`INSERT INTO downloads (id, name, source_kind, category_id, state, priority, queue_position,
		paused, failure_reason, failure_detail, total_bytes, done_bytes, added_at, fetched_at, ready_at, updated_at, output_path)
		VALUES (?, ?, 'nzb', ?, ?, 'normal', ?, 0, NULL, '', 0, 0, ?, ?, ?, ?, ?)`)
	require.NoError(t, err)
	for i := range 10_000 {
		at := epoch.Add(time.Duration(i) * time.Second).UnixMilli()
		state, fetched, ready, path := "queued", any(nil), any(nil), ""
		if i%2 == 0 {
			state, fetched, ready, path = "ready", at, at, "/x"
		}
		_, err := stmt.Exec(string(core.NewID()), "dl"+strconv.Itoa(i), string(tv.ID), state, i, at, fetched, ready, at, path)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	start := time.Now()
	q := s.queue("start=100&limit=50&cat=tv")
	h := s.call("mode=history&start=0&limit=50&category=tv")
	elapsed := time.Since(start)

	assert.Equal(t, 5000, q.Queue.NoOfSlots)
	assert.Len(t, q.Queue.Slots, 50)
	assert.Equal(t, "dl201", q.Queue.Slots[0].Filename)
	assert.Contains(t, h.Body.String(), `"noofslots":5000`)
	t.Logf("queue+history page over 10k downloads: %v", elapsed)
	assert.Less(t, elapsed, 30*time.Second)
}
